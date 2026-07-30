// Package cloudprovider implements the Karpenter CloudProvider interface for Hetzner Cloud.
package cloudprovider

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/awslabs/operatorpkg/status"
	"github.com/hetznercloud/hcloud-go/v2/hcloud"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	karpcp "sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/scheduling"
	"sigs.k8s.io/karpenter/pkg/utils/resources"

	apiv1 "github.com/echohello-dev/karpenter-provider-hetzner/v1/pkg/apis/v1"
	"github.com/echohello-dev/karpenter-provider-hetzner/v1/pkg/metrics"
	"github.com/echohello-dev/karpenter-provider-hetzner/v1/pkg/providers/imagefamily"
	"github.com/echohello-dev/karpenter-provider-hetzner/v1/pkg/providers/instance"
	"github.com/echohello-dev/karpenter-provider-hetzner/v1/pkg/providers/instancetype"
)

const (
	providerName            = "hetzner"
	hcloudNodeClaimLabelKey = "karpenter.sh/nodeclaim"

	operationCreate          = "create_instance"
	operationDelete          = "delete_instance"
	operationGet             = "get_instance"
	operationList            = "list_instances"
	operationGetInstanceType = "get_instance_types"

	outcomeOK    = "ok"
	outcomeError = "error"
)

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

// Create provisions a new Hetzner server for the given NodeClaim.
//
// The flow is:
//
//  1. Resolve and validate the referenced HCloudNodeClass (group+kind match,
//     not being deleted, Ready).
//  2. List Karpenter InstanceTypes for the NodeClass's allowed locations and
//     pick the cheapest AVAILABLE offering compatible with the NodeClaim's
//     requirements AND able to fit its requested resources.
//  3. Resolve the snapshot that matches the chosen instance type's
//     architecture and the NodeClass's ImageSelector.
//  4. Resolve user data: Secret-backed first, inline UserData as fallback.
//  5. Build the hcloud CreateOpts (nodeClass labels, network, firewalls, SSH
//     keys, public IP, placement) and call instance.Provider.Create.
//  6. Hydrate the returned NodeClaim with the chosen standard labels
//     (instance-type, zone, capacity-type, NodeClass label), the
//     provider-owned NodeClass label (for round-trip hydration), ProviderID,
//     ImageID, capacity and allocatable.
//
// Real hcloud insufficient-capacity errors are classified and translated into
// a karpcp.InsufficientCapacityError plus an instancetype.MarkUnavailable call
// so the next scheduling pass skips the offending (type, zone) pair.
func (cp *CloudProvider) Create(ctx context.Context, nodeClaim *karpv1.NodeClaim) (*karpv1.NodeClaim, error) {
	if nodeClaim == nil {
		err := fmt.Errorf("cloudprovider.Create: nodeClaim is nil")
		cp.recordOperation(operationCreate, err)
		return nil, err
	}
	nodeClass, err := cp.resolveNodeClass(ctx, nodeClaim.Spec.NodeClassRef)
	if err != nil {
		cp.recordOperation(operationCreate, err)
		return nil, err
	}
	if err := cp.validateNodeClassReady(nodeClass); err != nil {
		cp.recordOperation(operationCreate, err)
		return nil, err
	}

	instanceTypes, err := cp.typeProvider.List(ctx, nodeClass.Spec.Locations)
	if err != nil {
		err = fmt.Errorf("listing instance types for NodeClass %q: %w", nodeClass.Name, err)
		cp.recordOperation(operationCreate, err)
		return nil, err
	}

	pick, err := pickInstanceType(nodeClaim, instanceTypes)
	if err != nil {
		cp.recordOperation(operationCreate, err)
		return nil, err
	}

	image, err := cp.imageProvider.Resolve(ctx, nodeClass.Spec.ImageSelector, pick.architecture)
	if err != nil {
		err = fmt.Errorf("resolving image for NodeClass %q (arch=%s): %w", nodeClass.Name, pick.architecture, err)
		cp.recordOperation(operationCreate, err)
		return nil, err
	}

	userData, err := cp.resolveUserData(ctx, nodeClass)
	if err != nil {
		err = fmt.Errorf("resolving user data for NodeClass %q: %w", nodeClass.Name, err)
		cp.recordOperation(operationCreate, err)
		return nil, err
	}

	strategy := pickPlacementStrategy(nodeClass.Spec.PlacementGroupStrategy)
	createOpts := instance.CreateOpts{
		Name:                   pick.serverName(nodeClaim),
		ServerType:             pick.instanceType.Name,
		Location:               pick.zone,
		Image:                  image.Image,
		NetworkID:              nodeClass.Spec.NetworkID,
		FirewallIDs:            nodeClass.Spec.FirewallIDs,
		SSHKeyIDs:              nodeClass.Spec.SSHKeyIDs,
		Labels:                 buildServerLabels(nodeClaim, nodeClass, pick),
		UserData:               userData,
		NodeClaim:              nodeClaim.Name,
		NodePool:               nodeClaim.Labels[karpv1.NodePoolLabelKey],
		PlacementGroupStrategy: strategy,
		EnablePublicIPv4:       nodeClass.Spec.PublicIPv4Enabled(),
		EnablePublicIPv6:       nodeClass.Spec.PublicIPv6Enabled(),
	}

	server, err := cp.instanceProvider.Create(ctx, createOpts)
	if err != nil {
		if reason := classifyInsufficientCapacity(err); reason != nil {
			cp.typeProvider.MarkUnavailable(pick.instanceType.Name, pick.zone)
			wrapped := karpcp.NewInsufficientCapacityError(fmt.Errorf("insufficient capacity for type=%s zone=%s: %w", pick.instanceType.Name, pick.zone, reason))
			cp.recordOperation(operationCreate, wrapped)
			return nil, wrapped
		}
		cp.recordOperation(operationCreate, err)
		return nil, err
	}

	hydrated := hydrateNodeClaim(nodeClaim, nodeClass, pick, server, image)
	cp.recordOperation(operationCreate, nil)
	return hydrated, nil
}

