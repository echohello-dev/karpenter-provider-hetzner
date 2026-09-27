package pricing

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hetznercloud/hcloud-go/v2/hcloud"
	"github.com/hetznercloud/hcloud-go/v2/hcloud/schema"
)

// newPricingServer returns an httptest server that answers GET /pricing with
// the supplied pricing payload, plus an hcloud.Client wired to talk to it.
// The pricing argument may be partial — the test passes only the fields it
// cares about and leaves the rest at the zero value.
func newPricingServer(t *testing.T, pricing schema.Pricing) (*httptest.Server, *hcloud.Client) {
	t.Helper()

	mux := http.NewServeMux()
	mux.HandleFunc("/pricing", func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewEncoder(w).Encode(schema.PricingGetResponse{Pricing: pricing}); err != nil {
			t.Fatalf("encode pricing: %v", err)
		}
	})
	srv := httptest.NewServer(mux)

	client := hcloud.NewClient(
		hcloud.WithEndpoint(srv.URL),
		hcloud.WithToken("token"),
		hcloud.WithRetryOpts(hcloud.RetryOpts{
			BackoffFunc: hcloud.ConstantBackoff(time.Millisecond),
			MaxRetries:  1,
		}),
	)
	return srv, client
}

// st constructs an hcloud.ServerType with just the name set — that's all
// the pricing lookup needs.
func st(name string) *hcloud.ServerType {
	return &hcloud.ServerType{Name: name}
}

func TestPrice_NilServerTypeRejected(t *testing.T) {
	p := New(nil)
	_, err := p.Price(context.Background(), nil)
	if err == nil {
		t.Fatal("expected error for nil serverType, got nil")
	}
	if !strings.Contains(err.Error(), "serverType is nil") {
		t.Fatalf("error should mention nil serverType, got %q", err.Error())
	}
}

func TestPrice_NilHcloudClientFailsCleanly(t *testing.T) {
	// nil hcloud client surfaces as a fetch error rather than a panic.
	p := New(nil)
	_, err := p.Price(context.Background(), st("cx22"))
	if err == nil {
		t.Fatal("expected error when hcloud client is nil, got nil")
	}
	if !strings.Contains(err.Error(), "hcloud client is nil") {
		t.Fatalf("error should mention nil hcloud client, got %q", err.Error())
	}
}

func TestPrice_ServerTypeNotInCatalog(t *testing.T) {
	srv, client := newPricingServer(t, schema.Pricing{
		Currency: "EUR",
		ServerTypes: []schema.PricingServerType{
			{
				Name: "cx22",
				Prices: []schema.PricingServerTypePrice{
					{Location: "fsn1", PriceHourly: schema.Price{Net: "3.49", Gross: "4.1531"}},
				},
			},
		},
	})
	defer srv.Close()

	p := New(client)
	_, err := p.Price(context.Background(), st("cx999"))
	if err == nil {
		t.Fatal("expected error for unknown server type, got nil")
	}
	if !strings.Contains(err.Error(), `server type "cx999" not in pricing catalog`) {
		t.Fatalf("error should name the missing type, got %q", err.Error())
	}
}

func TestPrice_SumServerHourlyAndIPv4Surcharge(t *testing.T) {
	// Mirror a realistic Hetzner snapshot: cx22 at €3.49/mo net (€0.00483/hr)
	// is a simplified case — actual values are 3.49/mo = 0.004847.../hr, and
	// IPv4 primary is €0.0040/hr. We use round numbers here so the test is
	// independent of Hetzner's price changes.
	//
	// FloatingIPs is included with a matching count to work around an
	// upstream bug in primaryIPPricingFromSchema that allocates the
	// PrimaryIPs slice via `len(s.FloatingIPs)`. Hetzner's real /pricing
	// response includes one floating_ips entry per primary_ips entry, so a
	// stub here matches the production shape.
	srv, client := newPricingServer(t, schema.Pricing{
		Currency: "EUR",
		ServerTypes: []schema.PricingServerType{
			{
				Name: "cx22",
				Prices: []schema.PricingServerTypePrice{
					{
						Location:    "fsn1",
						PriceHourly: schema.Price{Net: "0.005000", Gross: "0.005950"},
					},
					{
						Location:    "nbg1",
						PriceHourly: schema.Price{Net: "0.005000", Gross: "0.005950"},
					},
				},
			},
		},
		FloatingIPs: []schema.PricingFloatingIPType{
			{Type: "ipv4"},
		},
		PrimaryIPs: []schema.PricingPrimaryIP{
			{
				Type: "ipv4",
				Prices: []schema.PricingPrimaryIPTypePrice{
					{
						Location:    "fsn1",
						PriceHourly: schema.Price{Net: "0.001000", Gross: "0.001190"},
					},
				},
			},
		},
	})
	defer srv.Close()

	p := New(client)
	got, err := p.Price(context.Background(), st("cx22"))
	if err != nil {
		t.Fatalf("Price(cx22): unexpected error: %v", err)
	}
	want := 0.006000
	if !floatNear(got, want) {
		t.Fatalf("Price(cx22) = %v, want %v", got, want)
	}
}

