package e2e

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/awslabs/operatorpkg/status"
	"github.com/hetznercloud/hcloud-go/v2/hcloud"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	karpcp "sigs.k8s.io/karpenter/pkg/cloudprovider"

	apiv1 "github.com/echohello-dev/karpenter-provider-hetzner/v1/pkg/apis/v1"
	"github.com/echohello-dev/karpenter-provider-hetzner/v1/pkg/cloudprovider"
	"github.com/echohello-dev/karpenter-provider-hetzner/v1/pkg/providers/imagefamily"
	"github.com/echohello-dev/karpenter-provider-hetzner/v1/pkg/providers/instance"
	"github.com/echohello-dev/karpenter-provider-hetzner/v1/pkg/providers/instancetype"
	"github.com/echohello-dev/karpenter-provider-hetzner/v1/pkg/providers/pricing"
)

// TestE2E_RealHetzner_Lifecycle is an explicitly opt-in smoke test against
// the LIVE Hetzner Cloud API. It is never part of the default run:
//
//	E2E=1 HCLOUD_TOKEN=... mise run e2e
//
// Optional knobs: E2E_SERVER_TYPE (optional; when unset the test
// auto-selects the cheapest server type Hetzner reports available at
// E2E_LOCATION), E2E_LOCATION (default fsn1), E2E_CLUSTER_NAME (default
// karpenter-e2e).
//
// What is real here: the Hetzner Cloud API — the test creates one billed
// server (seconds old, IPv6-only to skip the IPv4 surcharge), reads it back
// through CloudProvider.Get, checks IsDrifted, and deletes it again. What
// stays local: the Kubernetes side (a controller-runtime fake client holding
// the HCloudNodeClass), so no cluster or kubeconfig is needed. The NodeClass
// status.resolvedImages is seeded with a live image ID picked from the
// account so the create path boots something that actually exists.
//
// WARNING: this creates and deletes a real, billed Hetzner server. Clean-up
// is registered before any assertions and retries until deletion is confirmed.
func TestE2E_RealHetzner_Lifecycle(t *testing.T) {
	if os.Getenv("E2E") != "1" {
		t.Skip("opt-in test: set E2E=1 to run the real Hetzner Cloud path (see docs: Testing)")
	}
	token := os.Getenv("HCLOUD_TOKEN")
	if token == "" {
		t.Skip("HCLOUD_TOKEN not set; skipping the real Hetzner Cloud path")
	}

	location := envOr("E2E_LOCATION", "fsn1")
	typeName := os.Getenv("E2E_SERVER_TYPE") // empty = auto-select cheapest available
	cluster := envOr("E2E_CLUSTER_NAME", "karpenter-e2e")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	client := hcloud.NewClient(hcloud.WithToken(token))
	runRealHetznerLifecycle(t, ctx, client, location, typeName, cluster)
}

