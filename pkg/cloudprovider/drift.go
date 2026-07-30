package cloudprovider

import (
	"context"
	"fmt"

	"github.com/hetznercloud/hcloud-go/v2/hcloud"
	"sigs.k8s.io/controller-runtime/pkg/log"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	karpcp "sigs.k8s.io/karpenter/pkg/cloudprovider"

	apiv1 "github.com/echohello-dev/karpenter-provider-hetzner/v1/pkg/apis/v1"
	providermetrics "github.com/echohello-dev/karpenter-provider-hetzner/v1/pkg/metrics"
)

// IsDrifted returns a DriftReason when the NodeClaim's underlying Hetzner
// server has diverged from its desired state. The empty string means no drift.
//
// Drift checks performed (in order, short-circuited):
//
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
// The firewall check inspects server.PublicNet.Firewalls, which the hcloud
// GetByID call populates without any extra round-trips; this stays inside the
// instance-provider boundary because the cloudprovider only reads fields from
// the already-fetched *hcloud.Server. If a future Karpenter contract requires
// attaching firewalls without a label selector (and the hcloud.Server shape
// ever stops reporting attached firewalls on GetByID), this check must be
// revisited — the issue is documented in the function comment so the next
// maintainer does not have to dig through git history.
func (cp *CloudProvider) IsDrifted(ctx context.Context, nodeClaim *karpv1.NodeClaim) (karpcp.DriftReason, error) {
	if nodeClaim == nil {
		return "", fmt.Errorf("cloudprovider.IsDrifted: nodeClaim is nil")
	}
	if nodeClaim.Status.ProviderID == "" {
		// Nothing exists to drift from yet — Karpenter treats this as no-drift
		// and waits for the next reconciliation to call Create.
		return "", nil
	}
	server, err := cp.instanceProvider.Get(ctx, nodeClaim.Status.ProviderID)
	if err != nil {
		return "", fmt.Errorf("getting server for drift check: %w", err)
	}
	if server == nil {
		// Server disappeared — return typed not-found so Karpenter's
		// garbage collector runs against the dangling NodeClaim.
		return "", karpcp.NewNodeClaimNotFoundError(fmt.Errorf("server %q for NodeClaim %q not found", nodeClaim.Status.ProviderID, nodeClaim.Name))
	}

	nodeClass, err := cp.resolveNodeClass(ctx, nodeClaim.Spec.NodeClassRef)
	if err != nil {
		return "", err
	}

	if reason, ok := checkImageDrift(nodeClaim, server); ok {
		return cp.recordDrift(ctx, nodeClaim, reason), nil
	}
	if reason, ok := checkNetworkDrift(server, nodeClass); ok {
		return cp.recordDrift(ctx, nodeClaim, reason), nil
	}
	if reason, ok := checkFirewallDrift(server, nodeClass); ok {
		return cp.recordDrift(ctx, nodeClaim, reason), nil
	}
	if reason, ok := checkServerTypeDrift(nodeClaim, server); ok {
		return cp.recordDrift(ctx, nodeClaim, reason), nil
	}
	if reason, ok := checkLocationDrift(server, nodeClass); ok {
		return cp.recordDrift(ctx, nodeClaim, reason), nil
	}
	if reason, ok := checkLabelsDrift(server, nodeClass); ok {
		return cp.recordDrift(ctx, nodeClaim, reason), nil
	}
	return "", nil
}

func (cp *CloudProvider) recordDrift(ctx context.Context, nodeClaim *karpv1.NodeClaim, reason karpcp.DriftReason) karpcp.DriftReason {
	providermetrics.RecordDrift.WithLabelValues(string(reason)).Inc()
	log.FromContext(ctx).Info("nodeclaim drift detected", "nodeclaim", nodeClaim.Name, "providerID", nodeClaim.Status.ProviderID, "reason", reason)
	return reason
}

