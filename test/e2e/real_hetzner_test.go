package e2e

import (
	"context"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/awslabs/operatorpkg/status"
	"github.com/hetznercloud/hcloud-go/v2/hcloud"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
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
// Optional knobs: E2E_SERVER_TYPE (default cx22), E2E_LOCATION (default
// fsn1), E2E_CLUSTER_NAME (default karpenter-e2e).
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
// is registered before any assertions so a failure cannot leak one.
func TestE2E_RealHetzner_Lifecycle(t *testing.T) {
	if os.Getenv("E2E") != "1" {
		t.Skip("opt-in test: set E2E=1 to run the real Hetzner Cloud path (see docs: Testing)")
	}
	token := os.Getenv("HCLOUD_TOKEN")
	if token == "" {
		t.Skip("HCLOUD_TOKEN not set; skipping the real Hetzner Cloud path")
	}

	location := envOr("E2E_LOCATION", "fsn1")
	typeName := envOr("E2E_SERVER_TYPE", "cx22")
	cluster := envOr("E2E_CLUSTER_NAME", "karpenter-e2e")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	client := hcloud.NewClient(hcloud.WithToken(token))

	// Pick a live boot image: newest snapshot in the account, falling
	// back to a well-known system image when the project has none.
	image := pickRealImage(t, ctx, client)

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
			ResolvedImages: []apiv1.ResolvedImage{{Architecture: "amd64", ImageID: image.ID}},
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

	claim := newNodeClaim("e2e-real-claim", "e2e-real")
	claim.Spec.Requirements = append(claim.Spec.Requirements, karpv1.NodeSelectorRequirementWithMinValues{
		Key:      corev1.LabelInstanceTypeStable,
		Operator: corev1.NodeSelectorOpIn,
		Values:   []string{typeName},
	})

	created, err := cp.Create(ctx, claim)
	if err != nil {
		t.Fatalf("Create against live Hetzner API: %v", err)
	}
	// Register clean-up before any assertion so a red test cannot leave a
	// billed server behind.
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cleanupCancel()
		if err := cp.Delete(cleanupCtx, created); err != nil && !karpcp.IsNodeClaimNotFoundError(err) {
			t.Logf("clean-up delete of %s: %v", created.Status.ProviderID, err)
		}
	})

	if created.Status.ProviderID == "" {
		t.Fatal("Create returned empty ProviderID")
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

	if err := cp.Delete(ctx, created); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := cp.Get(ctx, created.Status.ProviderID); !karpcp.IsNodeClaimNotFoundError(err) {
		t.Fatalf("Get after delete: got %v, want NodeClaimNotFoundError", err)
	}
}

// pickRealImage returns the newest available snapshot in the account, or a
// well-known system image when the project has no snapshots at all.
func pickRealImage(t *testing.T, ctx context.Context, client *hcloud.Client) *hcloud.Image {
	t.Helper()
	images, _, err := client.Image.List(ctx, hcloud.ImageListOpts{
		Type:   []hcloud.ImageType{hcloud.ImageTypeSnapshot},
		Status: []hcloud.ImageStatus{hcloud.ImageStatusAvailable},
		Sort:   []string{"created:desc"},
	})
	if err != nil {
		t.Fatalf("listing live snapshots: %v", err)
	}
	if len(images) > 0 {
		return images[0]
	}
	img, _, err := client.Image.Get(ctx, "ubuntu-24.04")
	if err != nil {
		t.Fatalf("fetching fallback image ubuntu-24.04: %v", err)
	}
	if img == nil {
		t.Fatal("account has no snapshots and no ubuntu-24.04 system image; set the account up with a boot image first")
	}
	return img
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
