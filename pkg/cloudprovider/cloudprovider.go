// Package cloudprovider implements the Karpenter CloudProvider interface for Hetzner Cloud.
package cloudprovider

import (
	"context"
	"fmt"
	"time"

	"github.com/awslabs/operatorpkg/status"
	"github.com/hetznercloud/hcloud-go/v2/hcloud"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	karpcp "sigs.k8s.io/karpenter/pkg/cloudprovider"

	apiv1 "github.com/echohello-dev/karpenter-provider-hetzner/v1/pkg/apis/v1"
	"github.com/echohello-dev/karpenter-provider-hetzner/v1/pkg/providers/imagefamily"
	"github.com/echohello-dev/karpenter-provider-hetzner/v1/pkg/providers/instance"
	"github.com/echohello-dev/karpenter-provider-hetzner/v1/pkg/providers/instancetype"
)

const providerName = "hetzner"

// Drift reasons for HCloud-specific drift detection. Each maps to a single
// Server attribute and produces a structured INFO log + a Prometheus counter
// when triggered.
const (
	DriftImage      karpcp.DriftReason = "ImageDrift"
	DriftNetwork    karpcp.DriftReason = "NetworkDrift"
	DriftFirewall   karpcp.DriftReason = "FirewallDrift"
	DriftServerType karpcp.DriftReason = "ServerTypeDrift"
	DriftLocation   karpcp.DriftReason = "LocationDrift"
	DriftLabels     karpcp.DriftReason = "LabelsDrift"
)

// CloudProvider implements karpcp.CloudProvider against the Hetzner Cloud API.
//
// It provisions, bin-packs, autosizes, and replaces Hetzner Cloud servers as
// Kubernetes nodes, picking the cost-optimal server type for the pending pods
// in a NodeClaim. HCloudNodeClass describes how a node should be built.
type CloudProvider struct {
	kubeClient       client.Client
	instanceProvider *instance.Provider
	typeProvider     *instancetype.Provider
	imageProvider    *imagefamily.Provider
}

// New constructs a CloudProvider from its sub-providers.
func New(
	kubeClient client.Client,
	instanceProvider *instance.Provider,
	typeProvider *instancetype.Provider,
	imageProvider *imagefamily.Provider,
) *CloudProvider {
	return &CloudProvider{
		kubeClient:       kubeClient,
		instanceProvider: instanceProvider,
		typeProvider:     typeProvider,
		imageProvider:    imageProvider,
	}
}

// Name returns the cloud provider identifier used in logs, metrics, and
// Karpenter-internal labels.
func (cp *CloudProvider) Name() string { return providerName }

// GetSupportedNodeClasses enumerates the CRD kinds this provider knows how to
// reconcile. Only HCloudNodeClass today; future releases could add more.
func (cp *CloudProvider) GetSupportedNodeClasses() []status.Object {
	return []status.Object{&apiv1.HCloudNodeClass{}}
}

// RepairPolicies describes how Karpenter should repair unhealthy nodes.
//
// A node that reports NotReady (false or unknown) for more than five minutes
// is treated as broken and Karpenter will replace it. The two entries cover
// both the false and unknown cases.
func (cp *CloudProvider) RepairPolicies() []karpcp.RepairPolicy {
	return []karpcp.RepairPolicy{
		{
			ConditionType:      corev1.NodeReady,
			ConditionStatus:    corev1.ConditionFalse,
			TolerationDuration: 5 * time.Minute,
		},
		{
			ConditionType:      corev1.NodeReady,
			ConditionStatus:    corev1.ConditionUnknown,
			TolerationDuration: 5 * time.Minute,
		},
	}
}