// Delete terminates the server backing the given NodeClaim.
//
// Returns karpcp.NodeClaimNotFoundError when the underlying server is gone,
// as the Karpenter contract requires, so its reconciliation loop can finish.
// Detection is two-step: probe with Get first so a missing server surfaces
// as NodeClaimNotFound even when the instance provider's idempotent Delete
// would silently swallow the 404.
func (cp *CloudProvider) Delete(ctx context.Context, nodeClaim *karpv1.NodeClaim) error {
	if nodeClaim == nil {
		err := fmt.Errorf("cloudprovider.Delete: nodeClaim is nil")
		cp.recordOperation(operationDelete, err)
		return err
	}
	if nodeClaim.Status.ProviderID == "" {
		err := karpcp.NewNodeClaimNotFoundError(fmt.Errorf("nodeClaim %q has no providerID set", nodeClaim.Name))
		cp.recordOperation(operationDelete, err)
		return err
	}
	server, err := cp.instanceProvider.Get(ctx, nodeClaim.Status.ProviderID)
	if err != nil {
		if isHcloudNotFound(err) {
			wrapped := karpcp.NewNodeClaimNotFoundError(fmt.Errorf("server for nodeClaim %q not found: %w", nodeClaim.Name, err))
			cp.recordOperation(operationDelete, wrapped)
			return wrapped
		}
		wrapped := fmt.Errorf("getting server for nodeClaim %q: %w", nodeClaim.Name, err)
		cp.recordOperation(operationDelete, wrapped)
		return wrapped
	}
	if server == nil {
		err := karpcp.NewNodeClaimNotFoundError(fmt.Errorf("server for nodeClaim %q not found", nodeClaim.Name))
		cp.recordOperation(operationDelete, err)
		return err
	}
	if err := cp.instanceProvider.Delete(ctx, nodeClaim.Status.ProviderID); err != nil {
		wrapped := fmt.Errorf("deleting server for nodeClaim %q: %w", nodeClaim.Name, err)
		cp.recordOperation(operationDelete, wrapped)
		return wrapped
	}
	cp.recordOperation(operationDelete, nil)
	return nil
}

