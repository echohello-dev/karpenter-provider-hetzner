package instancetype

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hetznercloud/hcloud-go/v2/hcloud"
	"github.com/hetznercloud/hcloud-go/v2/hcloud/schema"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"

	"github.com/echohello-dev/karpenter-provider-hetzner/v1/pkg/providers/pricing"
)

// fakeClock is a deterministic clock for cooldown and TTL tests. Real
// time.Sleep would make this suite slow and unreliable on shared CI.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock(t time.Time) *fakeClock { return &fakeClock{now: t} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// testHarness wires an httptest server that serves both /server_types
// (consumed by the instancetype provider's hcloud client) and /pricing
// (consumed by the pricing provider's hcloud client). The pricing
// provider in main fetches its catalog lazily and sticks to it, so
// both providers can talk to the same server safely.
type testHarness struct {
	server  *httptest.Server
	mux     *http.ServeMux
	client  *hcloud.Client
	pricing *pricing.Provider

	mu              sync.Mutex
	registeredTypes []schema.ServerType

	serverTypes atomic.Int64
	pricingHits atomic.Int64
}

func newHarness(t *testing.T) *testHarness {
	t.Helper()
	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	client := hcloud.NewClient(
		hcloud.WithEndpoint(server.URL),
		hcloud.WithToken("test-token"),
		hcloud.WithRetryOpts(hcloud.RetryOpts{BackoffFunc: hcloud.ConstantBackoff(time.Millisecond), MaxRetries: 1}),
		hcloud.WithPollOpts(hcloud.PollOpts{BackoffFunc: hcloud.ConstantBackoff(time.Millisecond)}),
	)
	t.Cleanup(server.Close)

	// Pricing is location-uniform: a single /pricing response is enough
	// for every offering. We use the same hcloud client so the
	// pricing fetch shares the mux.
	pricer := pricing.New(client)
	return &testHarness{server: server, mux: mux, client: client, pricing: pricer}
}

// handleServerTypes serves a list of hcloud server types on
// /server_types. Each entry can carry a per-location availability
// map; locations not present are treated as "type not offered there"
// by the Provider. The list is also retained for the /pricing handler
// so the pricing provider's lookup-by-name finds each server type.
func (h *testHarness) handleServerTypes(t *testing.T, types []schema.ServerType) {
	t.Helper()
	h.mu.Lock()
	h.registeredTypes = types
	h.mu.Unlock()
	h.mux.HandleFunc("/server_types", func(w http.ResponseWriter, r *http.Request) {
		h.serverTypes.Add(1)
		_ = json.NewEncoder(w).Encode(schema.ServerTypeListResponse{ServerTypes: types})
	})
}

// handlePricing serves a /pricing payload covering the server types
// previously registered via handleServerTypes, plus a primary-IPv4
// surcharge of ipv4HourlyNet. The pricing provider in main returns a
// single per-server-type figure (sum of server hourly + IPv4 hourly),
// so the IPv4 surcharge is what makes the offering price meaningful.
//
// pricingHits is bumped on every call so tests can verify the
// one-shot fetch caching of the pricing provider.
func (h *testHarness) handlePricing(t *testing.T, ipv4HourlyNet string) {
	t.Helper()
	h.mu.Lock()
	src := h.registeredTypes
	h.mu.Unlock()

	serverTypeEntries := make([]schema.PricingServerType, 0, len(src))
	for _, st := range src {
		if st.Name == "" {
			continue
		}
		prices := st.Prices
		if len(prices) == 0 {
			continue
		}
		// Re-shape each schema.ServerType's per-location prices into
		// the schema.PricingServerType form /pricing returns. The
		// schema is the same wire format; we just renarrow the type.
		out := make([]schema.PricingServerTypePrice, 0, len(prices))
		for _, p := range prices {
			out = append(out, schema.PricingServerTypePrice{
				Location:    p.Location,
				PriceHourly: p.PriceHourly,
			})
		}
		serverTypeEntries = append(serverTypeEntries, schema.PricingServerType{
			Name:   st.Name,
			Prices: out,
		})
	}

	ipv4 := []schema.PricingPrimaryIPTypePrice{
		{Location: "fsn1", PriceHourly: schema.Price{Net: ipv4HourlyNet, Gross: ipv4HourlyNet}},
	}
	h.mux.HandleFunc("/pricing", func(w http.ResponseWriter, r *http.Request) {
		h.pricingHits.Add(1)
		_ = json.NewEncoder(w).Encode(schema.PricingGetResponse{
			Pricing: schema.Pricing{
				Currency: "EUR",
				FloatingIPs: []schema.PricingFloatingIPType{
					{Type: "ipv4"},
				},
				PrimaryIPs: []schema.PricingPrimaryIP{
					{Type: "ipv4", Prices: ipv4},
				},
				ServerTypes: serverTypeEntries,
			},
		})
	})
}

