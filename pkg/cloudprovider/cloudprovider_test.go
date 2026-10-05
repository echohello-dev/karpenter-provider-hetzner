package cloudprovider

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/awslabs/operatorpkg/status"
	"github.com/hetznercloud/hcloud-go/v2/hcloud"
	hcloudschema "github.com/hetznercloud/hcloud-go/v2/hcloud/schema"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	karpcp "sigs.k8s.io/karpenter/pkg/cloudprovider"

	apiv1 "github.com/echohello-dev/karpenter-provider-hetzner/v1/pkg/apis/v1"
	"github.com/echohello-dev/karpenter-provider-hetzner/v1/pkg/providers/imagefamily"
	"github.com/echohello-dev/karpenter-provider-hetzner/v1/pkg/providers/instance"
	"github.com/echohello-dev/karpenter-provider-hetzner/v1/pkg/providers/instancetype"
	"github.com/echohello-dev/karpenter-provider-hetzner/v1/pkg/providers/pricing"
)

func TestResolveArchitecture(t *testing.T) {
	cases := []struct {
		arch string
		want hcloud.Architecture
	}{
		{"arm64", hcloud.ArchitectureARM},
		{"amd64", hcloud.ArchitectureX86},
		{"", hcloud.ArchitectureX86}, // empty => default to x86 (matches upstream behaviour)
		{"other", hcloud.ArchitectureX86},
	}
	for _, tc := range cases {
		t.Run(tc.arch, func(t *testing.T) {
			if got := resolveArchitecture(tc.arch); got != tc.want {
				t.Fatalf("resolveArchitecture(%q) = %v, want %v", tc.arch, got, tc.want)
			}
		})
	}
}

// testEnv wires a fake controller-runtime client (with the HCloudNodeClass
// + Secret schemes registered) and a httptest-backed hcloud client. Tests
// can register handlers against env.mux to shape hcloud responses, and load
// NodeClasses / Secrets through env.kubeClient.
type testEnv struct {
	kubeClient client.Client
	hcloud     *hcloud.Client
	server     *httptest.Server
	mux        *http.ServeMux
}

func newTestEnv(t *testing.T, initObjs ...client.Object) *testEnv {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := apiv1.AddToScheme(scheme); err != nil {
		t.Fatalf("adding apiv1 to scheme: %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("adding corev1 to scheme: %v", err)
	}

	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	hcloudClient := hcloud.NewClient(
		hcloud.WithEndpoint(server.URL),
		hcloud.WithToken("test-token"),
		hcloud.WithRetryOpts(hcloud.RetryOpts{BackoffFunc: hcloud.ConstantBackoff(time.Microsecond), MaxRetries: 1}),
		hcloud.WithPollOpts(hcloud.PollOpts{BackoffFunc: hcloud.ConstantBackoff(time.Microsecond)}),
	)

	kubeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(initObjs...).
		WithStatusSubresource(&apiv1.HCloudNodeClass{}).
		Build()

	return &testEnv{
		kubeClient: kubeClient,
		hcloud:     hcloudClient,
		server:     server,
		mux:        mux,
	}
}

// validNodeClass returns a minimal HCloudNodeClass that satisfies the CRD's
// required fields. Tests can override individual fields after construction.
func validNodeClass(name string) *apiv1.HCloudNodeClass {
	nc := &apiv1.HCloudNodeClass{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: apiv1.HCloudNodeClassSpec{
			Locations:     []string{"fsn1", "hel1"},
			NetworkID:     12345,
			FirewallIDs:   []int64{9001},
			SSHKeyIDs:     []int64{7001},
			ImageSelector: apiv1.ImageSelector{Family: apiv1.ImageFamily("talos")},
		},
	}
	return nc
}

// readyNodeClass marks an HCloudNodeClass Ready=True on its status so the
// CloudProvider's readiness check accepts it for scheduling.
func readyNodeClass(t *testing.T, nc *apiv1.HCloudNodeClass) {
	t.Helper()
	nc.StatusConditions().SetTrue(status.ConditionReady)
}

// hcloudServerForTest builds a *hcloud.Server with the supplied identity and
// optional attachments. Tests use this to construct the "already-exists"
// scenarios for Get / List / Drift paths. Always x86 — the tests that need
// a different architecture can extend this helper. Attaches a primary
// public IPv4 and IPv6 by default (the provider's create defaults enable
// both); tests exercising missing-address scenarios can zero the
// PublicNet fields on the returned server.
func hcloudServerForTest(id int64, name string, stName string, location string, networkID int64, firewallIDs []int64, imageID int64, labels map[string]string) *hcloud.Server {
	srv := &hcloud.Server{
		ID:     id,
		Name:   name,
		Status: hcloud.ServerStatusRunning,
		ServerType: &hcloud.ServerType{
			Name:         stName,
			Architecture: hcloud.ArchitectureX86,
			Cores:        2,
			Memory:       4,
			Disk:         40,
		},
		Location: &hcloud.Location{Name: location},
		Image:    &hcloud.Image{ID: imageID, Architecture: hcloud.ArchitectureX86},
		Labels:   labels,
	}
	srv.PublicNet.IPv4 = hcloud.ServerPublicNetIPv4{IP: net.ParseIP("203.0.113.10")}
	ipv6, ipv6Net, _ := net.ParseCIDR("2001:db8:1::/64")
	srv.PublicNet.IPv6 = hcloud.ServerPublicNetIPv6{IP: ipv6, Network: ipv6Net}
	if networkID != 0 {
		srv.PrivateNet = []hcloud.ServerPrivateNet{{Network: &hcloud.Network{ID: networkID}}}
	}
	for _, fw := range firewallIDs {
		srv.PublicNet.Firewalls = append(srv.PublicNet.Firewalls, &hcloud.ServerFirewallStatus{
			Firewall: hcloud.Firewall{ID: fw},
			Status:   hcloud.FirewallStatusApplied,
		})
	}
	return srv
}

// hcloudServerJSON returns the schema-encoded representation of a Server
// (with metadata wrappers) for httptest responses.
func hcloudServerJSON(t *testing.T, srv *hcloud.Server) []byte {
	t.Helper()
	wrapper := struct {
		Server hcloudschema.Server `json:"server"`
	}{Server: serverToSchema(srv)}
	b, err := json.Marshal(wrapper)
	if err != nil {
		t.Fatalf("marshal server: %v", err)
	}
	return b
}

// serverToSchema converts an *hcloud.Server into its schema equivalent so we
// can return it from a test httptest handler. Keeps the test code from
// having to hand-build the schema struct for every fixture.
func serverToSchema(srv *hcloud.Server) hcloudschema.Server {
	if srv == nil {
		return hcloudschema.Server{}
	}
	out := hcloudschema.Server{
		ID:         srv.ID,
		Name:       srv.Name,
		Status:     string(srv.Status),
		Labels:     srv.Labels,
		ServerType: hcloudschema.ServerType{Name: srv.ServerType.Name},
		Location:   hcloudschema.Location{Name: srv.Location.Name},
	}
	if srv.Image != nil {
		out.Image = &hcloudschema.Image{ID: srv.Image.ID, Architecture: string(srv.Image.Architecture)}
	}
	// hcloud's server schema encodes the primary IPv4 as a plain address and
	// the primary IPv6 as a CIDR (its /64 network); keep that shape so the
	// httptest round-trip lands back on the same fields.
	if srv.PublicNet.IPv4.IP != nil {
		out.PublicNet.IPv4 = hcloudschema.ServerPublicNetIPv4{IP: srv.PublicNet.IPv4.IP.String()}
	}
	if srv.PublicNet.IPv6.IP != nil {
		ipv6 := srv.PublicNet.IPv6.IP.String() + "/64"
		if srv.PublicNet.IPv6.Network != nil {
			ipv6 = srv.PublicNet.IPv6.Network.String()
		}
		out.PublicNet.IPv6 = hcloudschema.ServerPublicNetIPv6{IP: ipv6}
	}
	for _, pn := range srv.PrivateNet {
		if pn.Network != nil {
			out.PrivateNet = append(out.PrivateNet, hcloudschema.ServerPrivateNet{Network: pn.Network.ID})
		}
	}
	for _, fw := range srv.PublicNet.Firewalls {
		out.PublicNet.Firewalls = append(out.PublicNet.Firewalls, hcloudschema.ServerFirewall{
			ID:     fw.Firewall.ID,
			Status: string(fw.Status),
		})
	}
	return out
}

// notFoundJSON is the canonical hcloud 404 body — tests use it so
// hcloud-go's error-decoder recognises the failure mode.
func notFoundJSON() []byte {
	return []byte(`{"error":{"code":"not_found","message":"not found"}}`)
}

// serverTypesJSON encodes a server-type catalog for the
// instancetype.Provider list path.
func serverTypesJSON(types ...hcloudschema.ServerType) []byte {
	wrapper := struct {
		ServerTypes []hcloudschema.ServerType `json:"server_types"`
	}{ServerTypes: types}
	b, _ := json.Marshal(wrapper)
	return b
}

// pricingJSON returns a minimal pricing document so the pricing provider
// can compute hourly prices for the test server types (cx22, cpx21). The
// hourlies here are placeholders — bin-packing code paths under test do
// not assert specific numbers, only ordering. IPv4 surcharge is set to 0
// so the price stays a single number per server type.
func pricingJSON() []byte {
	return []byte(`{"pricing":{"currency":"EUR","primary_ips":[{"type":"ipv4","prices":[{"location":"fsn1","price_hourly":{"net":"0","gross":"0"},"price_monthly":{"net":"3.29","gross":"3.29"}}]}],"floating_ips":[{"type":"ipv4"}],"server_types":[{"name":"cx22","prices":[{"location":"fsn1","price_hourly":{"net":"0.01","gross":"0.01"}}]},{"name":"cpx21","prices":[{"location":"fsn1","price_hourly":{"net":"0.02","gross":"0.02"}}]}]}}`)
}

// serverTypeFixture builds a schema.ServerType with the supplied (type,
// cores, memoryGB, diskGB, hourlyNet, locations, available-by-location)
// parameters. Always x86 — the cloudprovider tests don't need an ARM
// catalog to exercise the create/get/drift paths.
func serverTypeFixture(name string, cores, memGB, diskGB int, hourlyNet string, locations map[string]bool) hcloudschema.ServerType {
	locs := make([]hcloudschema.ServerTypeLocation, 0, len(locations))
	prices := make([]hcloudschema.PricingServerTypePrice, 0, len(locations))
	for loc, avail := range locations {
		locs = append(locs, hcloudschema.ServerTypeLocation{Name: loc, Available: avail})
		prices = append(prices, hcloudschema.PricingServerTypePrice{
			Location:    loc,
			PriceHourly: hcloudschema.Price{Net: hourlyNet, Gross: hourlyNet},
		})
	}
	return hcloudschema.ServerType{
		Name:         name,
		Architecture: string(hcloud.ArchitectureX86),
		Cores:        cores,
		Memory:       float32(memGB),
		Disk:         diskGB,
		StorageType:  "local",
		CPUType:      "shared",
		Locations:    locs,
		Prices:       prices,
	}
}

// setupHcloudBackend registers the standard endpoints an instancetype +
// pricing + instance provider needs (server_types, pricing, placement_groups,
// servers, servers/<id>, images) on the test mux. Returns a counter
// incremented for every server create call so tests can assert on the
// number of creates issued.
func setupHcloudBackend(t *testing.T, env *testEnv) *atomic.Int64 {
	t.Helper()
	creates := &atomic.Int64{}
	env.mux.HandleFunc("/placement_groups", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodGet:
			_, _ = w.Write([]byte(`{"placement_groups":[]}`))
		case http.MethodPost:
			_, _ = w.Write([]byte(`{"placement_group":{"id":700,"name":"karpenter-cluster","type":"spread","labels":{"karpenter.sh/cluster":"cluster-a"},"servers":[]},"action":{"id":1,"command":"create_placement_group"}}`))
		default:
			http.NotFound(w, r)
		}
	})
	env.mux.HandleFunc("/servers", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost {
			creates.Add(1)
			body := map[string]any{
				"server": map[string]any{
					"id":          4242,
					"name":        "karpenter-test",
					"status":      "initializing",
					"server_type": map[string]any{"name": "cx22", "architecture": "x86"},
					"image":       map[string]any{"id": 12, "architecture": "x86"},
					"location":    map[string]any{"name": "fsn1"},
					"private_net": []any{map[string]any{"network": 12345, "ip": "10.0.0.2"}},
					"public_net":  map[string]any{"ipv4": map[string]any{"ip": "1.2.3.4"}, "ipv6": map[string]any{"ip": "::1"}, "firewalls": []any{map[string]any{"id": 9001, "status": "applied"}}},
					"labels":      map[string]any{"karpenter.sh/cluster": "cluster-a"},
					"created":     "2026-01-01T00:00:00Z",
					"protection":  map[string]any{"delete": false, "rebuild": false},
				},
				"action": map[string]any{"id": 1, "command": "create_server"},
			}
			b, _ := json.Marshal(body)
			_, _ = w.Write(b)
			return
		}
		_, _ = w.Write([]byte(`{"servers":[]}`))
	})
	return creates
}

