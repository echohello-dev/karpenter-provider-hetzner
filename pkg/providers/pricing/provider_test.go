package pricing

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
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
	if !floatNear(got, want, 1e-9) {
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
	if !floatNear(got, 0.005, 1e-9) {
		t.Fatalf("Price(cx22) = %v, want 0.005 (server-only)", got)
	}
}

func TestPrice_CachesAcrossCalls(t *testing.T) {
	// One Price call must trigger exactly one /pricing fetch. A second call
	// must reuse the cached snapshot, even if the server would otherwise
	// return an error.
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
	// If the first fetch fails, every subsequent Price call must surface
	// the same error without retrying. The server here returns 500 every
	// time; the client must NOT loop, but it may also not succeed.
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

	// Second call must reuse the sticky error without re-fetching.
	_, err2 := p.Price(context.Background(), st("cx22"))
	if err2 == nil {
		t.Fatal("expected sticky error on second call, got nil")
	}
	if err2.Error() != err.Error() {
		t.Fatalf("second error differs from first: %q vs %q", err2.Error(), err.Error())
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
		if !tc.wantErr && !floatNear(got, tc.want, 1e-9) {
			t.Errorf("parsePrice(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// floatNear returns true if a and b are within eps of each other.
func floatNear(a, b, eps float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d < eps
}