// stTypeWithLocations builds a schema.ServerType with the supplied
// name, architecture, and (location, available) map. The hourly net
// price for the type is the value Karpenter compares across offerings.
func stTypeWithLocations(name string, arch hcloud.Architecture, perLoc map[string]bool, hourlyNet string) schema.ServerType {
	locs := make([]schema.ServerTypeLocation, 0, len(perLoc))
	prices := make([]schema.PricingServerTypePrice, 0, len(perLoc))
	for loc, avail := range perLoc {
		locs = append(locs, schema.ServerTypeLocation{Name: loc, Available: avail})
		prices = append(prices, schema.PricingServerTypePrice{
			Location:    loc,
			PriceHourly: schema.Price{Net: hourlyNet, Gross: hourlyNet},
		})
	}
	return schema.ServerType{
		Name:         name,
		Architecture: string(arch),
		Cores:        2,
		Memory:       4,
		Disk:         40,
		StorageType:  "local",
		CPUType:      "shared",
		Prices:       prices,
		Locations:    locs,
	}
}

// findOffering returns the Offering that matches zone and
// capacity-type (only one per type in these tests, but the API
// signature is what matters).
func findOffering(t *testing.T, it *cloudprovider.InstanceType, zone string) *cloudprovider.Offering {
	t.Helper()
	for _, o := range it.Offerings {
		if o == nil {
			continue
		}
		if z := o.Requirements.Get(corev1.LabelTopologyZone).Any(); z == zone {
			return o
		}
	}
	t.Fatalf("no offering for zone %q on type %q; offerings: %+v", zone, it.Name, it.Offerings)
	return nil
}

// reqHas reports whether the InstanceType.Requirements has the given
// key with at least one of the supplied values.
func reqHas(it *cloudprovider.InstanceType, key string, want ...string) bool {
	r := it.Requirements.Get(key)
	for _, w := range want {
		if r.Has(w) {
			return true
		}
	}
	return false
}

