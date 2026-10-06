package fakehcloud_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/hetznercloud/hcloud-go/v2/hcloud"

	"github.com/echohello-dev/karpenter-provider-hetzner/v1/test/e2e/fakehcloud"
)

func testCatalog() fakehcloud.Config {
	return fakehcloud.Config{
		Images: []fakehcloud.Image{
			{ID: 11, Description: "talos v1.9.5", Architecture: "x86", Type: "snapshot", Created: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)},
			{ID: 12, Description: "talos v1.9.6", Architecture: "x86", Type: "snapshot", Created: time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)},
			{ID: 13, Description: "talos v1.9.5", Architecture: "arm", Type: "snapshot", Created: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)},
		},
		ServerTypes: []fakehcloud.ServerType{
			{Name: "cx22", Architecture: "x86", Cores: 2, MemoryGB: 4, DiskGB: 40, HourlyNet: "0.01", Locations: map[string]bool{"fsn1": true}},
		},
		Locations: []fakehcloud.Location{{ID: 1, Name: "fsn1", NetworkZone: "eu-central"}},
		Networks:  map[int64]string{12345: "cluster-net"},
	}
}

// TestServerLifecycleRoundTrip drives create → get → list → delete through
// the hcloud client so the fake's wire format stays compatible with the SDK
// the provider uses.
func TestServerLifecycleRoundTrip(t *testing.T) {
	backend := fakehcloud.New(testCatalog())
	t.Cleanup(backend.Close)
	client := backend.Client()
	ctx := context.Background()

	result, _, err := client.Server.Create(ctx, hcloud.ServerCreateOpts{
		Name:       "karpenter-claim-a",
		ServerType: &hcloud.ServerType{Name: "cx22"},
		Image:      &hcloud.Image{ID: 12},
		Location:   &hcloud.Location{Name: "fsn1"},
		Labels:     map[string]string{"karpenter.sh/cluster": "e2e"},
		Networks:   []*hcloud.Network{{ID: 12345}},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if result.Server == nil || result.Server.ID == 0 {
		t.Fatalf("create returned no server: %+v", result)
	}
	id := result.Server.ID

	got, _, err := client.Server.GetByID(ctx, id)
	if err != nil || got == nil {
		t.Fatalf("get: %v (server=%v)", err, got)
	}
	if got.Labels["karpenter.sh/cluster"] != "e2e" {
		t.Errorf("labels = %v, want karpenter.sh/cluster=e2e", got.Labels)
	}
	if len(got.PrivateNet) != 1 || got.PrivateNet[0].Network == nil || got.PrivateNet[0].Network.ID != 12345 {
		t.Errorf("private nets = %v, want [12345]", got.PrivateNet)
	}

	all, err := client.Server.AllWithOpts(ctx, hcloud.ServerListOpts{
		ListOpts: hcloud.ListOpts{LabelSelector: "karpenter.sh/cluster=e2e"},
	})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("list returned %d servers, want 1", len(all))
	}

	if _, err := client.Server.Delete(ctx, got); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if backend.ServerCount() != 0 {
		t.Fatalf("server count = %d after delete, want 0", backend.ServerCount())
	}
	after, _, err := client.Server.GetByID(ctx, id)
	if err != nil {
		t.Fatalf("get after delete: %v", err)
	}
	if after != nil {
		t.Fatalf("server %d still present after delete", id)
	}
}

// TestCreateRejectsReservedLabelKeys mirrors the live API's reservation of
// the "hetzner.cloud" label namespace — the constraint that motivated the
// boundary rename in pkg/cloudprovider.
func TestCreateRejectsReservedLabelKeys(t *testing.T) {
	backend := fakehcloud.New(testCatalog())
	t.Cleanup(backend.Close)
	ctx := context.Background()

	_, _, err := backend.Client().Server.Create(ctx, hcloud.ServerCreateOpts{
		Name:       "bad-labels",
		ServerType: &hcloud.ServerType{Name: "cx22"},
		Image:      &hcloud.Image{ID: 12},
		Location:   &hcloud.Location{Name: "fsn1"},
		Labels:     map[string]string{"karpenter.hetzner.cloud/hcloudnodeclass": "default"},
	})
	if err == nil {
		t.Fatal("expected reserved label key to be rejected, got nil error")
	}
	if !hcloud.IsError(err, hcloud.ErrorCodeInvalidInput) {
		t.Fatalf("error = %v, want invalid_input", err)
	}
	if !strings.Contains(err.Error(), "labels") {
		t.Errorf("error = %v, want it to name the labels field", err)
	}
	if backend.ServerCount() != 0 {
		t.Fatalf("rejected create still left %d server(s) behind", backend.ServerCount())
	}
}

// TestCreateRejectsMalformedLabels verifies the fake also enforces hcloud's
// label-format rules, not just the reserved namespace.
func TestCreateRejectsMalformedLabels(t *testing.T) {
	backend := fakehcloud.New(testCatalog())
	t.Cleanup(backend.Close)
	ctx := context.Background()

	_, _, err := backend.Client().Server.Create(ctx, hcloud.ServerCreateOpts{
		Name:       "bad-labels",
		ServerType: &hcloud.ServerType{Name: "cx22"},
		Image:      &hcloud.Image{ID: 12},
		Location:   &hcloud.Location{Name: "fsn1"},
		Labels:     map[string]string{"not a valid key": "x"},
	})
	if !hcloud.IsError(err, hcloud.ErrorCodeInvalidInput) {
		t.Fatalf("error = %v, want invalid_input for malformed label key", err)
	}
}

// TestSetServerImageInjectsDrift confirms the drift-injection seam reports
// the mutated image through the SDK.
func TestSetServerImageInjectsDrift(t *testing.T) {
	backend := fakehcloud.New(testCatalog())
	t.Cleanup(backend.Close)
	client := backend.Client()
	ctx := context.Background()

	result, _, err := client.Server.Create(ctx, hcloud.ServerCreateOpts{
		Name:       "drift-me",
		ServerType: &hcloud.ServerType{Name: "cx22"},
		Image:      &hcloud.Image{ID: 12},
		Location:   &hcloud.Location{Name: "fsn1"},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := backend.SetServerImage(result.Server.ID, 11); err != nil {
		t.Fatalf("SetServerImage: %v", err)
	}
	got, _, err := client.Server.GetByID(ctx, result.Server.ID)
	if err != nil || got == nil {
		t.Fatalf("get: %v", err)
	}
	if got.Image == nil || got.Image.ID != 11 {
		t.Fatalf("server image = %+v, want id 11", got.Image)
	}
}

// TestImageListFiltering checks the fake honours the architecture and
// label_selector query filters the image family provider relies on.
func TestImageListFiltering(t *testing.T) {
	backend := fakehcloud.New(testCatalog())
	t.Cleanup(backend.Close)
	ctx := context.Background()

	x86, err := backend.Client().Image.AllWithOpts(ctx, hcloud.ImageListOpts{
		Architecture: []hcloud.Architecture{hcloud.ArchitectureX86},
		Type:         []hcloud.ImageType{hcloud.ImageTypeSnapshot},
		Status:       []hcloud.ImageStatus{hcloud.ImageStatusAvailable},
	})
	if err != nil {
		t.Fatalf("list x86 images: %v", err)
	}
	if len(x86) != 2 {
		t.Fatalf("x86 snapshots = %v, want 2 (ids 11, 12)", x86)
	}

	emptyOpts := hcloud.ImageListOpts{}
	emptyOpts.LabelSelector = "caph-image-name=talos-v1.9.5-gvisor"
	empty, err := backend.Client().Image.AllWithOpts(ctx, emptyOpts)
	if err != nil {
		t.Fatalf("list by label: %v", err)
	}
	if len(empty) != 0 {
		t.Fatalf("label selector matched %v, want none", empty)
	}
}

// TestServerCreatePublicIPsRoundTrip pins the fake's public-net wire format
// to what Hetzner actually returns: a primary IPv4 address and a primary
// IPv6 in CIDR form.
//
// hcloud-go decodes the IPv6 with net.ParseCIDR and discards the error, so a
// bare address like "::1" decodes to a nil IP. The SDK's IsUnspecified() then
// reports "no IPv6" on a server that does have one, which makes the provider's
// public-IP drift checks fire on a freshly created server.
func TestServerCreatePublicIPsRoundTrip(t *testing.T) {
	backend := fakehcloud.New(testCatalog())
	t.Cleanup(backend.Close)
	ctx := context.Background()

	created, _, err := backend.Client().Server.Create(ctx, hcloud.ServerCreateOpts{
		Name:       "karpenter-claim-publicnet",
		ServerType: &hcloud.ServerType{Name: "cx22"},
		Image:      &hcloud.Image{ID: 12},
		Location:   &hcloud.Location{Name: "fsn1"},
		PublicNet:  &hcloud.ServerCreatePublicNet{EnableIPv4: true, EnableIPv6: true},
	})
	if err != nil {
		t.Fatalf("create server: %v", err)
	}

	assertPublicIPs := func(stage string, server *hcloud.Server) {
		t.Helper()
		if server == nil {
			t.Fatalf("%s: server is nil", stage)
		}
		if server.PublicNet.IPv4.IsUnspecified() {
			t.Errorf("%s: primary IPv4 did not round-trip (got %v)", stage, server.PublicNet.IPv4.IP)
		}
		if server.PublicNet.IPv6.IsUnspecified() {
			t.Errorf("%s: primary IPv6 did not round-trip (got %v) — the fake must emit a CIDR, "+
				"because hcloud-go parses IPv6 with net.ParseCIDR and drops the error", stage, server.PublicNet.IPv6.IP)
		}
	}
	assertPublicIPs("create response", created.Server)

	got, _, err := backend.Client().Server.GetByID(ctx, created.Server.ID)
	if err != nil {
		t.Fatalf("get server: %v", err)
	}
	assertPublicIPs("get response", got)
}