// newCloudProvider builds a fully-wired CloudProvider backed by the testEnv.
// Tests use this to exercise the public interface without having to repeat
// the construction boilerplate.
func newCloudProvider(env *testEnv) *CloudProvider {
	inst, err := instance.New(env.hcloud, "cluster-a")
	if err != nil {
		panic(err)
	}
	typeProv := instancetype.New(env.hcloud, pricing.New(env.hcloud))
	imageProv := imagefamily.New(env.hcloud)
	return New(env.kubeClient, inst, typeProv, imageProv)
}

// newNodeClaim builds a minimal NodeClaim referencing the named NodeClass,
// with the supplied requirements and resources.
func newNodeClaim(name, nodeClassName string, reqs []karpv1.NodeSelectorRequirementWithMinValues, resources corev1.ResourceList) *karpv1.NodeClaim {
	return &karpv1.NodeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: map[string]string{karpv1.NodePoolLabelKey: "pool-a"},
		},
		Spec: karpv1.NodeClaimSpec{
			NodeClassRef: &karpv1.NodeClassReference{
				Group: apiv1.GroupVersion.Group,
				Kind:  "HCloudNodeClass",
				Name:  nodeClassName,
			},
			Requirements: reqs,
			Resources:    karpv1.ResourceRequirements{Requests: resources},
		},
	}
}

// TestResolveNodeClass_RejectsBadGroup verifies that a NodeClassReference
// with a non-matching group is rejected before any API call.
func TestResolveNodeClass_RejectsBadGroup(t *testing.T) {
	env := newTestEnv(t)
	cp := newCloudProvider(env)

	_, err := cp.resolveNodeClass(context.Background(), &karpv1.NodeClassReference{
		Group: "wrong.example.com",
		Kind:  "HCloudNodeClass",
		Name:  "any",
	})
	if err == nil {
		t.Fatal("expected error for non-matching group, got nil")
	}
}

// TestResolveNodeClass_RejectsBadKind ensures the resolver requires the
// exact "HCloudNodeClass" kind — the group alone isn't enough.
func TestResolveNodeClass_RejectsBadKind(t *testing.T) {
	env := newTestEnv(t)
	cp := newCloudProvider(env)

	_, err := cp.resolveNodeClass(context.Background(), &karpv1.NodeClassReference{
		Group: apiv1.GroupVersion.Group,
		Kind:  "SomeOtherNodeClass",
		Name:  "any",
	})
	if err == nil {
		t.Fatal("expected error for non-matching kind, got nil")
	}
}

// TestResolveNodeClass_RejectsNilRef ensures a nil ref is surfaced as an
// error rather than triggering a nil-deref downstream.
func TestResolveNodeClass_RejectsNilRef(t *testing.T) {
	env := newTestEnv(t)
	cp := newCloudProvider(env)

	if _, err := cp.resolveNodeClass(context.Background(), nil); err == nil {
		t.Fatal("expected error for nil ref, got nil")
	}
}