// Get retrieves the NodeClaim corresponding to the given provider ID.
//
// Translates the hcloud server into a hydrated NodeClaim using the persisted
// labels and server fields, returning karpcp.NodeClaimNotFoundError when the
// server has been deleted out from under us.
func (cp *CloudProvider) Get(ctx context.Context, providerID string) (*karpv1.NodeClaim, error) {
	server, err := cp.instanceProvider.Get(ctx, providerID)
	if err != nil {
		cp.recordOperation(operationGet, err)
		return nil, err
	}
	if server == nil {
		err := karpcp.NewNodeClaimNotFoundError(fmt.Errorf("server %q not found", providerID))
		cp.recordOperation(operationGet, err)
		return nil, err
	}
	cp.recordOperation(operationGet, nil)
	return serverToNodeClaim(server), nil
}

// List retrieves all NodeClaims managed by this provider on Hetzner Cloud.
//
// Scoped by the karpenter.sh/cluster=<CLUSTER_NAME> label so two clusters
// sharing a single Hetzner project can never see each other's servers.
func (cp *CloudProvider) List(ctx context.Context) ([]*karpv1.NodeClaim, error) {
	servers, err := cp.instanceProvider.List(ctx)
	if err != nil {
		cp.recordOperation(operationList, err)
		return nil, err
	}
	out := make([]*karpv1.NodeClaim, 0, len(servers))
	for _, server := range servers {
		if server == nil {
			continue
		}
		out = append(out, serverToNodeClaim(server))
	}
	cp.recordOperation(operationList, nil)
	return out, nil
}

// GetInstanceTypes returns the available instance types for the given NodePool.
//
// Resolves the NodeClass that backs the NodePool and asks the instancetype
// provider for a Karpenter-shaped list of types whose offerings cover the
// NodeClass's allowed locations. Returning an empty list when the NodeClass
// reference is missing is intentional: an unresolved NodeClass should not
// crash Karpenter's scheduling loop.
func (cp *CloudProvider) GetInstanceTypes(ctx context.Context, nodePool *karpv1.NodePool) ([]*karpcp.InstanceType, error) {
	nodeClass, err := cp.resolveNodeClassFromPool(ctx, nodePool)
	if err != nil {
		if errors.Is(err, errForeignNodePool) {
			cp.recordOperation(operationGetInstanceType, nil)
			return []*karpcp.InstanceType{}, nil
		}
		cp.recordOperation(operationGetInstanceType, err)
		return nil, err
	}
	if err := cp.validateNodeClassReady(nodeClass); err != nil {
		cp.recordOperation(operationGetInstanceType, err)
		return nil, err
	}
	its, err := cp.typeProvider.List(ctx, nodeClass.Spec.Locations)
	if err != nil {
		cp.recordOperation(operationGetInstanceType, err)
		return nil, err
	}
	cp.recordOperation(operationGetInstanceType, nil)
	return its, nil
}

// errForeignNodePool is the sentinel returned when a NodePool is not backed
// by an HCloudNodeClass. Karpenter may hand us pools for other providers;
// we silently skip those and the caller (GetInstanceTypes) translates the
// sentinel into an empty list.
var errForeignNodePool = errors.New("nodePool is not backed by an HCloudNodeClass")

// resolveNodeClass fetches the HCloudNodeClass referenced by ref. Validates
// the group + kind (so a NodeClaim pointing at an unrelated NodeClass kind
// is rejected before we hit the API), then resolves the named resource.
// Returns a descriptive error when ref is nil, the group/kind does not match
// this provider, the CRD is not installed, or the named resource does not
// exist.
func (cp *CloudProvider) resolveNodeClass(ctx context.Context, ref *karpv1.NodeClassReference) (*apiv1.HCloudNodeClass, error) {
	if ref == nil {
		return nil, fmt.Errorf("nodeClassRef is nil")
	}
	if ref.Group != apiv1.GroupVersion.Group {
		return nil, fmt.Errorf("nodeClassRef group %q is not supported by this provider (want %q)", ref.Group, apiv1.GroupVersion.Group)
	}
	if ref.Kind != "HCloudNodeClass" {
		return nil, fmt.Errorf("nodeClassRef kind %q is not supported by this provider (want %q)", ref.Kind, "HCloudNodeClass")
	}
	nodeClass := &apiv1.HCloudNodeClass{}
	if err := cp.kubeClient.Get(ctx, types.NamespacedName{Name: ref.Name}, nodeClass); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, karpcp.NewNodeClassNotReadyError(fmt.Errorf("HCloudNodeClass %q not found: %w", ref.Name, err))
		}
		return nil, fmt.Errorf("getting HCloudNodeClass %q: %w", ref.Name, err)
	}
	return nodeClass, nil
}