func runRealHetznerLifecycle(t *testing.T, ctx context.Context, client *hcloud.Client, location, typeName, cluster string) {
	t.Helper()
	if typeName == "" {
		// The catalog churns (cx22 vanished, cx23 flips availability), so
		// with no explicit pin the test follows Hetzner's live availability
		// instead of hard-coding a type that may not exist tomorrow.
		typeName = pickLiveServerType(t, ctx, client, location)
	}
	// Pick a boot image compatible with the requested server type.
	image := pickRealImage(t, ctx, client, typeName)
	architecture := "amd64"
	if image.Architecture == hcloud.ArchitectureARM {
		architecture = "arm64"
	}

	scheme := runtime.NewScheme()
	if err := apiv1.AddToScheme(scheme); err != nil {
		t.Fatalf("adding apiv1 to scheme: %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("adding corev1 to scheme: %v", err)
	}

	enableIPv4, enableIPv6 := false, true
	nodeClass := &apiv1.HCloudNodeClass{
		ObjectMeta: metav1.ObjectMeta{Name: "e2e-real"},
		Spec: apiv1.HCloudNodeClassSpec{
			Locations:              []string{location},
			NetworkID:              0,
			ImageSelector:          apiv1.ImageSelector{Family: apiv1.ImageFamily("ubuntu")},
			Labels:                 map[string]string{"team": "platform"},
			PlacementGroupStrategy: apiv1.PlacementGroupStrategy("none"),
			EnablePublicIPv4:       &enableIPv4,
			EnablePublicIPv6:       &enableIPv6,
		},
		Status: apiv1.HCloudNodeClassStatus{
			ResolvedImages: []apiv1.ResolvedImage{{Architecture: architecture, ImageID: image.ID}},
		},
	}
	nodeClass.StatusConditions().SetTrue(status.ConditionReady)
	kube := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(nodeClass).
		WithStatusSubresource(&apiv1.HCloudNodeClass{}).
		Build()

	inst, err := instance.New(client, cluster)
	if err != nil {
		t.Fatalf("building instance provider: %v", err)
	}
	cp := cloudprovider.New(kube, inst, instancetype.New(client, pricing.New(client)), imagefamily.New(client))

	claim := newRealNodeClaim(location, typeName)

	created, err := cp.Create(ctx, claim)
	if err != nil {
		t.Fatalf("Create against live Hetzner API: %v", err)
	}
	// Cleanup has its own context so it can run even after the test times out.
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cleanupCancel()
		if err := deleteRealServer(cleanupCtx, cp, created); err != nil {
			t.Errorf("clean-up delete of %s: %v", created.Status.ProviderID, err)
		}
	})

	if created.Status.ProviderID == "" {
		t.Fatal("Create returned empty ProviderID")
	}
	if err := waitRealServer(ctx, client, created.Status.ProviderID); err != nil {
		t.Fatalf("waiting for live server creation: %v", err)
	}
	if created.Status.ImageID != hcloudID(image.ID) {
		t.Errorf("Status.ImageID = %q, want %q (live image)", created.Status.ImageID, hcloudID(image.ID))
	}
	if created.Labels[corev1.LabelInstanceTypeStable] != typeName {
		t.Errorf("instance-type label = %q, want %q", created.Labels[corev1.LabelInstanceTypeStable], typeName)
	}

	got, err := cp.Get(ctx, created.Status.ProviderID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status.ProviderID != created.Status.ProviderID {
		t.Errorf("Get ProviderID = %q, want %q", got.Status.ProviderID, created.Status.ProviderID)
	}

	reason, err := cp.IsDrifted(ctx, created)
	if err != nil {
		t.Fatalf("IsDrifted: %v", err)
	}
	if reason != "" {
		t.Errorf("IsDrifted = %q on a freshly created live server, want no drift", reason)
	}

	if err := deleteRealServer(ctx, cp, created); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := cp.Get(ctx, created.Status.ProviderID); !karpcp.IsNodeClaimNotFoundError(err) {
		t.Fatalf("Get after delete: got %v, want NodeClaimNotFoundError", err)
	}
}

func newRealNodeClaim(location, typeName string) *karpv1.NodeClaim {
	claim := newNodeClaim("e2e-real-claim", "e2e-real")
	claim.Spec.Requirements = []karpv1.NodeSelectorRequirementWithMinValues{
		{Key: corev1.LabelTopologyZone, Operator: corev1.NodeSelectorOpIn, Values: []string{location}},
		{Key: corev1.LabelInstanceTypeStable, Operator: corev1.NodeSelectorOpIn, Values: []string{typeName}},
	}
	return claim
}

// pickLiveServerType returns the cheapest server type the live catalog
// reports as available at location (with at least one CPU), discovered
// through the provider's own instancetype + pricing path. It keeps the
// smoke test working as Hetzner's catalog churns, instead of hard-coding a
// type that may vanish or lose availability.
func pickLiveServerType(t *testing.T, ctx context.Context, client *hcloud.Client, location string) string {
	t.Helper()
	its, err := instancetype.New(client, pricing.New(client)).List(ctx, []string{location})
	if err != nil {
		t.Fatalf("listing live server types for %s: %v", location, err)
	}
	bestName := ""
	bestPrice := 0.0
	for _, it := range its {
		if it == nil {
			continue
		}
		cpu := it.Capacity[corev1.ResourceCPU]
		if cpu.Value() < 1 {
			continue
		}
		for _, offering := range it.Offerings {
			if offering == nil || !offering.Available || offering.Zone() != location {
				continue
			}
			if bestName == "" || offering.Price < bestPrice {
				bestName, bestPrice = it.Name, offering.Price
			}
		}
	}
	if bestName == "" {
		t.Fatalf("no server type is available at %s right now; set E2E_SERVER_TYPE explicitly or pick another E2E_LOCATION", location)
	}
	t.Logf("auto-selected server type %s at %s (hourly net %.4f)", bestName, location, bestPrice)
	return bestName
}

