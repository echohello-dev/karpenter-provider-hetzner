package e2e

import (
	"context"
	"strings"
	"testing"

	"github.com/awslabs/operatorpkg/status"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	karpcp "sigs.k8s.io/karpenter/pkg/cloudprovider"

	apiv1 "github.com/echohello-dev/karpenter-provider-hetzner/v1/pkg/apis/v1"
	"github.com/echohello-dev/karpenter-provider-hetzner/v1/pkg/cloudprovider"
	"github.com/echohello-dev/karpenter-provider-hetzner/v1/pkg/providers/instance"
)

// TestE2E_NodeClassAndNodeLifecycle is the flagship end-to-end scenario.
//
// It plays the full provisioning story against a real kube-apiserver and a
// stateful fake Hetzner Cloud API:
//
//  1. An HCloudNodeClass is created through the API server; the real
//     nodeclass controller (running under a real manager) resolves images
//     and dependencies and reports Ready=True with status.resolvedImages
//     populated.
//  2. CloudProvider.Create provisions a server for a NodeClaim using the
//     resolved image, asserting labels, ProviderID, capacity and
//     allocatable round-trip onto the NodeClaim.
//  3. CloudProvider.Get reads the live server back into a NodeClaim.
//  4. CloudProvider.IsDrifted reports no drift, then image drift when the
//     server's image changes underneath the NodeClaim.
//  5. CloudProvider.Delete terminates the server and the follow-up Get /
//     Delete calls honour the Karpenter NodeClaimNotFound contract.
func TestE2E_NodeClassAndNodeLifecycle(t *testing.T) {
	h := startHarness(t)
	ctx := context.Background()

	// --- Phase 1: NodeClass reconcile → Ready=True --------------------
	nc := h.createNodeClass(t, testNodeClass("e2e"), metav1.ConditionTrue)

	for _, condType := range []string{
		apiv1.ConditionTypeImagesReady,
		apiv1.ConditionTypeNetworkReady,
		apiv1.ConditionTypeResourcesReady,
		apiv1.ConditionTypeUserDataReady,
		status.ConditionReady,
	} {
		if c := mustCondition(t, nc, condType); c.Status != metav1.ConditionTrue {
			t.Fatalf("condition %s = %s (%s), want True", condType, c.Status, c.Reason)
		}
	}

	wantImages := map[string]int64{"amd64": 12, "arm64": 13}
	if len(nc.Status.ResolvedImages) != len(wantImages) {
		t.Fatalf("status.resolvedImages = %+v, want %d entries", nc.Status.ResolvedImages, len(wantImages))
	}
	for _, ri := range nc.Status.ResolvedImages {
		if want := wantImages[ri.Architecture]; ri.ImageID != want {
			t.Errorf("resolvedImages[%s] = %d, want %d (newest matching talos snapshot)", ri.Architecture, ri.ImageID, want)
		}
	}

	// --- Phase 2: CloudProvider.Create -------------------------------
	claim := newNodeClaim("e2e-claim", "e2e")
	created, err := h.cp.Create(ctx, claim)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if created == nil {
		t.Fatal("Create returned nil NodeClaim")
	}

	// The image the reconciler resolved is the image the node boots.
	if created.Status.ImageID != "12" {
		t.Errorf("Status.ImageID = %q, want 12 (from status.resolvedImages)", created.Status.ImageID)
	}
	if want := instance.FormatProviderID(4242); created.Status.ProviderID != want {
		t.Errorf("Status.ProviderID = %q, want %q", created.Status.ProviderID, want)
	}

	nodeClassLabel := karpv1.NodeClassLabelKey(schema.GroupKind{Group: apiv1.GroupVersion.Group, Kind: "HCloudNodeClass"})
	for key, want := range map[string]string{
		corev1.LabelInstanceTypeStable: "cx22", // cheapest type fitting the claim
		corev1.LabelTopologyZone:       "fsn1",
		karpv1.CapacityTypeLabelKey:    karpv1.CapacityTypeOnDemand,
		nodeClassLabel:                 "e2e",
		karpv1.NodePoolLabelKey:        "pool-a",
	} {
		if got := created.Labels[key]; got != want {
			t.Errorf("created %s, want %q", fmtLabel(created.Labels, key), want)
		}
	}

	mustQuantity(t, created.Status.Capacity, corev1.ResourceCPU, "2")
	mustQuantity(t, created.Status.Capacity, corev1.ResourceMemory, "4Gi")
	mustQuantity(t, created.Status.Capacity, corev1.ResourceEphemeralStorage, "40G")
	mustQuantity(t, created.Status.Capacity, corev1.ResourcePods, "110")
	// The fake catalog publishes no overhead, so allocatable == capacity.
	mustQuantity(t, created.Status.Allocatable, corev1.ResourceCPU, "2")
	mustQuantity(t, created.Status.Allocatable, corev1.ResourceMemory, "4Gi")

	// The Hetzner-side server carries the provider-owned labels and the
	// NodeClass-specified labels and user data.
	if h.fake.ServerCount() != 1 {
		t.Fatalf("expected exactly 1 server in the fake project, got %d", h.fake.ServerCount())
	}
	srv := h.fake.Servers()[0]
	for key, want := range map[string]string{
		"karpenter.sh/cluster":         clusterName,
		"karpenter.sh/nodeclaim":       "e2e-claim",
		"karpenter.sh/nodepool":        "pool-a",
		"karpenter.sh/hcloudnodeclass": "e2e",
		"team":                         "platform",
		corev1.LabelInstanceTypeStable: "cx22",
	} {
		if got := srv.Labels[key]; got != want {
			t.Errorf("server %s, want %q", fmtLabel(srv.Labels, key), want)
		}
	}
	// Regression guard: the live API rejects any server-label key containing
	// "hetzner.cloud" (reserved prefix), so none may ever be emitted.
	for key := range srv.Labels {
		if strings.Contains(key, "hetzner.cloud") {
			t.Errorf("server label key %q uses the reserved hetzner.cloud namespace", key)
		}
	}
	if srv.UserData != "#!e2e-scenario" {
		t.Errorf("server user_data = %q, want %q", srv.UserData, "#!e2e-scenario")
	}
	if srv.ServerType != "cx22" || srv.Location != "fsn1" || srv.ImageID != 12 {
		t.Errorf("server shape = type %q loc %q image %d, want cx22/fsn1/12", srv.ServerType, srv.Location, srv.ImageID)
	}
	if len(srv.NetworkIDs) != 1 || srv.NetworkIDs[0] != networkID {
		t.Errorf("server networks = %v, want [%d]", srv.NetworkIDs, networkID)
	}
	if len(srv.FirewallIDs) != 1 || srv.FirewallIDs[0] != firewallID {
		t.Errorf("server firewalls = %v, want [%d]", srv.FirewallIDs, firewallID)
	}

	// --- Phase 3: CloudProvider.Get ----------------------------------
	got, err := h.cp.Get(ctx, created.Status.ProviderID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status.ProviderID != created.Status.ProviderID {
		t.Errorf("Get ProviderID = %q, want %q", got.Status.ProviderID, created.Status.ProviderID)
	}
	if got.Labels[corev1.LabelInstanceTypeStable] != "cx22" {
		t.Errorf("Get instance-type label = %q, want cx22", got.Labels[corev1.LabelInstanceTypeStable])
	}
	if got.Labels[corev1.LabelTopologyZone] != "fsn1" {
		t.Errorf("Get zone label = %q, want fsn1", got.Labels[corev1.LabelTopologyZone])
	}
	if got.Labels[karpv1.CapacityTypeLabelKey] != karpv1.CapacityTypeOnDemand {
		t.Errorf("Get capacity-type label = %q, want %q", got.Labels[karpv1.CapacityTypeLabelKey], karpv1.CapacityTypeOnDemand)
	}
	if got.Labels[karpv1.NodePoolLabelKey] != "pool-a" {
		t.Errorf("Get nodepool label = %q, want pool-a", got.Labels[karpv1.NodePoolLabelKey])
	}
	mustQuantity(t, got.Status.Capacity, corev1.ResourceCPU, "2")
	mustQuantity(t, got.Status.Capacity, corev1.ResourceMemory, "4Gi")
	mustQuantity(t, got.Status.Capacity, corev1.ResourceEphemeralStorage, "40G")
	mustQuantity(t, got.Status.Capacity, corev1.ResourcePods, "110")

	// --- Phase 4: CloudProvider.IsDrifted ----------------------------
	reason, err := h.cp.IsDrifted(ctx, created)
	if err != nil {
		t.Fatalf("IsDrifted: %v", err)
	}
	if reason != "" {
		t.Fatalf("IsDrifted = %q on a freshly created server, want no drift", reason)
	}

	// Mutate the server's image underneath the NodeClaim → image drift.
	if err := h.fake.SetServerImage(srv.ID, 99); err != nil {
		t.Fatalf("injecting image drift: %v", err)
	}
	reason, err = h.cp.IsDrifted(ctx, created)
	if err != nil {
		t.Fatalf("IsDrifted after image change: %v", err)
	}
	if reason != cloudprovider.DriftImage {
		t.Fatalf("IsDrifted = %q after image change, want %q", reason, cloudprovider.DriftImage)
	}

	if err := h.fake.SetServerImage(srv.ID, 12); err != nil {
		t.Fatalf("restoring image: %v", err)
	}
	reason, err = h.cp.IsDrifted(ctx, created)
	if err != nil {
		t.Fatalf("IsDrifted after restore: %v", err)
	}
	if reason != "" {
		t.Fatalf("IsDrifted = %q after restore, want no drift", reason)
	}

	// --- Phase 5: CloudProvider.Delete -------------------------------
	if err := h.cp.Delete(ctx, created); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if h.fake.ServerCount() != 0 {
		t.Fatalf("expected 0 servers after delete, got %d", h.fake.ServerCount())
	}

	if _, err := h.cp.Get(ctx, created.Status.ProviderID); !karpcp.IsNodeClaimNotFoundError(err) {
		t.Fatalf("Get after delete: got %v, want NodeClaimNotFoundError", err)
	}
	if err := h.cp.Delete(ctx, created); !karpcp.IsNodeClaimNotFoundError(err) {
		t.Fatalf("Delete after delete: got %v, want NodeClaimNotFoundError", err)
	}
}

// TestE2E_NodeClassNotReadyBlocksCreate is the negative counterpart: a
// NodeClass the reconciler cannot make Ready (its network does not exist)
// must block provisioning with a typed NodeClassNotReadyError, proving the
// reconcile → cloudprovider guard works across the real API server.
func TestE2E_NodeClassNotReadyBlocksCreate(t *testing.T) {
	h := startHarness(t)
	ctx := context.Background()

	broken := testNodeClass("e2e-broken")
	broken.Spec.NetworkID = 5555 // not in the fake catalog
	nc := h.createNodeClass(t, broken, metav1.ConditionFalse)

	c := mustCondition(t, nc, apiv1.ConditionTypeNetworkReady)
	if c.Status != metav1.ConditionFalse || c.Reason != "NetworkNotFound" {
		t.Fatalf("NetworkReady = %s/%s, want False/NetworkNotFound", c.Status, c.Reason)
	}

	_, err := h.cp.Create(ctx, newNodeClaim("e2e-blocked", "e2e-broken"))
	if err == nil {
		t.Fatal("Create against a non-Ready NodeClass succeeded, want NodeClassNotReadyError")
	}
	if !karpcp.IsNodeClassNotReadyError(err) {
		t.Fatalf("Create error = %T: %v, want NodeClassNotReadyError", err, err)
	}
}