// TestValidateNodeClassReady_RejectsDeletion checks that a NodeClass with
// a DeletionTimestamp is refused for new scheduling.
func TestValidateNodeClassReady_RejectsDeletion(t *testing.T) {
	nc := validNodeClass("dl")
	now := metav1.Now()
	nc.DeletionTimestamp = &now
	nc.Finalizers = []string{"keep.test/finalizer"}
	env := newTestEnv(t, nc)
	cp := newCloudProvider(env)

	err := cp.validateNodeClassReady(nc)
	if err == nil {
		t.Fatal("expected NodeClassNotReady error for deleted NodeClass")
	}
	if !karpcp.IsNodeClassNotReadyError(err) {
		t.Fatalf("expected NodeClassNotReadyError, got %T: %v", err, err)
	}
}

// TestValidateNodeClassReady_RejectsNotReady exercises the explicit
// Ready=False rejection.
func TestValidateNodeClassReady_RejectsNotReady(t *testing.T) {
	nc := validNodeClass("nr")
	nc.StatusConditions().SetFalse(status.ConditionReady, "NotReady", "still resolving images")
	env := newTestEnv(t, nc)
	cp := newCloudProvider(env)

	err := cp.validateNodeClassReady(nc)
	if err == nil {
		t.Fatal("expected NodeClassNotReady error for Ready=False")
	}
	if !karpcp.IsNodeClassNotReadyError(err) {
		t.Fatalf("expected NodeClassNotReadyError, got %T: %v", err, err)
	}
}

// TestValidateNodeClassReady_AcceptsReady confirms a Ready=True NodeClass
// sails through validation.
func TestValidateNodeClassReady_AcceptsReady(t *testing.T) {
	nc := validNodeClass("ok")
	readyNodeClass(t, nc)
	env := newTestEnv(t, nc)
	cp := newCloudProvider(env)

	if err := cp.validateNodeClassReady(nc); err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
}

// TestCreate_RejectsMissingNodeClass ensures a NodeClaim pointing at a
// non-existent NodeClass returns a wrapped NotFound error rather than
// silently succeeding.
func TestCreate_RejectsMissingNodeClass(t *testing.T) {
	env := newTestEnv(t)
	cp := newCloudProvider(env)

	nc := newNodeClaim("nc-a", "missing", nil, nil)
	_, err := cp.Create(context.Background(), nc)
	if err == nil {
		t.Fatal("expected error for missing NodeClass, got nil")
	}
}

// TestCreate_RejectsDeletion ensures Create refuses a NodeClass that is
// already being torn down.
func TestCreate_RejectsDeletion(t *testing.T) {
	nc := validNodeClass("being-deleted")
	now := metav1.Now()
	nc.DeletionTimestamp = &now
	nc.Finalizers = []string{"keep.test/finalizer"}
	env := newTestEnv(t, nc)
	cp := newCloudProvider(env)

	claim := newNodeClaim("nc-a", "being-deleted", nil, nil)
	_, err := cp.Create(context.Background(), claim)
	if err == nil {
		t.Fatal("expected error for deleted NodeClass, got nil")
	}
	if !karpcp.IsNodeClassNotReadyError(err) {
		t.Fatalf("expected NodeClassNotReadyError, got %T: %v", err, err)
	}
}

// TestCreate_HappyPath exercises the full create pipeline against a fake
// hcloud: it sets up the instance + image + pricing + instancetype mocks,
// points them at a tiny catalog, and verifies the NodeClaim that comes
// back carries the standard Karpenter labels and provider-owned fields.
func TestCreate_HappyPath(t *testing.T) {
	nc := validNodeClass("default")
	readyNodeClass(t, nc)

	env := newTestEnv(t, nc)
	env.mux.HandleFunc("/server_types", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(serverTypesJSON(
			serverTypeFixture("cx22", 2, 4, 40, "0.01", map[string]bool{"fsn1": true, "hel1": true}),
			serverTypeFixture("cpx21", 4, 8, 80, "0.02", map[string]bool{"fsn1": true}),
		))
	})
	env.mux.HandleFunc("/pricing", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(pricingJSON())
	})
	env.mux.HandleFunc("/images", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"images":[{"id":12,"status":"available","type":"snapshot","description":"talos v1.9.6","architecture":"x86"}]}`))
	})
	creates := setupHcloudBackend(t, env)

	cp := newCloudProvider(env)
	claim := newNodeClaim("nc-a", "default", []karpv1.NodeSelectorRequirementWithMinValues{
		karpv1.NodeSelectorRequirementWithMinValues{Key: corev1.LabelTopologyZone, Operator: corev1.NodeSelectorOpIn, Values: []string{"fsn1"}},
	}, nil)

	hydrated, err := cp.Create(context.Background(), claim)
	if err != nil {
		t.Fatalf("Create error = %v", err)
	}
	if hydrated == nil {
		t.Fatal("Create returned nil NodeClaim")
	}
	if got := hydrated.Status.ProviderID; got != instance.FormatProviderID(4242) {
		t.Fatalf("ProviderID = %q, want %q", got, instance.FormatProviderID(4242))
	}
	if got := hydrated.Status.ImageID; got != "12" {
		t.Fatalf("ImageID = %q, want 12", got)
	}
	if hydrated.Labels[corev1.LabelInstanceTypeStable] == "" {
		t.Fatalf("expected instance-type label, got %v", hydrated.Labels)
	}
	if hydrated.Labels[corev1.LabelTopologyZone] != "fsn1" {
		t.Fatalf("zone label = %q, want fsn1", hydrated.Labels[corev1.LabelTopologyZone])
	}
	if hydrated.Labels[karpv1.CapacityTypeLabelKey] != karpv1.CapacityTypeOnDemand {
		t.Fatalf("capacity-type label = %q, want %q", hydrated.Labels[karpv1.CapacityTypeLabelKey], karpv1.CapacityTypeOnDemand)
	}
	wantKey := karpv1.NodeClassLabelKey(schema.GroupKind{Group: apiv1.GroupVersion.Group, Kind: "HCloudNodeClass"})
	if got := hydrated.Labels[wantKey]; got != "default" {
		t.Fatalf("NodeClass label = %q, want default (key=%q)", got, wantKey)
	}
	cpuCap := hydrated.Status.Capacity[corev1.ResourceCPU]
	if cpuCap.Cmp(resource.Quantity{}) == 0 {
		t.Fatalf("expected non-zero CPU capacity, got %v", hydrated.Status.Capacity)
	}
	if creates.Load() != 1 {
		t.Fatalf("expected exactly one hcloud server create, got %d", creates.Load())
	}
}

// TestCreate_UsesResolvedImageFromStatus verifies Create boots the snapshot
// the NodeClass reconciler already resolved into status.resolvedImages
// instead of listing the project's snapshots again. The /images handler
// fails the test if it is ever called, so a regression to live resolution
// is caught rather than silently passing.
func TestCreate_UsesResolvedImageFromStatus(t *testing.T) {
	nc := validNodeClass("default")
	readyNodeClass(t, nc)
	nc.Status.ResolvedImages = []apiv1.ResolvedImage{
		{Architecture: "amd64", ImageID: 12},
		{Architecture: "arm64", ImageID: 13},
	}

	env := newTestEnv(t, nc)
	env.mux.HandleFunc("/server_types", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(serverTypesJSON(
			serverTypeFixture("cx22", 2, 4, 40, "0.01", map[string]bool{"fsn1": true}),
		))
	})
	env.mux.HandleFunc("/pricing", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(pricingJSON())
	})
	env.mux.HandleFunc("/images", func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected live image lookup (%s): status.resolvedImages should have been used", r.URL.RawQuery)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"images":[]}`))
	})
	setupHcloudBackend(t, env)

	cp := newCloudProvider(env)
	claim := newNodeClaim("nc-img", "default", []karpv1.NodeSelectorRequirementWithMinValues{
		karpv1.NodeSelectorRequirementWithMinValues{Key: corev1.LabelTopologyZone, Operator: corev1.NodeSelectorOpIn, Values: []string{"fsn1"}},
	}, nil)

	hydrated, err := cp.Create(context.Background(), claim)
	if err != nil {
		t.Fatalf("Create error = %v", err)
	}
	if got := hydrated.Status.ImageID; got != "12" {
		t.Fatalf("ImageID = %q, want 12 (from status.resolvedImages)", got)
	}
}