func TestPrice_MissingIPv4FallsBackToServerOnly(t *testing.T) {
	// No PrimaryIPs entry at all — Price must still return the server rate,
	// not fail the whole lookup. Operators see the gap in logs.
	srv, client := newPricingServer(t, schema.Pricing{
		Currency: "EUR",
		ServerTypes: []schema.PricingServerType{
			{
				Name: "cx22",
				Prices: []schema.PricingServerTypePrice{
					{Location: "fsn1", PriceHourly: schema.Price{Net: "0.005", Gross: "0.006"}},
				},
			},
		},
	})
	defer srv.Close()

	p := New(client)
	got, err := p.Price(context.Background(), st("cx22"))
	if err != nil {
		t.Fatalf("Price(cx22): unexpected error: %v", err)
	}
	if !floatNear(got, 0.005) {
		t.Fatalf("Price(cx22) = %v, want 0.005 (server-only)", got)
	}
}

func TestPrice_CachesAcrossCalls(t *testing.T) {
	// One Price call must trigger exactly one /pricing fetch. Further calls
	// reuse the cached snapshot for the whole TTL.
	var calls int
	mux := http.NewServeMux()
	mux.HandleFunc("/pricing", func(w http.ResponseWriter, r *http.Request) {
		calls++
		_ = json.NewEncoder(w).Encode(schema.PricingGetResponse{
			Pricing: schema.Pricing{
				Currency: "EUR",
				ServerTypes: []schema.PricingServerType{
					{
						Name: "cx22",
						Prices: []schema.PricingServerTypePrice{
							{Location: "fsn1", PriceHourly: schema.Price{Net: "0.005"}},
						},
					},
				},
			},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	client := hcloud.NewClient(
		hcloud.WithEndpoint(srv.URL),
		hcloud.WithToken("token"),
	)
	p := New(client)
	for i := 0; i < 3; i++ {
		if _, err := p.Price(context.Background(), st("cx22")); err != nil {
			t.Fatalf("Price call %d: %v", i, err)
		}
	}
	if calls != 1 {
		t.Fatalf("expected exactly 1 /pricing fetch across 3 calls, got %d", calls)
	}
}

func TestPrice_FetchErrorIsSticky(t *testing.T) {
	// While the retry backoff is running, every Price call must surface the
	// same error without re-fetching. The server here returns 500 every
	// time; the client must NOT loop inside a single call, but it may also
	// not succeed. Recovery once the backoff elapses is covered by
	// TestPrice_FailedFetchIsRetriedAfterBackoff.
	mux := http.NewServeMux()
	mux.HandleFunc("/pricing", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	client := hcloud.NewClient(
		hcloud.WithEndpoint(srv.URL),
		hcloud.WithToken("token"),
		hcloud.WithRetryOpts(hcloud.RetryOpts{
			BackoffFunc: hcloud.ConstantBackoff(time.Millisecond),
			MaxRetries:  0,
		}),
	)
	p := New(client)

	_, err := p.Price(context.Background(), st("cx22"))
	if err == nil {
		t.Fatal("expected error on 500 response, got nil")
	}
	if !strings.Contains(err.Error(), "hcloud Pricing.Get") {
		t.Fatalf("error should mention the underlying fetch, got %q", err.Error())
	}

	// Second call reuses the sticky error without re-fetching: it lands
	// inside the retry backoff, so no second request is issued.
	_, err2 := p.Price(context.Background(), st("cx22"))
	if err2 == nil {
		t.Fatal("expected sticky error on second call, got nil")
	}
	if err2.Error() != err.Error() {
		t.Fatalf("second error differs from first: %q vs %q", err2.Error(), err.Error())
	}
}

// TestPrice_FailedFetchIsRetriedAfterBackoff proves a bad first fetch does
// not poison the provider for the lifetime of the process: the error is
// reused (without extra API calls) while the backoff is running, then the
// next attempt after the backoff recovers.
func TestPrice_FailedFetchIsRetriedAfterBackoff(t *testing.T) {
	var calls atomic.Int64
	var failing atomic.Bool
	failing.Store(true)

	mux := http.NewServeMux()
	mux.HandleFunc("/pricing", func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if failing.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(schema.PricingGetResponse{Pricing: pricingCatalog("0.005000")})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	client := hcloud.NewClient(
		hcloud.WithEndpoint(srv.URL),
		hcloud.WithToken("token"),
		hcloud.WithRetryOpts(hcloud.RetryOpts{MaxRetries: 0}),
	)

	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	p := New(client,
		WithClock(func() time.Time { return now }),
		WithTTL(time.Hour),
		WithRetryBackoff(time.Minute),
	)

	if _, err := p.Price(context.Background(), st("cx22")); err == nil {
		t.Fatal("expected error while the pricing endpoint is failing, got nil")
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("expected 1 fetch attempt, got %d", got)
	}

	// Inside the backoff the cached outcome is reused: same error, no request.
	now = now.Add(10 * time.Second)
	_, err := p.Price(context.Background(), st("cx22"))
	if err == nil {
		t.Fatal("expected the same failure while the backoff is running, got nil")
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("expected no fetch inside the retry backoff, got %d attempts", got)
	}

	// Backoff elapsed and the endpoint recovered: pricing works again.
	failing.Store(false)
	now = now.Add(time.Minute)
	got, err := p.Price(context.Background(), st("cx22"))
	if err != nil {
		t.Fatalf("Price after backoff: %v", err)
	}
	if !floatNear(got, 0.006) {
		t.Fatalf("Price = %v, want 0.006", got)
	}
	if attempts := calls.Load(); attempts != 2 {
		t.Fatalf("expected exactly 2 fetch attempts total, got %d", attempts)
	}
}

// TestPrice_StaleCatalogSurvivesRefreshFailure checks the refresh policy: a
// catalog that has aged out keeps serving when the refresh fails (stale
// prices beat no prices, which would mark every offering unavailable), and
// the next healthy attempt picks up the new prices.
func TestPrice_StaleCatalogSurvivesRefreshFailure(t *testing.T) {
	var calls atomic.Int64
	var failing atomic.Bool
	var hourly atomic.Value
	hourly.Store("0.005000")

	mux := http.NewServeMux()
	mux.HandleFunc("/pricing", func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if failing.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(schema.PricingGetResponse{Pricing: pricingCatalog(hourly.Load().(string))})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	client := hcloud.NewClient(
		hcloud.WithEndpoint(srv.URL),
		hcloud.WithToken("token"),
		hcloud.WithRetryOpts(hcloud.RetryOpts{MaxRetries: 0}),
	)

	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	p := New(client,
		WithClock(func() time.Time { return now }),
		WithTTL(time.Hour),
		WithRetryBackoff(time.Minute),
	)

	if _, err := p.Price(context.Background(), st("cx22")); err != nil {
		t.Fatalf("initial Price: %v", err)
	}

	// Catalog ages out and the refresh fails: the cached price is returned
	// with no error, so offerings stay schedulable.
	failing.Store(true)
	now = now.Add(2 * time.Hour)
	got, err := p.Price(context.Background(), st("cx22"))
	if err != nil {
		t.Fatalf("Price on failed refresh: %v", err)
	}
	if !floatNear(got, 0.006) {
		t.Fatalf("Price = %v, want the stale 0.006", got)
	}

	// Endpoint recovers: the next attempt past the backoff refreshes the
	// catalog, which now carries the new hourly rate plus the IPv4 charge.
	failing.Store(false)
	hourly.Store("0.007000")
	now = now.Add(2 * time.Minute)
	got, err = p.Price(context.Background(), st("cx22"))
	if err != nil {
		t.Fatalf("Price after recovery: %v", err)
	}
	if !floatNear(got, 0.008) {
		t.Fatalf("Price = %v, want the refreshed 0.008", got)
	}
	if attempts := calls.Load(); attempts != 3 {
		t.Fatalf("expected 3 fetch attempts (ok, fail, ok), got %d", attempts)
	}
}

// pricingCatalog builds a /pricing payload whose cx22 hourly net is the
// given string. The IPv4 primary-IP rate is fixed at 0.001 so tests can
// reason about the total, and FloatingIPs mirrors PrimaryIPs to work around
// the upstream primaryIPPricingFromSchema bug documented above.
func pricingCatalog(hourlyNet string) schema.Pricing {
	return schema.Pricing{
		Currency: "EUR",
		ServerTypes: []schema.PricingServerType{
			{
				Name: "cx22",
				Prices: []schema.PricingServerTypePrice{
					{Location: "fsn1", PriceHourly: schema.Price{Net: hourlyNet, Gross: hourlyNet}},
				},
			},
		},
		FloatingIPs: []schema.PricingFloatingIPType{
			{Type: "ipv4"},
		},
		PrimaryIPs: []schema.PricingPrimaryIP{
			{
				Type: "ipv4",
				Prices: []schema.PricingPrimaryIPTypePrice{
					{Location: "fsn1", PriceHourly: schema.Price{Net: "0.001", Gross: "0.001"}},
				},
			},
		},
	}
}

func TestParsePrice(t *testing.T) {
	cases := []struct {
		in      string
		want    float64
		wantErr bool
	}{
		{"3.490000", 3.49, false},
		{"0", 0, false},
		{"0.005", 0.005, false},
		{"", 0, true},
		{"not-a-number", 0, true},
	}
	for _, tc := range cases {
		got, err := parsePrice(tc.in)
		if (err != nil) != tc.wantErr {
			t.Errorf("parsePrice(%q) err = %v, wantErr %v", tc.in, err, tc.wantErr)
		}
		if !tc.wantErr && !floatNear(got, tc.want) {
			t.Errorf("parsePrice(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// floatNear returns true if a and b agree to within 1e-9, which is tight
// enough for the sub-cent hourly rates these fixtures use.
func floatNear(a, b float64) bool {
	const eps = 1e-9
	d := a - b
	if d < 0 {
		d = -d
	}
	return d < eps
}