// Create provisions a new Hetzner server for the given NodeClaim, returning a
// hydrated NodeClaim with ProviderID, ImageID, and Capacity populated.
//
// TODO: implement cost-optimal instance type selection (filter instance types
// by NodeClaim requirements, pick cheapest compatible offering, resolve an
// image matching the required architecture, resolve userData from inline or
// referenced Secret, issue hcloud Server Create, hydrate NodeClaim).
func (cp *CloudProvider) Create(ctx context.Context, nodeClaim *karpv1.NodeClaim) (*karpv1.NodeClaim, error) {
	nodeClass, err := cp.resolveNodeClass(ctx, nodeClaim.Spec.NodeClassRef)
	if err != nil {
		return nil, fmt.Errorf("resolving node class: %w", err)
	}
	_ = nodeClass
	return nil, karpcp.NewInsufficientCapacityError(
		fmt.Errorf("cloudprovider.Create: not yet implemented (TODO: cost-optimal scheduling, image resolution, server creation)"),
	)
}

// Delete terminates the server backing the given NodeClaim.
//
// Idempotent: deleting a non-existent server is treated as success. The
// controller may call Delete repeatedly during disruption and consolidation.
func (cp *CloudProvider) Delete(ctx context.Context, nodeClaim *karpv1.NodeClaim) error {
	return fmt.Errorf("cloudprovider.Delete: not yet implemented (TODO: route to instance.Provider.Delete)")
}

// Get retrieves the NodeClaim corresponding to the given provider ID.
//
// Returns NodeClaimNotFoundError when the underlying server is gone, so
// Karpenter's garbage collector can clean up the dangling NodeClaim.
func (cp *CloudProvider) Get(ctx context.Context, providerID string) (*karpv1.NodeClaim, error) {
	return nil, karpcp.NewNodeClaimNotFoundError(fmt.Errorf("cloudprovider.Get: not yet implemented"))
}

// List retrieves all NodeClaims managed by this provider on Hetzner Cloud.
//
// Bounded by the hcloud label tag karpenter.sh/cluster=<CLUSTER_NAME>, so two
// clusters sharing a single Hetzner project can never see each other's
// servers.
func (cp *CloudProvider) List(ctx context.Context) ([]*karpv1.NodeClaim, error) {
	return []*karpv1.NodeClaim{}, fmt.Errorf("cloudprovider.List: not yet implemented")
}

// GetInstanceTypes returns the available instance types for the given NodePool.
//
// Karpenter calls this many times per second during scheduling; the
// implementation MUST be cheap. Caching the per-location instance type list
// for the lifetime of an instancetype.Provider is acceptable.
func (cp *CloudProvider) GetInstanceTypes(ctx context.Context, nodePool *karpv1.NodePool) ([]*karpcp.InstanceType, error) {
	_ = ctx
	_ = nodePool
	return nil, fmt.Errorf("cloudprovider.GetInstanceTypes: not yet implemented")
}

// resolveNodeClass fetches the HCloudNodeClass referenced by ref. Returns a
// descriptive error when ref is nil, the CRD is not installed, or the named
// resource does not exist.
func (cp *CloudProvider) resolveNodeClass(ctx context.Context, ref *karpv1.NodeClassReference) (*apiv1.HCloudNodeClass, error) {
	if ref == nil {
		return nil, fmt.Errorf("nodeClassRef is nil")
	}
	if ref.Group != apiv1.GroupVersion.Group {
		return nil, fmt.Errorf("nodeClassRef group %q is not supported by this provider (want %q)", ref.Group, apiv1.GroupVersion.Group)
	}
	nodeClass := &apiv1.HCloudNodeClass{}
	if err := cp.kubeClient.Get(ctx, types.NamespacedName{Name: ref.Name}, nodeClass); err != nil {
		return nil, fmt.Errorf("getting HCloudNodeClass %q: %w", ref.Name, err)
	}
	return nodeClass, nil
}

// resolveArchitecture picks an hcloud architecture from a Karpenter requirement
// (e.g. nodeSelector kubernetes.io/arch). Defaults to x86.
func resolveArchitecture(arch string) hcloud.Architecture {
	switch arch {
	case "arm64":
		return hcloud.ArchitectureARM
	case "amd64":
		return hcloud.ArchitectureX86
	default:
		return hcloud.ArchitectureX86
	}
}