// TestCreate_FallsBackToLiveImageLookup covers the complement: when status
// has no entry for the architecture being provisioned (here an amd64 pick
// with only an arm64 entry recorded), Create must resolve the snapshot from
// the ImageSelector rather than booting nothing.
func TestCreate_FallsBackToLiveImageLookup(t *testing.T) {
	nc := validNodeClass("default")
	readyNodeClass(t, nc)
	nc.Status.ResolvedImages = []apiv1.ResolvedImage{
		{Architecture: "arm64", ImageID: 13},
	}

	env := newTestEnv(t, nc)
	env.mux.HandleFunc("/server_types", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(serverTypesJSON(
			serverTypeFixture("cx22", 2, 4, 40, "0.01", map[string]bool{"fsn1": true}),
		))
	})
	env.mux.HandleFunc("/pricing", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(pricingJSON())
	})
	env.mux.HandleFunc("/images", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"images":[{"id":12,"status":"available","type":"snapshot","description":"talos","architecture":"x86"}]}`))
	})
	setupHcloudBackend(t, env)

	cp := newCloudProvider(env)
	claim := newNodeClaim("nc-fb", "default", []karpv1.NodeSelectorRequirementWithMinValues{
		karpv1.NodeSelectorRequirementWithMinValues{Key: corev1.LabelTopologyZone, Operator: corev1.NodeSelectorOpIn, Values: []string{"fsn1"}},
	}, nil)

	hydrated, err := cp.Create(context.Background(), claim)
	if err != nil {
		t.Fatalf("Create error = %v", err)
	}
	if got := hydrated.Status.ImageID; got != "12" {
		t.Fatalf("ImageID = %q, want 12 (resolved live)", got)
	}
}

// TestCreate_PicksCheapest ensures the price-ordered selection picks the
// cheapest available type when both fit the requested resources.
func TestCreate_PicksCheapest(t *testing.T) {
	nc := validNodeClass("default")
	readyNodeClass(t, nc)
	env := newTestEnv(t, nc)
	env.mux.HandleFunc("/server_types", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(serverTypesJSON(
			serverTypeFixture("cx22", 2, 4, 40, "0.01", map[string]bool{"fsn1": true}),
			serverTypeFixture("cpx21", 4, 8, 80, "0.05", map[string]bool{"fsn1": true}),
		))
	})
	env.mux.HandleFunc("/pricing", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(pricingJSON())
	})
	env.mux.HandleFunc("/images", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"images":[{"id":12,"status":"available","type":"snapshot","description":"talos","architecture":"x86"}]}`))
	})
	env.mux.HandleFunc("/servers", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var body struct {
			ServerType json.RawMessage `json:"server_type"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		var stName string
		if err := json.Unmarshal(body.ServerType, &stName); err != nil {
			t.Errorf("decode server_type: %v", err)
		}
		if stName != "cx22" {
			t.Errorf("expected cheapest type cx22, got %q", stName)
		}
		_, _ = w.Write([]byte(`{"server":{"id":555,"name":"x","server_type":{"name":"cx22"},"image":{"id":12,"architecture":"x86"},"location":{"name":"fsn1"},"labels":{}}}`))
	})
	env.mux.HandleFunc("/placement_groups", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`{"placement_groups":[]}`))
			return
		}
		_, _ = w.Write([]byte(`{"placement_group":{"id":1,"name":"x","type":"spread","servers":[]}}`))
	})

	cp := newCloudProvider(env)
	claim := newNodeClaim("nc-pick", "default", []karpv1.NodeSelectorRequirementWithMinValues{
		karpv1.NodeSelectorRequirementWithMinValues{Key: corev1.LabelTopologyZone, Operator: corev1.NodeSelectorOpIn, Values: []string{"fsn1"}},
	}, corev1.ResourceList{
		corev1.ResourceCPU: *resource.NewQuantity(1, resource.DecimalSI),
	})
	if _, err := cp.Create(context.Background(), claim); err != nil {
		t.Fatalf("Create: %v", err)
	}
}

// TestCreate_InsufficientCapacityReturned ensures a NodeClaim whose
// requirements no type can satisfy returns InsufficientCapacityError rather
// than a generic failure.
func TestCreate_InsufficientCapacityReturned(t *testing.T) {
	nc := validNodeClass("default")
	readyNodeClass(t, nc)
	env := newTestEnv(t, nc)
	env.mux.HandleFunc("/server_types", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(serverTypesJSON(
			serverTypeFixture("cx22", 2, 4, 40, "0.01", map[string]bool{"fsn1": true}),
		))
	})
	env.mux.HandleFunc("/pricing", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(pricingJSON())
	})

	cp := newCloudProvider(env)
	// Request a zone that the catalog does not serve — the offering
	// filter will reject every type.
	claim := newNodeClaim("nc-zcap", "default", []karpv1.NodeSelectorRequirementWithMinValues{
		karpv1.NodeSelectorRequirementWithMinValues{Key: corev1.LabelTopologyZone, Operator: corev1.NodeSelectorOpIn, Values: []string{"nope"}},
	}, nil)
	_, err := cp.Create(context.Background(), claim)
	if err == nil {
		t.Fatal("expected InsufficientCapacityError, got nil")
	}
	if !karpcp.IsInsufficientCapacityError(err) {
		t.Fatalf("expected InsufficientCapacityError, got %T: %v", err, err)
	}
}

// TestCreate_HcloudResourceUnavailableMarksUnavailable verifies that an
// hcloud 4xx capacity-class error translates into InsufficientCapacityError
// AND records the (type, zone) cooldown so the next pass skips it.
func TestCreate_HcloudResourceUnavailableMarksUnavailable(t *testing.T) {
	nc := validNodeClass("default")
	readyNodeClass(t, nc)
	env := newTestEnv(t, nc)
	env.mux.HandleFunc("/server_types", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(serverTypesJSON(
			serverTypeFixture("cx22", 2, 4, 40, "0.01", map[string]bool{"fsn1": true}),
		))
	})
	env.mux.HandleFunc("/pricing", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(pricingJSON())
	})
	env.mux.HandleFunc("/images", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"images":[{"id":12,"status":"available","type":"snapshot","description":"talos","architecture":"x86"}]}`))
	})
	env.mux.HandleFunc("/placement_groups", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`{"placement_groups":[]}`))
			return
		}
		_, _ = w.Write([]byte(`{"placement_group":{"id":1,"name":"x","type":"spread","servers":[]}}`))
	})
	var seenInsufficient atomic.Bool
	env.mux.HandleFunc("/servers", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		seenInsufficient.Store(true)
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"error":{"code":"resource_unavailable","message":"no capacity"}}`))
	})

	cp := newCloudProvider(env)
	claim := newNodeClaim("nc-c", "default", []karpv1.NodeSelectorRequirementWithMinValues{
		karpv1.NodeSelectorRequirementWithMinValues{Key: corev1.LabelTopologyZone, Operator: corev1.NodeSelectorOpIn, Values: []string{"fsn1"}},
	}, nil)
	_, err := cp.Create(context.Background(), claim)
	if err == nil {
		t.Fatal("expected error from capacity-class failure")
	}
	if !karpcp.IsInsufficientCapacityError(err) {
		t.Fatalf("expected InsufficientCapacityError, got %T: %v", err, err)
	}
	if !seenInsufficient.Load() {
		t.Fatal("expected hcloud server-create call to be issued before translation")
	}
}

// TestCreate_ResolveUserDataSecret verifies that a UserDataSecretRef takes
// precedence over inline UserData and the resolved blob ends up on the
// server.
func TestCreate_ResolveUserDataSecret(t *testing.T) {
	nc := validNodeClass("default")
	readyNodeClass(t, nc)
	nc.Spec.UserDataSecretRef = &apiv1.UserDataSecretRef{
		Namespace: "kube-system",
		Name:      "talos-config",
		Key:       "value",
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "talos-config",
			Namespace: "kube-system",
		},
		Data: map[string][]byte{
			"value": []byte("#talos-config\nmachine:\n  hostname: node-a\n"),
		},
	}

	env := newTestEnv(t, nc, secret)
	env.mux.HandleFunc("/server_types", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(serverTypesJSON(
			serverTypeFixture("cx22", 2, 4, 40, "0.01", map[string]bool{"fsn1": true}),
		))
	})
	env.mux.HandleFunc("/pricing", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(pricingJSON())
	})
	env.mux.HandleFunc("/images", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"images":[{"id":12,"status":"available","type":"snapshot","description":"talos","architecture":"x86"}]}`))
	})
	env.mux.HandleFunc("/placement_groups", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`{"placement_groups":[]}`))
			return
		}
		_, _ = w.Write([]byte(`{"placement_group":{"id":1,"name":"x","type":"spread","servers":[]}}`))
	})
	env.mux.HandleFunc("/servers", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var body struct {
			UserData string `json:"user_data"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.UserData != "#talos-config\nmachine:\n  hostname: node-a\n" {
			t.Errorf("user_data = %q, want secret value", body.UserData)
		}
		_, _ = w.Write([]byte(`{"server":{"id":1,"name":"x","server_type":{"name":"cx22"},"image":{"id":12,"architecture":"x86"},"location":{"name":"fsn1"},"labels":{}}}`))
	})

	cp := newCloudProvider(env)
	claim := newNodeClaim("nc-s", "default", []karpv1.NodeSelectorRequirementWithMinValues{
		karpv1.NodeSelectorRequirementWithMinValues{Key: corev1.LabelTopologyZone, Operator: corev1.NodeSelectorOpIn, Values: []string{"fsn1"}},
	}, nil)
	if _, err := cp.Create(context.Background(), claim); err != nil {
		t.Fatalf("Create: %v", err)
	}
}

// TestDelete_TranslatesNotFound asserts that an hcloud 404 from the delete
// call surfaces as a typed NodeClaimNotFoundError, matching the Karpenter
// contract.
func TestDelete_TranslatesNotFound(t *testing.T) {
	env := newTestEnv(t)
	env.mux.HandleFunc("/servers/77", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write(notFoundJSON())
	})
	cp := newCloudProvider(env)

	claim := newNodeClaim("nc-x", "default", nil, nil)
	claim.Status.ProviderID = instance.FormatProviderID(77)
	err := cp.Delete(context.Background(), claim)
	if err == nil {
		t.Fatal("expected NodeClaimNotFoundError, got nil")
	}
	if !karpcp.IsNodeClaimNotFoundError(err) {
		t.Fatalf("expected NodeClaimNotFoundError, got %T: %v", err, err)
	}
}

// TestDelete_NoProviderIDReturnsNotFound exercises the early-exit when the
// NodeClaim has not yet been hydrated with a providerID.
func TestDelete_NoProviderIDReturnsNotFound(t *testing.T) {
	env := newTestEnv(t)
	cp := newCloudProvider(env)

	err := cp.Delete(context.Background(), newNodeClaim("nc-y", "default", nil, nil))
	if err == nil {
		t.Fatal("expected NodeClaimNotFoundError, got nil")
	}
	if !karpcp.IsNodeClaimNotFoundError(err) {
		t.Fatalf("expected NodeClaimNotFoundError, got %T: %v", err, err)
	}
}

// TestDelete_HappyPath confirms a successful hcloud delete returns nil.
func TestDelete_HappyPath(t *testing.T) {
	env := newTestEnv(t)
	env.mux.HandleFunc("/servers/55", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodGet:
			_, _ = w.Write(hcloudServerJSON(t, hcloudServerForTest(55, "karpenter-nc-ok", "cx22", "fsn1", 0, nil, 12, map[string]string{"karpenter.sh/cluster": "cluster-a"})))
		case http.MethodDelete:
			_, _ = w.Write([]byte(`{"action":{"id":1,"command":"delete_server"}}`))
		default:
			http.NotFound(w, r)
		}
	})
	cp := newCloudProvider(env)

	claim := newNodeClaim("nc-ok", "default", nil, nil)
	claim.Status.ProviderID = instance.FormatProviderID(55)
	if err := cp.Delete(context.Background(), claim); err != nil {
		t.Fatalf("Delete: %v", err)
	}
}

// TestGet_MissingReturnsNotFound asserts the Get path returns a typed
// NodeClaimNotFoundError when the underlying server is absent.
func TestGet_MissingReturnsNotFound(t *testing.T) {
	env := newTestEnv(t)
	env.mux.HandleFunc("/servers/33", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write(notFoundJSON())
	})
	cp := newCloudProvider(env)
	_, err := cp.Get(context.Background(), instance.FormatProviderID(33))
	if err == nil {
		t.Fatal("expected NodeClaimNotFoundError, got nil")
	}
	if !karpcp.IsNodeClaimNotFoundError(err) {
		t.Fatalf("expected NodeClaimNotFoundError, got %T: %v", err, err)
	}
}

// TestGet_HappyPath confirms a hydrated NodeClaim comes back with the
// expected standard labels and ProviderID.
func TestGet_HappyPath(t *testing.T) {
	env := newTestEnv(t)
	env.mux.HandleFunc("/servers/91", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(hcloudServerJSON(t, hcloudServerForTest(91, "karpenter-x", "cx22", "fsn1", 0, nil, 12, map[string]string{
			"karpenter.sh/cluster":  "cluster-a",
			"karpenter.sh/nodepool": "pool-a",
		})))
	})
	cp := newCloudProvider(env)
	got, err := cp.Get(context.Background(), instance.FormatProviderID(91))
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status.ProviderID != instance.FormatProviderID(91) {
		t.Fatalf("ProviderID = %q", got.Status.ProviderID)
	}
	if got.Status.ImageID != "12" {
		t.Fatalf("ImageID = %q", got.Status.ImageID)
	}
	if got.Labels[corev1.LabelInstanceTypeStable] != "cx22" {
		t.Fatalf("instance-type label = %q", got.Labels[corev1.LabelInstanceTypeStable])
	}
	if got.Labels[corev1.LabelTopologyZone] != "fsn1" {
		t.Fatalf("zone label = %q", got.Labels[corev1.LabelTopologyZone])
	}
	if got.Labels[karpv1.NodePoolLabelKey] != "pool-a" {
		t.Fatalf("nodepool label = %q", got.Labels[karpv1.NodePoolLabelKey])
	}
}

// TestList_OnlyIncludesClusterScopedServers ensures that the cluster label
// selector is forwarded to the hcloud list call and that the returned
// servers round-trip to hydrated NodeClaims.
func TestList_OnlyIncludesClusterScopedServers(t *testing.T) {
	env := newTestEnv(t)
	env.mux.HandleFunc("/servers", func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("label_selector"); got != "karpenter.sh/cluster=cluster-a" {
			t.Errorf("label_selector = %q, want karpenter.sh/cluster=cluster-a", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"servers":[
			{"id":1,"name":"k1","status":"running","server_type":{"name":"cx22"},"image":{"id":12,"architecture":"x86"},"location":{"name":"fsn1"},"labels":{"karpenter.sh/cluster":"cluster-a"}},
			{"id":2,"name":"k2","status":"running","server_type":{"name":"cpx21"},"image":{"id":12,"architecture":"x86"},"location":{"name":"hel1"},"labels":{"karpenter.sh/cluster":"cluster-a"}}
		]}`))
	})
	cp := newCloudProvider(env)
	out, err := cp.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("List returned %d nodes, want 2", len(out))
	}
	if out[0].Status.ProviderID != instance.FormatProviderID(1) || out[1].Status.ProviderID != instance.FormatProviderID(2) {
		t.Fatalf("unexpected provider IDs: %v / %v", out[0].Status.ProviderID, out[1].Status.ProviderID)
	}
}

