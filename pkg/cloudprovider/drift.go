package cloudprovider

import (
	"context"
	"fmt"

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