func TestList_OneInstanceTypePerServerType(t *testing.T) {
	h := newHarness(t)
	h.handleServerTypes(t, []schema.ServerType{
		stTypeWithLocations("cx22", hcloud.ArchitectureX86, map[string]bool{"fsn1": true}, "0.01"),
		stTypeWithLocations("cax21", hcloud.ArchitectureARM, map[string]bool{"fsn1": true}, "0.015"),
	})
	h.handlePricing(t, "0.004")

	p := New(h.client, h.pricing)

	its, err := p.List(context.Background(), []string{"fsn1"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(its) != 2 {
		t.Fatalf("List returned %d types, want 2", len(its))
	}
	names := []string{its[0].Name, its[1].Name}
	sort.Strings(names)
	if names[0] != "cax21" || names[1] != "cx22" {
		t.Fatalf("unexpected instance type names: %v", names)
	}
}

func TestList_HasArchitectureRequirement(t *testing.T) {
	h := newHarness(t)
	h.handleServerTypes(t, []schema.ServerType{
		stTypeWithLocations("cx22", hcloud.ArchitectureX86, map[string]bool{"fsn1": true}, "0.01"),
		stTypeWithLocations("cax21", hcloud.ArchitectureARM, map[string]bool{"fsn1": true}, "0.015"),
	})
	h.handlePricing(t, "0.004")
	p := New(h.client, h.pricing)

	its, err := p.List(context.Background(), []string{"fsn1"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, it := range its {
		switch it.Name {
		case "cx22":
			if !reqHas(it, corev1.LabelArchStable, "amd64") {
				t.Fatalf("cx22 missing amd64 arch requirement, got %v", it.Requirements.Get(corev1.LabelArchStable).Values())
			}
		case "cax21":
			if !reqHas(it, corev1.LabelArchStable, "arm64") {
				t.Fatalf("cax21 missing arm64 arch requirement, got %v", it.Requirements.Get(corev1.LabelArchStable).Values())
			}
		}
	}
}

func TestList_HasHostnameAndFamilyRequirements(t *testing.T) {
	h := newHarness(t)
	h.handleServerTypes(t, []schema.ServerType{
		stTypeWithLocations("cx22", hcloud.ArchitectureX86, map[string]bool{"fsn1": true}, "0.01"),
		stTypeWithLocations("cax21", hcloud.ArchitectureARM, map[string]bool{"fsn1": true}, "0.015"),
	})
	h.handlePricing(t, "0.004")
	p := New(h.client, h.pricing)

	its, err := p.List(context.Background(), []string{"fsn1"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, it := range its {
		// instance-type = name
		if !reqHas(it, corev1.LabelInstanceTypeStable, it.Name) {
			t.Fatalf("%s: instance-type requirement missing its own name", it.Name)
		}
		// family = ClassOf result
		var wantFamily string
		switch it.Name {
		case "cx22":
			wantFamily = "cx"
		case "cax21":
			wantFamily = "cax"
		}
		if !reqHas(it, LabelServerFamily, wantFamily) {
			t.Fatalf("%s: family requirement missing %q, got %v", it.Name, wantFamily, it.Requirements.Get(LabelServerFamily).Values())
		}
	}
}

func TestList_OnDemandCapacityTypeRequirement(t *testing.T) {
	h := newHarness(t)
	h.handleServerTypes(t, []schema.ServerType{
		stTypeWithLocations("cx22", hcloud.ArchitectureX86, map[string]bool{"fsn1": true}, "0.01"),
	})
	h.handlePricing(t, "0.004")
	p := New(h.client, h.pricing)

	its, err := p.List(context.Background(), []string{"fsn1"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(its) != 1 {
		t.Fatalf("expected 1 instance type, got %d", len(its))
	}
	if !reqHas(its[0], karpv1.CapacityTypeLabelKey, karpv1.CapacityTypeOnDemand) {
		t.Fatalf("missing on-demand capacity type requirement, got %v", its[0].Requirements.Get(karpv1.CapacityTypeLabelKey).Values())
	}
	if reqHas(its[0], karpv1.CapacityTypeLabelKey, karpv1.CapacityTypeSpot) {
		t.Fatal("Hetzner has no spot market; spot must not be offered")
	}
}

func TestList_Resources(t *testing.T) {
	h := newHarness(t)
	h.handleServerTypes(t, []schema.ServerType{
		{
			Name:         "cx32",
			Architecture: string(hcloud.ArchitectureX86),
			Cores:        4,
			Memory:       8,  // GB
			Disk:         80, // GB
			StorageType:  "local",
			CPUType:      "shared",
			Locations:    []schema.ServerTypeLocation{{Name: "fsn1", Available: true}},
			Prices:       []schema.PricingServerTypePrice{{Location: "fsn1", PriceHourly: schema.Price{Net: "0.02", Gross: "0.02"}}},
		},
	})
	h.handlePricing(t, "0.004")
	p := New(h.client, h.pricing)

	its, err := p.List(context.Background(), []string{"fsn1"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(its) != 1 {
		t.Fatalf("expected 1 instance type, got %d", len(its))
	}
	cap := its[0].Capacity

	cpu := cap[corev1.ResourceCPU]
	if cpu.CmpInt64(4) != 0 {
		t.Fatalf("CPU = %v, want 4", cpu)
	}
	mem := cap[corev1.ResourceMemory]
	// 8 GB in BinarySI bytes = 8 * 1024^3.
	if mem.Value() != 8*1024*1024*1024 {
		t.Fatalf("Memory bytes = %d, want %d", mem.Value(), 8*1024*1024*1024)
	}
	if mem.Format != resource.BinarySI {
		t.Fatalf("Memory format = %v, want BinarySI", mem.Format)
	}
	disk := cap[corev1.ResourceEphemeralStorage]
	if disk.Value() != 80*1000*1000*1000 {
		t.Fatalf("EphemeralStorage bytes = %d, want %d", disk.Value(), int64(80*1000*1000*1000))
	}
	pods := cap[corev1.ResourcePods]
	if pods.CmpInt64(110) != 0 {
		t.Fatalf("Pods = %v, want 110", pods)
	}
}

func TestList_OneOfferingPerRequestedLocation(t *testing.T) {
	h := newHarness(t)
	h.handleServerTypes(t, []schema.ServerType{
		stTypeWithLocations("cx22", hcloud.ArchitectureX86, map[string]bool{"fsn1": true, "nbg1": true}, "0.01"),
	})
	h.handlePricing(t, "0.004")
	p := New(h.client, h.pricing)

	its, err := p.List(context.Background(), []string{"fsn1", "nbg1"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(its) != 1 {
		t.Fatalf("expected 1 instance type, got %d", len(its))
	}
	if len(its[0].Offerings) != 2 {
		t.Fatalf("expected 2 offerings (one per location), got %d", len(its[0].Offerings))
	}
	zones := make([]string, 0, 2)
	for _, o := range its[0].Offerings {
		zones = append(zones, o.Requirements.Get(corev1.LabelTopologyZone).Any())
	}
	sort.Strings(zones)
	if zones[0] != "fsn1" || zones[1] != "nbg1" {
		t.Fatalf("offering zones = %v, want [fsn1 nbg1]", zones)
	}
}

func TestList_OfferingPriceIsLocationUniform(t *testing.T) {
	// Pricing is location-uniform: every offering for a server type
	// shares the same hourly figure, regardless of zone. The figure
	// already includes the primary-IPv4 surcharge (see the pricing
	// provider).
	h := newHarness(t)
	h.handleServerTypes(t, []schema.ServerType{
		stTypeWithLocations("cx22", hcloud.ArchitectureX86, map[string]bool{"fsn1": true, "nbg1": true}, "0.01"),
	})
	h.handlePricing(t, "0.005")
	p := New(h.client, h.pricing)

	its, err := p.List(context.Background(), []string{"fsn1", "nbg1"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(its) != 1 {
		t.Fatalf("expected 1 instance type, got %d", len(its))
	}
	want := 0.01 + 0.005
	for _, o := range its[0].Offerings {
		if !approxEqual(o.Price, want, 1e-9) {
			t.Fatalf("offering %s price = %.9f, want %.9f", o.Requirements.Get(corev1.LabelTopologyZone).Any(), o.Price, want)
		}
	}
}

func TestList_MarkUnavailableMarksOfferingUnavailable(t *testing.T) {
	h := newHarness(t)
	h.handleServerTypes(t, []schema.ServerType{
		stTypeWithLocations("cx22", hcloud.ArchitectureX86, map[string]bool{"fsn1": true, "nbg1": true}, "0.01"),
	})
	h.handlePricing(t, "0.004")

	clk := newFakeClock(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))
	p := New(h.client, h.pricing, WithClock(clk.Now), WithCooldown(5*time.Minute))

	its, err := p.List(context.Background(), []string{"fsn1", "nbg1"})
	if err != nil {
		t.Fatalf("first List: %v", err)
	}
	if !findOffering(t, its[0], "fsn1").Available {
		t.Fatal("fsn1 offering should be available before MarkUnavailable")
	}

	p.MarkUnavailable("cx22", "fsn1")
	its, err = p.List(context.Background(), []string{"fsn1", "nbg1"})
	if err != nil {
		t.Fatalf("second List: %v", err)
	}
	fsn1 := findOffering(t, its[0], "fsn1")
	nbg1 := findOffering(t, its[0], "nbg1")
	if fsn1.Available {
		t.Fatal("fsn1 offering should be unavailable after MarkUnavailable")
	}
	if !nbg1.Available {
		t.Fatal("nbg1 offering should remain available")
	}
}

func TestList_MarkUnavailableExpiresAfterCooldown(t *testing.T) {
	h := newHarness(t)
	h.handleServerTypes(t, []schema.ServerType{
		stTypeWithLocations("cx22", hcloud.ArchitectureX86, map[string]bool{"fsn1": true}, "0.01"),
	})
	h.handlePricing(t, "0.004")

	clk := newFakeClock(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))
	p := New(h.client, h.pricing, WithClock(clk.Now), WithCooldown(time.Minute))

	p.MarkUnavailable("cx22", "fsn1")

	its, err := p.List(context.Background(), []string{"fsn1"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if findOffering(t, its[0], "fsn1").Available {
		t.Fatal("fsn1 should be unavailable immediately after MarkUnavailable")
	}

	clk.Advance(30 * time.Second) // half of cooldown
	its, _ = p.List(context.Background(), []string{"fsn1"})
	if findOffering(t, its[0], "fsn1").Available {
		t.Fatal("fsn1 should still be unavailable at +30s")
	}

	clk.Advance(31 * time.Second) // total +61s > 1m
	its, _ = p.List(context.Background(), []string{"fsn1"})
	if !findOffering(t, its[0], "fsn1").Available {
		t.Fatal("fsn1 should be available again after cooldown elapses")
	}
}

func TestList_PreservesTypeWhenNoOfferingsAvailable(t *testing.T) {
	h := newHarness(t)
	// A type that Hetzner reports as not offered at the only requested
	// location must still appear in the list (with the offering
	// marked unavailable) so NodePool templates that reference it
	// keep validating.
	h.handleServerTypes(t, []schema.ServerType{
		stTypeWithLocations("ccx13", hcloud.ArchitectureX86, map[string]bool{"fsn1": false}, "0.05"),
	})
	h.handlePricing(t, "0.004")
	p := New(h.client, h.pricing)

	its, err := p.List(context.Background(), []string{"fsn1"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(its) != 1 {
		t.Fatalf("expected ccx13 to be preserved, got %d types", len(its))
	}
	if its[0].Name != "ccx13" {
		t.Fatalf("unexpected type: %s", its[0].Name)
	}
	if findOffering(t, its[0], "fsn1").Available {
		t.Fatal("ccx13 at fsn1 should be unavailable per Hetzner")
	}
}

func TestList_PreservesTypeWhenAllOfferingsOnCooldown(t *testing.T) {
	h := newHarness(t)
	h.handleServerTypes(t, []schema.ServerType{
		stTypeWithLocations("cx22", hcloud.ArchitectureX86, map[string]bool{"fsn1": true, "nbg1": true}, "0.01"),
	})
	h.handlePricing(t, "0.004")

	clk := newFakeClock(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))
	p := New(h.client, h.pricing, WithClock(clk.Now), WithCooldown(time.Minute))

	p.MarkUnavailable("cx22", "fsn1")
	p.MarkUnavailable("cx22", "nbg1")

	its, err := p.List(context.Background(), []string{"fsn1", "nbg1"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(its) != 1 {
		t.Fatalf("expected cx22 to be preserved with no available offerings, got %d types", len(its))
	}
	if len(its[0].Offerings.Available()) != 0 {
		t.Fatalf("expected no available offerings, got %+v", its[0].Offerings.Available())
	}
}

func TestList_EmptyLocationsIsError(t *testing.T) {
	h := newHarness(t)
	h.handleServerTypes(t, nil)
	h.handlePricing(t, "0.004")
	p := New(h.client, h.pricing)
	if _, err := p.List(context.Background(), nil); err == nil {
		t.Fatal("expected error for nil locations, got nil")
	}
	if _, err := p.List(context.Background(), []string{""}); err == nil {
		t.Fatal("expected error for empty-string location, got nil")
	}
}

func TestList_NormalizesLocationCacheKey(t *testing.T) {
	h := newHarness(t)
	h.handleServerTypes(t, []schema.ServerType{
		stTypeWithLocations("cx22", hcloud.ArchitectureX86, map[string]bool{"fsn1": true}, "0.01"),
	})
	h.handlePricing(t, "0.004")

	clk := newFakeClock(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))
	p := New(h.client, h.pricing, WithClock(clk.Now), WithTTL(time.Hour))

	// First call populates the cache; the second call uses a
	// differently-cased duplicate and should hit the cache.
	if _, err := p.List(context.Background(), []string{"fsn1", "FSN1"}); err != nil {
		t.Fatalf("first: %v", err)
	}
	if _, err := p.List(context.Background(), []string{" fsn1 "}); err != nil {
		t.Fatalf("second: %v", err)
	}
	if got := h.serverTypes.Load(); got != 1 {
		t.Fatalf("expected 1 server_types fetch under cache, got %d", got)
	}
}

func TestList_ReusesCatalogAcrossLocationSets(t *testing.T) {
	h := newHarness(t)
	h.handleServerTypes(t, []schema.ServerType{
		stTypeWithLocations("cx22", hcloud.ArchitectureX86, map[string]bool{"fsn1": true, "nbg1": true}, "0.01"),
	})
	h.handlePricing(t, "0.004")

	clk := newFakeClock(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))
	p := New(h.client, h.pricing, WithClock(clk.Now), WithTTL(time.Hour))

	if _, err := p.List(context.Background(), []string{"fsn1"}); err != nil {
		t.Fatalf("first: %v", err)
	}
	if _, err := p.List(context.Background(), []string{"fsn1", "nbg1"}); err != nil {
		t.Fatalf("second: %v", err)
	}
	if got := h.serverTypes.Load(); got != 1 {
		t.Fatalf("expected 1 server_types fetch across location sets, got %d", got)
	}
}

func TestList_PropagatesUpstreamError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/server_types", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"code":"server_error","message":"boom"}}`))
	})
	mux.HandleFunc("/pricing", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	client := hcloud.NewClient(
		hcloud.WithEndpoint(server.URL),
		hcloud.WithToken("t"),
		hcloud.WithRetryOpts(hcloud.RetryOpts{BackoffFunc: hcloud.ConstantBackoff(time.Millisecond), MaxRetries: 1}),
	)
	p := New(client, pricing.New(client))
	if _, err := p.List(context.Background(), []string{"fsn1"}); err == nil {
		t.Fatal("expected upstream error to be propagated, got nil")
	}
}

func TestList_PricingFailureMarksAllOfferingsUnavailable(t *testing.T) {
	// If /pricing is broken, every offering for the type is reported
	// unavailable rather than booting nodes at a zero-cost default.
	mux := http.NewServeMux()
	mux.HandleFunc("/server_types", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(schema.ServerTypeListResponse{
			ServerTypes: []schema.ServerType{
				stTypeWithLocations("cx22", hcloud.ArchitectureX86, map[string]bool{"fsn1": true}, "0.01"),
			},
		})
	})
	mux.HandleFunc("/pricing", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	client := hcloud.NewClient(
		hcloud.WithEndpoint(server.URL),
		hcloud.WithToken("t"),
		hcloud.WithRetryOpts(hcloud.RetryOpts{BackoffFunc: hcloud.ConstantBackoff(time.Millisecond), MaxRetries: 1}),
	)
	p := New(client, pricing.New(client))

	its, err := p.List(context.Background(), []string{"fsn1"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(its) != 1 {
		t.Fatalf("expected 1 instance type, got %d", len(its))
	}
	if findOffering(t, its[0], "fsn1").Available {
		t.Fatal("pricing failure must mark every offering unavailable")
	}
}

func TestList_ConcurrentMarkAndList(t *testing.T) {
	h := newHarness(t)
	h.handleServerTypes(t, []schema.ServerType{
		stTypeWithLocations("cx22", hcloud.ArchitectureX86, map[string]bool{"fsn1": true}, "0.01"),
	})
	h.handlePricing(t, "0.004")

	clk := newFakeClock(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))
	p := New(h.client, h.pricing, WithClock(clk.Now), WithCooldown(10*time.Minute))

	const writers = 8
	const readers = 8
	const perG = 64
	var wg sync.WaitGroup
	wg.Add(writers + readers)
	for i := 0; i < writers; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < perG; j++ {
				p.MarkUnavailable("cx22", "fsn1")
			}
		}()
	}
	for i := 0; i < readers; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < perG; j++ {
				_, _ = p.List(context.Background(), []string{"fsn1"})
			}
		}()
	}
	wg.Wait()
}

func TestList_FamilyRequirementForKnownAndOtherPrefixes(t *testing.T) {
	h := newHarness(t)
	h.handleServerTypes(t, []schema.ServerType{
		stTypeWithLocations("cx22", hcloud.ArchitectureX86, map[string]bool{"fsn1": true}, "0.01"),
		stTypeWithLocations("cpx42", hcloud.ArchitectureX86, map[string]bool{"fsn1": true}, "0.04"),
		stTypeWithLocations("ccx13", hcloud.ArchitectureX86, map[string]bool{"fsn1": true}, "0.05"),
		stTypeWithLocations("cax21", hcloud.ArchitectureARM, map[string]bool{"fsn1": true}, "0.015"),
		stTypeWithLocations("fake-1", hcloud.ArchitectureX86, map[string]bool{"fsn1": true}, "0.5"),
	})
	h.handlePricing(t, "0.004")
	p := New(h.client, h.pricing)

	its, err := p.List(context.Background(), []string{"fsn1"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	want := map[string]string{
		"cx22":   "cx",
		"cpx42":  "cpx",
		"ccx13":  "ccx",
		"cax21":  "cax",
		"fake-1": "other",
	}
	for _, it := range its {
		got := it.Requirements.Get(LabelServerFamily).Any()
		if got != want[it.Name] {
			t.Fatalf("%s: family = %q, want %q", it.Name, got, want[it.Name])
		}
	}
}

func TestList_LocationRequirementIncludesAllRequestedZones(t *testing.T) {
	h := newHarness(t)
	h.handleServerTypes(t, []schema.ServerType{
		stTypeWithLocations("cx22", hcloud.ArchitectureX86, map[string]bool{"fsn1": true, "nbg1": true, "hel1": true}, "0.01"),
	})
	h.handlePricing(t, "0.004")
	p := New(h.client, h.pricing)

	its, err := p.List(context.Background(), []string{"fsn1", "nbg1", "hel1"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	r := its[0].Requirements.Get(corev1.LabelTopologyZone)
	if !r.Has("fsn1") || !r.Has("nbg1") || !r.Has("hel1") {
		t.Fatalf("topology requirement missing zones, got %v", r.Values())
	}
}

func TestMarkUnavailable_IgnoresEmptyArgs(t *testing.T) {
	h := newHarness(t)
	h.handleServerTypes(t, nil)
	h.handlePricing(t, "0.004")
	clk := newFakeClock(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))
	p := New(h.client, h.pricing, WithClock(clk.Now))

	// These must be no-ops, not panics.
	p.MarkUnavailable("", "fsn1")
	p.MarkUnavailable("cx22", "")
}

func TestMarkUnavailable_NormalisesLocationCase(t *testing.T) {
	h := newHarness(t)
	h.handleServerTypes(t, []schema.ServerType{
		stTypeWithLocations("cx22", hcloud.ArchitectureX86, map[string]bool{"fsn1": true}, "0.01"),
	})
	h.handlePricing(t, "0.004")
	clk := newFakeClock(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))
	p := New(h.client, h.pricing, WithClock(clk.Now), WithCooldown(time.Minute))

	p.MarkUnavailable("cx22", "FSN1")

	its, err := p.List(context.Background(), []string{"fsn1"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if findOffering(t, its[0], "fsn1").Available {
		t.Fatal("MarkUnavailable with FSN1 should mark fsn1 unavailable (case-insensitive)")
	}
}

// approxEqual compares two float64s within an absolute tolerance.
func approxEqual(a, b, tol float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d < tol
}