// TestGetInstanceTypes_ResolvesNodeClass ensures GetInstanceTypes walks the
// NodePool -> NodeClass reference and returns the catalog for the
// NodeClass's allowed locations.
func TestGetInstanceTypes_ResolvesNodeClass(t *testing.T) {
	nc := validNodeClass("default")
	readyNodeClass(t, nc)
	env := newTestEnv(t, nc)
	env.mux.HandleFunc("/server_types", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(serverTypesJSON(
			serverTypeFixture("cx22", 2, 4, 40, "0.01", map[string]bool{"fsn1": true, "hel1": true}),
		))
	})
	env.mux.HandleFunc("/pricing", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(pricingJSON())
	})
	cp := newCloudProvider(env)
	its, err := cp.GetInstanceTypes(context.Background(), &karpv1.NodePool{
		Spec: karpv1.NodePoolSpec{
			Template: karpv1.NodeClaimTemplate{
				Spec: karpv1.NodeClaimTemplateSpec{
					NodeClassRef: &karpv1.NodeClassReference{
						Group: apiv1.GroupVersion.Group,
						Kind:  "HCloudNodeClass",
						Name:  "default",
					},
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("GetInstanceTypes: %v", err)
	}
	if len(its) != 1 || its[0].Name != "cx22" {
		t.Fatalf("unexpected instance types: %+v", its)
	}
}

// TestGetInstanceTypes_NilNodePoolReturnsEmpty ensures we don't crash when
// Karpenter hands us a nil NodePool — the call is expected to be defensive
// and return an empty list.
func TestGetInstanceTypes_NilNodePoolReturnsEmpty(t *testing.T) {
	env := newTestEnv(t)
	cp := newCloudProvider(env)
	got, err := cp.GetInstanceTypes(context.Background(), nil)
	if err != nil {
		t.Fatalf("GetInstanceTypes(nil): %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected empty list, got %+v", got)
	}
}

// TestGetInstanceTypes_ForeignNodePoolSkipped exercises the cross-provider
// pool path: a NodePool pointing at a different NodeClass kind should be
// silently skipped rather than causing an error.
func TestGetInstanceTypes_ForeignNodePoolSkipped(t *testing.T) {
	env := newTestEnv(t)
	cp := newCloudProvider(env)
	got, err := cp.GetInstanceTypes(context.Background(), &karpv1.NodePool{
		Spec: karpv1.NodePoolSpec{
			Template: karpv1.NodeClaimTemplate{
				Spec: karpv1.NodeClaimTemplateSpec{
					NodeClassRef: &karpv1.NodeClassReference{
						Group: "other.example.com",
						Kind:  "OtherNodeClass",
						Name:  "x",
					},
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("GetInstanceTypes(foreign): %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected empty list for foreign pool, got %+v", got)
	}
}

// TestIsDrifted_NoProviderIDReturnsNoDrift confirms that a NodeClaim which
// has not been hydrated yet (no ProviderID) does not trigger a drift
// signal — there is no live server to compare against.
func TestIsDrifted_NoProviderIDReturnsNoDrift(t *testing.T) {
	nc := validNodeClass("default")
	env := newTestEnv(t, nc)
	cp := newCloudProvider(env)
	claim := newNodeClaim("nc-x", "default", nil, nil)
	reason, err := cp.IsDrifted(context.Background(), claim)
	if err != nil {
		t.Fatalf("IsDrifted: %v", err)
	}
	if reason != "" {
		t.Fatalf("expected no drift, got %q", reason)
	}
}

// TestIsDrifted_MissingServerReturnsNotFound checks that an IsDrifted call
// against a deleted server returns a typed NodeClaimNotFoundError so the
// Karpenter drift controller can reap the NodeClaim.
func TestIsDrifted_MissingServerReturnsNotFound(t *testing.T) {
	nc := validNodeClass("default")
	env := newTestEnv(t, nc)
	env.mux.HandleFunc("/servers/77", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write(notFoundJSON())
	})
	cp := newCloudProvider(env)
	claim := newNodeClaim("nc-x", "default", nil, nil)
	claim.Status.ProviderID = instance.FormatProviderID(77)
	_, err := cp.IsDrifted(context.Background(), claim)
	if err == nil {
		t.Fatal("expected NodeClaimNotFoundError, got nil")
	}
	if !karpcp.IsNodeClaimNotFoundError(err) {
		t.Fatalf("expected NodeClaimNotFoundError, got %T: %v", err, err)
	}
}

// TestIsDrifted_ImageDrift verifies the first drift check fires when the
// server's image ID differs from the NodeClaim's recorded image.
func TestIsDrifted_ImageDrift(t *testing.T) {
	nc := validNodeClass("default")
	env := newTestEnv(t, nc)
	env.mux.HandleFunc("/servers/12", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(hcloudServerJSON(t, hcloudServerForTest(12, "k1", "cx22", "fsn1", 12345, []int64{9001}, 99, map[string]string{"karpenter.sh/cluster": "cluster-a"})))
	})
	cp := newCloudProvider(env)
	claim := newNodeClaim("nc-i", "default", nil, nil)
	claim.Status.ProviderID = instance.FormatProviderID(12)
	claim.Status.ImageID = "12"
	reason, err := cp.IsDrifted(context.Background(), claim)
	if err != nil {
		t.Fatalf("IsDrifted: %v", err)
	}
	if reason != DriftImage {
		t.Fatalf("expected %q, got %q", DriftImage, reason)
	}
}

// TestIsDrifted_NetworkDrift checks the second check fires when the server
// is not attached to the NodeClass's expected private network.
func TestIsDrifted_NetworkDrift(t *testing.T) {
	nc := validNodeClass("default")
	env := newTestEnv(t, nc)
	env.mux.HandleFunc("/servers/13", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Server is attached to a different network than what the
		// NodeClass (12345) requires.
		_, _ = w.Write(hcloudServerJSON(t, hcloudServerForTest(13, "k2", "cx22", "fsn1", 99999, []int64{9001}, 12, map[string]string{"karpenter.sh/cluster": "cluster-a"})))
	})
	cp := newCloudProvider(env)
	claim := newNodeClaim("nc-n", "default", nil, nil)
	claim.Status.ProviderID = instance.FormatProviderID(13)
	claim.Status.ImageID = "12"
	claim.Labels[corev1.LabelInstanceTypeStable] = "cx22"
	reason, err := cp.IsDrifted(context.Background(), claim)
	if err != nil {
		t.Fatalf("IsDrifted: %v", err)
	}
	if reason != DriftNetwork {
		t.Fatalf("expected %q, got %q", DriftNetwork, reason)
	}
}

// TestIsDrifted_FirewallDrift verifies that an expected firewall missing
// from the running server surfaces as firewall drift.
func TestIsDrifted_FirewallDrift(t *testing.T) {
	nc := validNodeClass("default")
	nc.Spec.FirewallIDs = []int64{9001, 9002}
	env := newTestEnv(t, nc)
	env.mux.HandleFunc("/servers/14", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Server only has firewall 9001 — 9002 is missing.
		_, _ = w.Write(hcloudServerJSON(t, hcloudServerForTest(14, "k3", "cx22", "fsn1", 12345, []int64{9001}, 12, map[string]string{"karpenter.sh/cluster": "cluster-a"})))
	})
	cp := newCloudProvider(env)
	claim := newNodeClaim("nc-fw", "default", nil, nil)
	claim.Status.ProviderID = instance.FormatProviderID(14)
	claim.Status.ImageID = "12"
	claim.Labels[corev1.LabelInstanceTypeStable] = "cx22"
	reason, err := cp.IsDrifted(context.Background(), claim)
	if err != nil {
		t.Fatalf("IsDrifted: %v", err)
	}
	if reason != DriftFirewall {
		t.Fatalf("expected %q, got %q", DriftFirewall, reason)
	}
}

// TestIsDrifted_ServerTypeDrift confirms the type check fires when the
// running server type does not match the recorded instance-type label.
func TestIsDrifted_ServerTypeDrift(t *testing.T) {
	nc := validNodeClass("default")
	env := newTestEnv(t, nc)
	env.mux.HandleFunc("/servers/15", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(hcloudServerJSON(t, hcloudServerForTest(15, "k4", "cpx21", "fsn1", 12345, []int64{9001}, 12, map[string]string{"karpenter.sh/cluster": "cluster-a"})))
	})
	cp := newCloudProvider(env)
	claim := newNodeClaim("nc-t", "default", nil, nil)
	claim.Status.ProviderID = instance.FormatProviderID(15)
	claim.Status.ImageID = "12"
	claim.Labels[corev1.LabelInstanceTypeStable] = "cx22" // mismatch
	reason, err := cp.IsDrifted(context.Background(), claim)
	if err != nil {
		t.Fatalf("IsDrifted: %v", err)
	}
	if reason != DriftServerType {
		t.Fatalf("expected %q, got %q", DriftServerType, reason)
	}
}

// TestIsDrifted_LocationDrift verifies the location check fires when the
// server is in a zone outside the NodeClass's allowed set.
func TestIsDrifted_LocationDrift(t *testing.T) {
	nc := validNodeClass("default")
	nc.Spec.Locations = []string{"fsn1"}
	env := newTestEnv(t, nc)
	env.mux.HandleFunc("/servers/16", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(hcloudServerJSON(t, hcloudServerForTest(16, "k5", "cx22", "nbg1", 12345, []int64{9001}, 12, map[string]string{"karpenter.sh/cluster": "cluster-a"})))
	})
	cp := newCloudProvider(env)
	claim := newNodeClaim("nc-l", "default", nil, nil)
	claim.Status.ProviderID = instance.FormatProviderID(16)
	claim.Status.ImageID = "12"
	claim.Labels[corev1.LabelInstanceTypeStable] = "cx22"
	reason, err := cp.IsDrifted(context.Background(), claim)
	if err != nil {
		t.Fatalf("IsDrifted: %v", err)
	}
	if reason != DriftLocation {
		t.Fatalf("expected %q, got %q", DriftLocation, reason)
	}
}

// TestIsDrifted_LabelsDrift checks the labels check fires when a
// NodeClass-spec label is missing or different on the server.
func TestIsDrifted_LabelsDrift(t *testing.T) {
	nc := validNodeClass("default")
	nc.Spec.Labels = map[string]string{"workload": "gpu"}
	env := newTestEnv(t, nc)
	env.mux.HandleFunc("/servers/17", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(hcloudServerJSON(t, hcloudServerForTest(17, "k6", "cx22", "fsn1", 12345, []int64{9001}, 12, map[string]string{
			"karpenter.sh/cluster": "cluster-a",
			"workload":             "cpu", // different from NodeClass
		})))
	})
	cp := newCloudProvider(env)
	claim := newNodeClaim("nc-la", "default", nil, nil)
	claim.Status.ProviderID = instance.FormatProviderID(17)
	claim.Status.ImageID = "12"
	claim.Labels[corev1.LabelInstanceTypeStable] = "cx22"
	reason, err := cp.IsDrifted(context.Background(), claim)
	if err != nil {
		t.Fatalf("IsDrifted: %v", err)
	}
	if reason != DriftLabels {
		t.Fatalf("expected %q, got %q", DriftLabels, reason)
	}
}