// pickRealImage returns the newest available snapshot matching the server
// type's architecture whose captured disk fits the type, or a system image
// of that architecture as a fallback. The live API rejects boots when the
// image's disk size exceeds the server type's disk ("image disk is bigger
// than server type disk"), so oversized snapshots are skipped.
func pickRealImage(t *testing.T, ctx context.Context, client *hcloud.Client, typeName string) *hcloud.Image {
	t.Helper()
	serverType, _, err := client.ServerType.GetByName(ctx, typeName)
	if err != nil {
		t.Fatalf("fetching live server type %q: %v", typeName, err)
	}
	if serverType == nil {
		t.Fatalf("live server type %q not found", typeName)
	}
	arch := serverType.Architecture
	if arch != hcloud.ArchitectureX86 && arch != hcloud.ArchitectureARM {
		t.Fatalf("unsupported architecture %q for live server type %q", arch, typeName)
	}
	images, _, err := client.Image.List(ctx, hcloud.ImageListOpts{
		Type:         []hcloud.ImageType{hcloud.ImageTypeSnapshot},
		Status:       []hcloud.ImageStatus{hcloud.ImageStatusAvailable},
		Sort:         []string{"created:desc"},
		Architecture: []hcloud.Architecture{arch},
	})
	if err != nil {
		t.Fatalf("listing live snapshots: %v", err)
	}
	for _, img := range images {
		if img.DiskSize == 0 || img.DiskSize <= float32(serverType.Disk) {
			return img
		}
	}
	img, _, err := client.Image.GetByNameAndArchitecture(ctx, "ubuntu-24.04", arch)
	if err != nil {
		t.Fatalf("fetching fallback image ubuntu-24.04: %v", err)
	}
	if img == nil || img.DiskSize > float32(serverType.Disk) {
		t.Fatalf("account has no %s snapshot or ubuntu-24.04 system image fitting a %d GB disk on %q; set E2E_SERVER_TYPE to a bigger type or add a compatible boot image", arch, serverType.Disk, typeName)
	}
	return img
}

func waitRealServer(ctx context.Context, client *hcloud.Client, providerID string) error {
	id, _, err := instance.ParseProviderID(providerID)
	if err != nil {
		return err
	}
	return wait.PollUntilContextCancel(ctx, time.Second, true, func(ctx context.Context) (bool, error) {
		server, _, err := client.Server.GetByID(ctx, id)
		if err != nil {
			return false, err
		}
		if server == nil {
			return false, fmt.Errorf("server %s disappeared during creation", providerID)
		}
		return !server.Locked && server.Status == hcloud.ServerStatusRunning, nil
	})
}

// deleteRealServer retries failed deletion requests, then waits for absence;
// accepting a delete request does not mean the asynchronous action is finished.
func deleteRealServer(ctx context.Context, cp *cloudprovider.CloudProvider, claim *karpv1.NodeClaim) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	deleteRequested := false
	var lastErr error
	err := wait.PollUntilContextCancel(ctx, time.Second, true, func(ctx context.Context) (bool, error) {
		_, err := cp.Get(ctx, claim.Status.ProviderID)
		if karpcp.IsNodeClaimNotFoundError(err) {
			return true, nil
		}
		if err != nil {
			lastErr = err
		} else if !deleteRequested {
			err = cp.Delete(ctx, claim)
			if karpcp.IsNodeClaimNotFoundError(err) {
				return true, nil
			}
			if err != nil {
				lastErr = err
			} else {
				deleteRequested = true
			}
		}
		// Keep polling through API errors, preserving the last one for timeout diagnostics.
		return false, nil
	})
	if err != nil {
		return fmt.Errorf("waiting for server %s to be deleted: %w", claim.Status.ProviderID, errors.Join(err, lastErr))
	}
	return nil
}

// envOr returns the environment variable or a default.
func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// hcloudID renders an hcloud image ID the way NodeClaim.Status.ImageID
// carries it.
func hcloudID(id int64) string {
	return strconv.FormatInt(id, 10)
}