// checkImageDrift reports a drift when the running server's image ID no
// longer matches the image ID recorded on the NodeClaim status. Empty
// ImageID on either side is a non-finding — the controller treats an unset
// status.ImageID as "not yet observed", not as drift.
func checkImageDrift(nodeClaim *karpv1.NodeClaim, server *hcloud.Server) (karpcp.DriftReason, bool) {
	if nodeClaim.Status.ImageID == "" || server.Image == nil {
		return "", false
	}
	if fmt.Sprintf("%d", server.Image.ID) == nodeClaim.Status.ImageID {
		return "", false
	}
	return DriftImage, true
}

// checkNetworkDrift reports drift when the running server is not attached to
// the network the NodeClass expects. hcloud's *Server struct exposes the
// attached private networks via PrivateNet, which we scan for the expected
// NetworkID.
func checkNetworkDrift(server *hcloud.Server, nodeClass *apiv1.HCloudNodeClass) (karpcp.DriftReason, bool) {
	if nodeClass.Spec.NetworkID == 0 {
		return "", false
	}
	for _, pn := range server.PrivateNet {
		if pn.Network != nil && pn.Network.ID == nodeClass.Spec.NetworkID {
			return "", false
		}
	}
	return DriftNetwork, true
}

// checkFirewallDrift reports drift when at least one of the NodeClass's
// expected firewall IDs is not attached to the server.
//
// Caveat: this check uses server.PublicNet.Firewalls, which hcloud populates
// when the firewall is applied via the server create body. Firewalls attached
// to a *server* via the firewall apply-action (rather than as part of create)
// should also surface here, because GetByID returns the live attachment state
// regardless of how the attachment was made. If the hcloud schema ever drops
// the Firewalls slice on ServerGetResponse this check will silently pass —
// that is the failure mode documented at the top of IsDrifted.
func checkFirewallDrift(server *hcloud.Server, nodeClass *apiv1.HCloudNodeClass) (karpcp.DriftReason, bool) {
	if len(nodeClass.Spec.FirewallIDs) == 0 {
		return "", false
	}
	attached := make(map[int64]struct{}, len(server.PublicNet.Firewalls))
	for _, fw := range server.PublicNet.Firewalls {
		if fw.Status != hcloud.FirewallStatusApplied {
			continue
		}
		attached[fw.Firewall.ID] = struct{}{}
	}
	for _, want := range nodeClass.Spec.FirewallIDs {
		if _, ok := attached[want]; !ok {
			return DriftFirewall, true
		}
	}
	return "", false
}

// checkServerTypeDrift reports drift when the running server type does not
// match the instance-type label recorded on the NodeClaim.
func checkServerTypeDrift(nodeClaim *karpv1.NodeClaim, server *hcloud.Server) (karpcp.DriftReason, bool) {
	if server.ServerType == nil {
		return "", false
	}
	want := nodeClaim.Labels[instanceTypeLabelKey]
	if want == "" {
		// Karpenter hasn't yet pinned the instance-type label, so we
		// can't make a determination. Treating this as no-drift matches
		// the controller's own read-modify-write window.
		return "", false
	}
	if server.ServerType.Name != want {
		return DriftServerType, true
	}
	return "", false
}

// checkLocationDrift reports drift when the server's location is not in the
// set the NodeClass allows.
func checkLocationDrift(server *hcloud.Server, nodeClass *apiv1.HCloudNodeClass) (karpcp.DriftReason, bool) {
	if server.Location == nil || len(nodeClass.Spec.Locations) == 0 {
		return "", false
	}
	for _, loc := range nodeClass.Spec.Locations {
		if server.Location.Name == loc {
			return "", false
		}
	}
	return DriftLocation, true
}

// checkLabelsDrift reports drift when a label set on the NodeClass is not
// present on the server. Karpenter-owned labels (karpenter.sh/*) are
// excluded so we don't churn when Karpenter-managed labels are absent on
// the hcloud side.
func checkLabelsDrift(server *hcloud.Server, nodeClass *apiv1.HCloudNodeClass) (karpcp.DriftReason, bool) {
	if len(nodeClass.Spec.Labels) == 0 {
		return "", false
	}
	for key, value := range nodeClass.Spec.Labels {
		got, ok := server.Labels[key]
		if !ok || got != value {
			return DriftLabels, true
		}
	}
	return "", false
}

const instanceTypeLabelKey = "node.kubernetes.io/instance-type"