// TestIsDrifted_NoDrift confirms a server whose attributes all match
// returns an empty drift reason. Includes a matching status.resolvedImages
// entry so the image-selector check is exercised in its "consistent" state,
// and the fixture's default primary IPv4/IPv6 satisfy the enabled
// public-IP checks.
func TestIsDrifted_NoDrift(t *testing.T) {
	nc := validNodeClass("default")
	nc.Spec.Labels = map[string]string{"workload": "gpu"}
	nc.Status.ResolvedImages = []apiv1.ResolvedImage{
		{Architecture: "amd64", ImageID: 12},
	}
	env := newTestEnv(t, nc)
	env.mux.HandleFunc("/servers/18", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(hcloudServerJSON(t, hcloudServerForTest(18, "k7", "cx22", "fsn1", 12345, []int64{9001}, 12, map[string]string{
			"karpenter.sh/cluster": "cluster-a",
			"workload":             "gpu",
		})))
	})
	cp := newCloudProvider(env)
	claim := newNodeClaim("nc-ok", "default", nil, nil)
	claim.Status.ProviderID = instance.FormatProviderID(18)
	claim.Status.ImageID = "12"
	claim.Labels[corev1.LabelArchStable] = "amd64"
	claim.Labels[corev1.LabelInstanceTypeStable] = "cx22"
	reason, err := cp.IsDrifted(context.Background(), claim)
	if err != nil {
		t.Fatalf("IsDrifted: %v", err)
	}
	if reason != "" {
		t.Fatalf("expected no drift, got %q", reason)
	}
}

