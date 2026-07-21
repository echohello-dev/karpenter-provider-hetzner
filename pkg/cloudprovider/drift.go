package cloudprovider

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	karpcp "sigs.k8s.io/karpenter/pkg/cloudprovider"
)

// IsDrifted returns a DriftReason when the NodeClaim's underlying Hetzner
// server has diverged from its desired state. The empty string means no drift.
//
// Drift checks performed (in order, short-circuited):
//  1. Image — NodeClaim.Status.ImageID vs the server's current image ID.
//  2. Network — server attached to the NodeClass-spec network.
//  3. Firewall — every NodeClass firewall attached to the server (subset check).
//  4. ServerType — running type matches the NodeClaim's instance-type label.
//  5. Location — server location is in the NodeClass allowed locations.
//  6. Labels — NodeClass-spec labels are present on the server (subset check).
//
// SSH-key and user-data drift are intentionally NOT checked: Hetzner does not
// reliably expose applied SSH keys or user-data after create, so a comparison
// would produce false positives. They are omitted rather than faked.
//
// TODO: implement against instance.Provider.Get once that provider is wired.
func (cp *CloudProvider) IsDrifted(ctx context.Context, nodeClaim *karpv1.NodeClaim) (karpcp.DriftReason, error) {
	_ = ctx
	_ = nodeClaim
	return "", fmt.Errorf("cloudprovider.IsDrifted: not yet implemented (TODO: route to instance.Provider.Get + per-attribute drift checks)")
}

// helperCapacity builds a corev1.ResourceList for a server with the given
// cores/memoryGB/diskGB. Used while Get/List bodies are stubs to keep the
// package compiling. Delete once Get/List are implemented.
func helperCapacity(cores int, memoryGB int, diskGB int) corev1.ResourceList {
	memBytes := int64(memoryGB) * 1024 * 1024 * 1024
	diskBytes := int64(diskGB) * 1024 * 1024 * 1024
	return corev1.ResourceList{
		corev1.ResourceCPU:              *resource.NewMilliQuantity(int64(cores)*1000, resource.DecimalSI),
		corev1.ResourceMemory:           *resource.NewQuantity(memBytes, resource.BinarySI),
		corev1.ResourceEphemeralStorage: *resource.NewQuantity(diskBytes, resource.BinarySI),
		corev1.ResourcePods:             *resource.NewQuantity(110, resource.DecimalSI),
	}
}