// resolveNodeClassFromPool walks a NodePool's nodeClassRef to a usable
// HCloudNodeClass. Returns (nil, errForeignNodePool) when the NodePool does
// not reference our NodeClass kind — Karpenter may pass pools backed by
// other providers into the call, and we should silently skip those.
func (cp *CloudProvider) resolveNodeClassFromPool(ctx context.Context, nodePool *karpv1.NodePool) (*apiv1.HCloudNodeClass, error) {
	if nodePool == nil || nodePool.Spec.Template.Spec.NodeClassRef == nil {
		return nil, errForeignNodePool
	}
	ref := nodePool.Spec.Template.Spec.NodeClassRef
	if ref.Group != apiv1.GroupVersion.Group || ref.Kind != "HCloudNodeClass" {
		return nil, errForeignNodePool
	}
	nodeClass, err := cp.resolveNodeClass(ctx, ref)
	if err != nil {
		return nil, err
	}
	return nodeClass, nil
}

// validateNodeClassReady refuses to provision against a NodeClass that is
// being deleted (DeletionTimestamp set) or has not reached Ready=True. The
// second check is best-effort: a NodeClass whose conditions have not been
// aggregated yet is treated as ready so we do not deadlock reconciliation.
func (cp *CloudProvider) validateNodeClassReady(nodeClass *apiv1.HCloudNodeClass) error {
	if nodeClass == nil {
		return fmt.Errorf("nodeClass is nil")
	}
	if !nodeClass.DeletionTimestamp.IsZero() {
		return karpcp.NewNodeClassNotReadyError(fmt.Errorf("HCloudNodeClass %q is being deleted", nodeClass.Name))
	}
	ready := nodeClass.StatusConditions().Get(status.ConditionReady)
	if ready == nil || !ready.IsTrue() {
		message := "Ready condition has not been reported"
		if ready != nil && ready.Message != "" {
			message = ready.Message
		}
		return karpcp.NewNodeClassNotReadyError(fmt.Errorf("HCloudNodeClass %q is not Ready: %s", nodeClass.Name, message))
	}
	return nil
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

// pick is the resolved (instanceType, zone, architecture) triple that Create
// uses to issue the hcloud server. zone is the topology label of the chosen
// offering, which is also the hcloud location name.
type pick struct {
	instanceType *karpcp.InstanceType
	offering     *karpcp.Offering
	zone         string
	architecture hcloud.Architecture
}

// pickInstanceType finds the cheapest AVAILABLE Karpenter InstanceType whose
// offerings are compatible with the NodeClaim's requirements AND whose
// allocatable resources cover the NodeClaim's requested resources. Returns an
// InsufficientCapacityError when no instance type can satisfy both
// constraints — the scheduler will keep retrying on transient capacity
// pressure, but a hard miss means the NodeClaim's requirements and the
// cluster's hardware catalog genuinely disagree.
func pickInstanceType(nodeClaim *karpv1.NodeClaim, instanceTypes []*karpcp.InstanceType) (*pick, error) {
	if len(instanceTypes) == 0 {
		return nil, karpcp.NewInsufficientCapacityError(fmt.Errorf("no instance types available for NodeClaim %q", nodeClaim.Name))
	}
	reqs := scheduling.NewNodeSelectorRequirementsWithMinValues(nodeClaim.Spec.Requirements...)
	if !reqs.Has(instancetype.LabelServerFamily) {
		reqs.Add(scheduling.NewRequirement(instancetype.LabelServerFamily, corev1.NodeSelectorOpExists))
	}
	requested := nodeClaim.Spec.Resources.Requests
	ordered := karpcp.InstanceTypes(instanceTypes).OrderByPrice(reqs)
	for _, it := range ordered {
		if it == nil || !reqs.IsCompatible(it.Requirements, scheduling.AllowUndefinedWellKnownLabels) {
			continue
		}
		if !resources.Fits(requested, allocatableFor(it)) {
			continue
		}
		compatible := it.Offerings.Available().Compatible(reqs)
		onDemand := make(karpcp.Offerings, 0, len(compatible))
		for _, offering := range compatible {
			if offering.CapacityType() == karpv1.CapacityTypeOnDemand {
				onDemand = append(onDemand, offering)
			}
		}
		offering := onDemand.Cheapest()
		if offering == nil {
			continue
		}
		arch := resolveArchitecture(it.Requirements.Get(corev1.LabelArchStable).Any())
		return &pick{instanceType: it, offering: offering, zone: offering.Zone(), architecture: arch}, nil
	}
	return nil, karpcp.NewInsufficientCapacityError(fmt.Errorf("no instance type fits NodeClaim %q requirements and resources", nodeClaim.Name))
}

// allocatableFor returns the Karpenter InstanceType's allocatable resources
// but tolerates a nil Overhead. The instancetype provider in this repo does
// not currently populate InstanceType.Overhead, and the upstream
// Allocatable() helper panics on a nil Overhead — we fall back to Capacity
// in that case so the pick path stays usable. When Overhead is added to the
// instancetype provider, this helper becomes a one-liner.
func allocatableFor(it *karpcp.InstanceType) corev1.ResourceList {
	if it == nil {
		return corev1.ResourceList{}
	}
	if it.Overhead == nil {
		return it.Capacity
	}
	return it.Allocatable()
}

// serverName returns the deterministic name to give the hcloud server. The
// instance provider already prepends a "karpenter-" prefix and a sha-derived
// suffix — using NodeClaim.Name as the body keeps the server traceable back
// to the NodeClaim without overloading the label system.
func (p *pick) serverName(nodeClaim *karpv1.NodeClaim) string {
	return fmt.Sprintf("karpenter-%s", nodeClaim.Name)
}

// pickPlacementStrategy maps the user-facing placement strategy onto the
// instance package's typed value. Empty defaults to spread.
func pickPlacementStrategy(s apiv1.PlacementGroupStrategy) instance.PlacementGroupStrategy {
	if s == "" {
		return "spread"
	}
	return instance.PlacementGroupStrategy(s)
}

// buildServerLabels composes the labels applied to the hcloud server.
// Provider-owned labels (NodeClass, cluster, NodeClaim, NodePool) are added
// here so the round-trip hydration path can use them.
func buildServerLabels(nodeClaim *karpv1.NodeClaim, nodeClass *apiv1.HCloudNodeClass, picks ...*pick) map[string]string {
	var selected *pick
	if len(picks) > 0 {
		selected = picks[0]
	}
	labels := make(map[string]string, len(nodeClass.Spec.Labels)+8)
	for key, value := range nodeClass.Spec.Labels {
		labels[key] = value
	}
	for _, key := range []string{
		corev1.LabelArchStable,
		corev1.LabelOSStable,
		corev1.LabelInstanceTypeStable,
		corev1.LabelTopologyZone,
		karpv1.CapacityTypeLabelKey,
		instancetype.LabelServerFamily,
	} {
		if value := nodeClaim.Labels[key]; value != "" {
			labels[key] = value
		}
	}
	for key, value := range resolvedLabels(selected) {
		labels[key] = value
	}
	if value := nodeClaim.Labels[karpv1.NodePoolLabelKey]; value != "" {
		labels[karpv1.NodePoolLabelKey] = value
	}
	labels[karpv1.NodeClassLabelKey(nodeClassGroupKind())] = nodeClass.Name
	return labels
}

func resolvedLabels(selected *pick) map[string]string {
	labels := map[string]string{}
	if selected == nil || selected.instanceType == nil || selected.offering == nil {
		return labels
	}
	for key, requirement := range selected.instanceType.Requirements {
		values := requirement.Values()
		if len(values) == 1 {
			labels[key] = values[0]
		}
	}
	for key, requirement := range selected.offering.Requirements {
		values := requirement.Values()
		if len(values) == 1 {
			labels[key] = values[0]
		}
	}
	labels[corev1.LabelInstanceTypeStable] = selected.instanceType.Name
	labels[corev1.LabelTopologyZone] = selected.zone
	labels[karpv1.CapacityTypeLabelKey] = karpv1.CapacityTypeOnDemand
	return labels
}

// nodeClassGroupKind returns the GroupKind of the HCloudNodeClass CRD. The
// shape is fixed by the CRD schema in pkg/apis/v1 so this can be a constant
// helper without consulting the runtime object.
func nodeClassGroupKind() schema.GroupKind {
	return schema.GroupKind{Group: apiv1.GroupVersion.Group, Kind: "HCloudNodeClass"}
}

// hydrateNodeClaim populates the standard Karpenter-expected NodeClaim fields
// after a successful hcloud server create. The returned NodeClaim is a deep
// copy of the input, so callers can safely mutate it.
func hydrateNodeClaim(nodeClaim *karpv1.NodeClaim, nodeClass *apiv1.HCloudNodeClass, p *pick, server *hcloud.Server, image *imagefamily.ResolvedImage) *karpv1.NodeClaim {
	out := nodeClaim.DeepCopy()
	if out.Labels == nil {
		out.Labels = map[string]string{}
	}
	for key, value := range resolvedLabels(p) {
		out.Labels[key] = value
	}
	out.Labels[karpv1.NodeClassLabelKey(nodeClassGroupKind())] = nodeClass.Name

	if server != nil {
		out.Status.ProviderID = instance.FormatProviderID(server.ID)
	}
	if image != nil && image.Image != nil {
		out.Status.ImageID = fmt.Sprintf("%d", image.Image.ID)
	}
	if p.instanceType != nil {
		out.Status.Capacity = stripZeroQuantities(p.instanceType.Capacity)
		out.Status.Allocatable = stripZeroQuantities(allocatableFor(p.instanceType))
	}
	return out
}

// stripZeroQuantities returns a copy of the resource list with all zero
// quantities removed. Karpenter's CloudProvider.Create contract calls for
// capacity/allocatable to reflect only what the node actually has, so
// resources the InstanceType advertises at zero should not surface on the
// NodeClaim.
func stripZeroQuantities(in corev1.ResourceList) corev1.ResourceList {
	out := corev1.ResourceList{}
	for k, v := range in {
		if v.IsZero() {
			continue
		}
		if v.Cmp(resource.Quantity{}) == 0 {
			continue
		}
		out[k] = v.DeepCopy()
	}
	return out
}

// serverToNodeClaim hydrates a NodeClaim from a live hcloud server. The
// returned NodeClaim reflects what the cloudprovider can know about the
// server without going back to the API server — NodePool labels come from
// the server's persisted label set, capacity comes from the hcloud server
// type attached to the server, and ProviderID / ImageID come straight off
// the server struct.
func serverToNodeClaim(server *hcloud.Server) *karpv1.NodeClaim {
	if server == nil {
		return nil
	}
	out := &karpv1.NodeClaim{}
	out.Name = server.Labels[hcloudNodeClaimLabelKey]
	if out.Name == "" {
		out.Name = server.Name
	}
	out.Labels = map[string]string{}
	for _, key := range []string{
		karpv1.NodePoolLabelKey,
		karpv1.NodeClassLabelKey(nodeClassGroupKind()),
		corev1.LabelArchStable,
		corev1.LabelOSStable,
		corev1.LabelInstanceTypeStable,
		corev1.LabelTopologyZone,
		karpv1.CapacityTypeLabelKey,
		instancetype.LabelServerFamily,
	} {
		if value := server.Labels[key]; value != "" {
			out.Labels[key] = value
		}
	}
	if server.ServerType != nil {
		out.Labels[corev1.LabelInstanceTypeStable] = server.ServerType.Name
		out.Labels[corev1.LabelArchStable] = architectureLabel(server.ServerType.Architecture)
		out.Labels[corev1.LabelOSStable] = "linux"
		out.Labels[instancetype.LabelServerFamily] = string(instancetype.ClassOf(server.ServerType))
	}
	if server.Location != nil {
		out.Labels[corev1.LabelTopologyZone] = server.Location.Name
	}
	out.Labels[karpv1.CapacityTypeLabelKey] = karpv1.CapacityTypeOnDemand
	if nodeClass := out.Labels[karpv1.NodeClassLabelKey(nodeClassGroupKind())]; nodeClass != "" {
		out.Spec.NodeClassRef = &karpv1.NodeClassReference{
			Group: apiv1.GroupVersion.Group,
			Kind:  "HCloudNodeClass",
			Name:  nodeClass,
		}
	}
	out.Status.ProviderID = instance.FormatProviderID(server.ID)
	if server.Image != nil {
		out.Status.ImageID = fmt.Sprintf("%d", server.Image.ID)
	}
	if server.ServerType != nil {
		out.Status.Capacity = serverTypeToCapacity(server.ServerType)
		out.Status.Allocatable = serverTypeToCapacity(server.ServerType)
	}
	return out
}

func architectureLabel(architecture hcloud.Architecture) string {
	if architecture == hcloud.ArchitectureARM {
		return "arm64"
	}
	return "amd64"
}

// serverTypeToCapacity translates an hcloud ServerType into a Karpenter
// ResourceList. Mirrors the values the instancetype provider publishes so
// the round-trip Get/List path reports the same capacity the schedule-time
// pick did.
func serverTypeToCapacity(st *hcloud.ServerType) corev1.ResourceList {
	if st == nil {
		return corev1.ResourceList{}
	}
	memBytes := int64(float64(st.Memory) * 1024 * 1024 * 1024)
	diskBytes := int64(st.Disk) * 1000 * 1000 * 1000
	return corev1.ResourceList{
		corev1.ResourceCPU:              *resource.NewQuantity(int64(st.Cores), resource.DecimalSI),
		corev1.ResourceMemory:           *resource.NewQuantity(memBytes, resource.BinarySI),
		corev1.ResourceEphemeralStorage: *resource.NewQuantity(diskBytes, resource.DecimalSI),
		corev1.ResourcePods:             *resource.NewQuantity(110, resource.DecimalSI),
	}
}

// isHcloudNotFound reports whether err originated from an hcloud 404. We use
// the typed error code to avoid string-matching the message.
func isHcloudNotFound(err error) bool {
	if err == nil {
		return false
	}
	var apiErr hcloud.Error
	if errors.As(err, &apiErr) {
		return apiErr.Code == hcloud.ErrorCodeNotFound
	}
	return false
}

// classifyInsufficientCapacity inspects err for known hcloud "we have no
// hardware for that" error codes and returns a human-readable reason when
// one is recognised. Returns "" when the error is not a capacity-class error
// so callers don't translate unrelated failures.
func classifyInsufficientCapacity(err error) error {
	if err == nil {
		return nil
	}
	var apiErr hcloud.Error
	if !errors.As(err, &apiErr) {
		return nil
	}
	switch apiErr.Code {
	case hcloud.ErrorCodeResourceUnavailable,
		hcloud.ErrorCodeResourceLimitExceeded,
		hcloud.ErrorCodePlacementError,
		hcloud.ErrorCodeNoSpaceLeftInLocation:
		return fmt.Errorf("%s: %s", apiErr.Code, apiErr.Message)
	}
	return nil
}

// resolveUserData returns the user-data blob to pass to hcloud Server.Create.
// Secret reference takes precedence over inline UserData per the NodeClass
// contract.
func (cp *CloudProvider) resolveUserData(ctx context.Context, nodeClass *apiv1.HCloudNodeClass) (string, error) {
	if nodeClass.Spec.UserDataSecretRef != nil {
		ref := nodeClass.Spec.UserDataSecretRef
		if ref.Namespace == "" || ref.Name == "" || ref.Key == "" {
			return "", fmt.Errorf("userDataSecretRef requires namespace, name, and key")
		}
		secret := &corev1.Secret{}
		nn := types.NamespacedName{Namespace: ref.Namespace, Name: ref.Name}
		if err := cp.kubeClient.Get(ctx, nn, secret); err != nil {
			return "", fmt.Errorf("getting user-data secret %s/%s: %w", ref.Namespace, ref.Name, err)
		}
		value, ok := secret.Data[ref.Key]
		if !ok {
			return "", fmt.Errorf("user-data secret %s/%s has no key %q", ref.Namespace, ref.Name, ref.Key)
		}
		if len(value) == 0 {
			return "", fmt.Errorf("user-data secret %s/%s key %q is empty", ref.Namespace, ref.Name, ref.Key)
		}
		return string(value), nil
	}
	return nodeClass.Spec.UserData, nil
}

// recordOperation bumps the per-operation Prometheus counter with an outcome
// label. It is the single seam between the lifecycle methods and the
// metrics package so future call sites can instrument other methods the
// same way without scattering label-string literals across the package.
func (cp *CloudProvider) recordOperation(operation string, err error) {
	outcome := outcomeOK
	if err != nil {
		outcome = outcomeError
	}
	metrics.RecordOperation.WithLabelValues(operation, outcome).Inc()
}