// TestIsDrifted_ImageSelectorDrift verifies the image-selector check fires
// when the NodeClass's status.resolvedImages entry for the NodeClaim's
// architecture no longer matches the image the NodeClaim booted — the
// mechanism that replaces running nodes when spec.imageSelector is edited.
// The server still runs the original snapshot, so the out-of-band image
// check must not fire first.
func TestIsDrifted_ImageSelectorDrift(t *testing.T) {
	nc := validNodeClass("default")
	nc.Status.ResolvedImages = []apiv1.ResolvedImage{
		{Architecture: "amd64", ImageID: 99}, // selector now resolves elsewhere
	}
	env := newTestEnv(t, nc)
	env.mux.HandleFunc("/servers/19", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(hcloudServerJSON(t, hcloudServerForTest(19, "k8", "cx22", "fsn1", 12345, []int64{9001}, 12, map[string]string{"karpenter.sh/cluster": "cluster-a"})))
	})
	cp := newCloudProvider(env)
	claim := newNodeClaim("nc-is", "default", nil, nil)
	claim.Status.ProviderID = instance.FormatProviderID(19)
	claim.Status.ImageID = "12"
	claim.Labels[corev1.LabelArchStable] = "amd64"
	reason, err := cp.IsDrifted(context.Background(), claim)
	if err != nil {
		t.Fatalf("IsDrifted: %v", err)
	}
	if reason != DriftImageSelector {
		t.Fatalf("expected %q, got %q", DriftImageSelector, reason)
	}
}

// TestIsDrifted_ImageSelectorDriftSkippedWhenStatusUnusable ensures the
// image-selector check never fires off an empty or unusable NodeClass
// status: no resolved images at all, no entry for the NodeClaim's
// architecture, or an entry with a zero image ID. An empty status must
// never churn nodes.
func TestIsDrifted_ImageSelectorDriftSkippedWhenStatusUnusable(t *testing.T) {
	cases := []struct {
		name   string
		images []apiv1.ResolvedImage
	}{
		{"no status", nil},
		{"wrong architecture", []apiv1.ResolvedImage{{Architecture: "arm64", ImageID: 99}}},
		{"zero image id", []apiv1.ResolvedImage{{Architecture: "amd64", ImageID: 0}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			nc := validNodeClass("default")
			nc.Status.ResolvedImages = tc.images
			env := newTestEnv(t, nc)
			env.mux.HandleFunc("/servers/20", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(hcloudServerJSON(t, hcloudServerForTest(20, "k9", "cx22", "fsn1", 12345, []int64{9001}, 12, map[string]string{"karpenter.sh/cluster": "cluster-a"})))
			})
			cp := newCloudProvider(env)
			claim := newNodeClaim("nc-isd", "default", nil, nil)
			claim.Status.ProviderID = instance.FormatProviderID(20)
			claim.Status.ImageID = "12"
			claim.Labels[corev1.LabelArchStable] = "amd64"
			claim.Labels[corev1.LabelInstanceTypeStable] = "cx22"
			reason, err := cp.IsDrifted(context.Background(), claim)
			if err != nil {
				t.Fatalf("IsDrifted: %v", err)
			}
			if reason != "" {
				t.Fatalf("expected no drift with unusable status, got %q", reason)
			}
		})
	}
}

// TestIsDrifted_PublicIPv4DriftWhenDisabledButAttached verifies the public
// IPv4 check fires when the NodeClass disables public IPv4 while the server
// still carries the billed primary address.
func TestIsDrifted_PublicIPv4DriftWhenDisabledButAttached(t *testing.T) {
	nc := validNodeClass("default")
	enabled := false
	nc.Spec.EnablePublicIPv4 = &enabled
	env := newTestEnv(t, nc)
	env.mux.HandleFunc("/servers/21", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// hcloudServerForTest attaches a primary public IPv4 by default.
		_, _ = w.Write(hcloudServerJSON(t, hcloudServerForTest(21, "k10", "cx22", "fsn1", 12345, []int64{9001}, 12, map[string]string{"karpenter.sh/cluster": "cluster-a"})))
	})
	cp := newCloudProvider(env)
	claim := newNodeClaim("nc-v4", "default", nil, nil)
	claim.Status.ProviderID = instance.FormatProviderID(21)
	claim.Status.ImageID = "12"
	claim.Labels[corev1.LabelInstanceTypeStable] = "cx22"
	reason, err := cp.IsDrifted(context.Background(), claim)
	if err != nil {
		t.Fatalf("IsDrifted: %v", err)
	}
	if reason != DriftPublicIPv4 {
		t.Fatalf("expected %q, got %q", DriftPublicIPv4, reason)
	}
}

// TestIsDrifted_PublicIPv4DriftWhenMissing covers the reverse mismatch: the
// NodeClass wants a public IPv4 (the default) but the server has none.
func TestIsDrifted_PublicIPv4DriftWhenMissing(t *testing.T) {
	nc := validNodeClass("default")
	env := newTestEnv(t, nc)
	env.mux.HandleFunc("/servers/22", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		srv := hcloudServerForTest(22, "k11", "cx22", "fsn1", 12345, []int64{9001}, 12, map[string]string{"karpenter.sh/cluster": "cluster-a"})
		srv.PublicNet.IPv4 = hcloud.ServerPublicNetIPv4{} // no primary IPv4
		_, _ = w.Write(hcloudServerJSON(t, srv))
	})
	cp := newCloudProvider(env)
	claim := newNodeClaim("nc-v4m", "default", nil, nil)
	claim.Status.ProviderID = instance.FormatProviderID(22)
	claim.Status.ImageID = "12"
	claim.Labels[corev1.LabelInstanceTypeStable] = "cx22"
	reason, err := cp.IsDrifted(context.Background(), claim)
	if err != nil {
		t.Fatalf("IsDrifted: %v", err)
	}
	if reason != DriftPublicIPv4 {
		t.Fatalf("expected %q, got %q", DriftPublicIPv4, reason)
	}
}

// TestIsDrifted_PublicIPv4ConsistentWhenDisabled confirms a private-network
// cluster (enablePublicIPv4=false) with a server that genuinely has no
// primary IPv4 reports no drift.
func TestIsDrifted_PublicIPv4ConsistentWhenDisabled(t *testing.T) {
	nc := validNodeClass("default")
	enabled := false
	nc.Spec.EnablePublicIPv4 = &enabled
	env := newTestEnv(t, nc)
	env.mux.HandleFunc("/servers/23", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		srv := hcloudServerForTest(23, "k12", "cx22", "fsn1", 12345, []int64{9001}, 12, map[string]string{"karpenter.sh/cluster": "cluster-a"})
		srv.PublicNet.IPv4 = hcloud.ServerPublicNetIPv4{} // no primary IPv4
		_, _ = w.Write(hcloudServerJSON(t, srv))
	})
	cp := newCloudProvider(env)
	claim := newNodeClaim("nc-v4c", "default", nil, nil)
	claim.Status.ProviderID = instance.FormatProviderID(23)
	claim.Status.ImageID = "12"
	claim.Labels[corev1.LabelInstanceTypeStable] = "cx22"
	reason, err := cp.IsDrifted(context.Background(), claim)
	if err != nil {
		t.Fatalf("IsDrifted: %v", err)
	}
	if reason != "" {
		t.Fatalf("expected no drift, got %q", reason)
	}
}

// TestIsDrifted_PublicIPv6DriftWhenDisabledButAttached is the IPv6 twin of
// the public IPv4 check.
func TestIsDrifted_PublicIPv6DriftWhenDisabledButAttached(t *testing.T) {
	nc := validNodeClass("default")
	enabled := false
	nc.Spec.EnablePublicIPv6 = &enabled
	env := newTestEnv(t, nc)
	env.mux.HandleFunc("/servers/24", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// hcloudServerForTest attaches a primary public IPv6 by default.
		_, _ = w.Write(hcloudServerJSON(t, hcloudServerForTest(24, "k13", "cx22", "fsn1", 12345, []int64{9001}, 12, map[string]string{"karpenter.sh/cluster": "cluster-a"})))
	})
	cp := newCloudProvider(env)
	claim := newNodeClaim("nc-v6", "default", nil, nil)
	claim.Status.ProviderID = instance.FormatProviderID(24)
	claim.Status.ImageID = "12"
	claim.Labels[corev1.LabelInstanceTypeStable] = "cx22"
	reason, err := cp.IsDrifted(context.Background(), claim)
	if err != nil {
		t.Fatalf("IsDrifted: %v", err)
	}
	if reason != DriftPublicIPv6 {
		t.Fatalf("expected %q, got %q", DriftPublicIPv6, reason)
	}
}

// TestBuildServerLabels_HasNodeClassAndStandardLabels asserts that the
// label set written to the hcloud server carries both the user-supplied
// NodeClass labels AND the standard Karpenter + NodeClass-reference labels.
func TestBuildServerLabels_HasNodeClassAndStandardLabels(t *testing.T) {
	nc := validNodeClass("default")
	nc.Spec.Labels = map[string]string{"team": "core"}
	claim := newNodeClaim("nc-l", "default", nil, nil)
	claim.Labels[karpv1.NodePoolLabelKey] = "pool-x"
	claim.Labels[corev1.LabelTopologyZone] = "fsn1"

	got := buildServerLabels(claim, nc)
	if got["team"] != "core" {
		t.Errorf("NodeClass label lost: %v", got)
	}
	if got[karpv1.NodePoolLabelKey] != "pool-x" {
		t.Errorf("NodePool label not propagated: %v", got)
	}
	if got[corev1.LabelTopologyZone] != "fsn1" {
		t.Errorf("zone label not propagated: %v", got)
	}
	if got[hcloudNodeClassLabelKey] != "default" {
		t.Errorf("NodeClass label = %v, want key %q with value default", got, hcloudNodeClassLabelKey)
	}
	if got[hcloudServerFamilyLabelKey] != "" {
		t.Errorf("unexpected server-family label without claim value: %v", got)
	}
	claim.Labels[instancetype.LabelServerFamily] = "cx"
	got = buildServerLabels(claim, nc)
	if got[hcloudServerFamilyLabelKey] != "cx" {
		t.Errorf("server-family label = %v, want key %q with value cx", got, hcloudServerFamilyLabelKey)
	}
	if got[instancetype.LabelServerFamily] != "" {
		t.Errorf("canonical server-family key leaked to hcloud labels: %v", got)
	}
	// The live API rejects any key containing "hetzner.cloud" (reserved
	// prefix), so the boundary must never emit one.
	for key := range got {
		if strings.Contains(key, "hetzner.cloud") {
			t.Errorf("label key %q uses the reserved hetzner.cloud namespace", key)
		}
	}
}

// TestClassifyInsufficientCapacity_MatchesKnownCodes makes sure the
// classifier picks up the four hcloud error codes we treat as
// capacity-class failures and ignores unrelated ones.
func TestClassifyInsufficientCapacity_MatchesKnownCodes(t *testing.T) {
	codes := []hcloud.ErrorCode{
		hcloud.ErrorCodeResourceUnavailable,
		hcloud.ErrorCodeResourceLimitExceeded,
		hcloud.ErrorCodePlacementError,
		hcloud.ErrorCodeNoSpaceLeftInLocation,
	}
	for _, code := range codes {
		t.Run(string(code), func(t *testing.T) {
			err := hcloud.Error{Code: code, Message: "x"}
			if got := classifyInsufficientCapacity(err); got == nil {
				t.Fatalf("expected non-nil classifier for %s", code)
			}
		})
	}
	t.Run("unrelated", func(t *testing.T) {
		err := hcloud.Error{Code: hcloud.ErrorCodeInvalidInput, Message: "bad"}
		if got := classifyInsufficientCapacity(err); got != nil {
			t.Fatalf("expected nil classifier for invalid_input, got %v", got)
		}
	})
	t.Run("plain", func(t *testing.T) {
		if got := classifyInsufficientCapacity(fmt.Errorf("plain error")); got != nil {
			t.Fatalf("expected nil for non-hcloud error, got %v", got)
		}
	})
}

// TestIsHcloudNotFound verifies the typed 404 detection works as expected.
func TestIsHcloudNotFound(t *testing.T) {
	if !isHcloudNotFound(hcloud.Error{Code: hcloud.ErrorCodeNotFound, Message: "x"}) {
		t.Fatal("expected true for not_found code")
	}
	if isHcloudNotFound(hcloud.Error{Code: hcloud.ErrorCodeInvalidInput, Message: "x"}) {
		t.Fatal("expected false for invalid_input code")
	}
	if isHcloudNotFound(nil) {
		t.Fatal("expected false for nil error")
	}
	if isHcloudNotFound(fmt.Errorf("plain")) {
		t.Fatal("expected false for non-hcloud error")
	}
}
