// Package instancetype converts Hetzner Cloud server types to Karpenter
// InstanceType objects.
package instancetype

import (
	"context"
	"fmt"
	"strings"

	"github.com/hetznercloud/hcloud-go/v2/hcloud"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/scheduling"

	"github.com/echohello-dev/karpenter-provider-hetzner/v1/pkg/providers/pricing"
)

// Family categorizes a server into a Karpenter-selectable well-known group
// (similar to AWS' family attribute). Karpenter can match against
// karpenter.hetzner.cloud/server-family as a NodePool requirement.
type Family string

const (
	FamilyCX    Family = "cx"  // shared Intel x86
	FamilyCPX   Family = "cpx" // shared Intel x86, newer
	FamilyCCX   Family = "ccx" // dedicated Intel x86
	FamilyCAX   Family = "cax" // shared Ampere ARM
	FamilyOther Family = "other"
)

// ClassOf returns the Family for an hcloud.ServerType based on its prefix.
//
// hcloud names server types like "cx22", "cpx42", "ccx13", "cax21" — the
// prefix encodes the family. Anything outside the known prefixes maps to
// "other" so unrecognised types remain schedulable.
func ClassOf(st *hcloud.ServerType) Family {
	if st == nil {
		return FamilyOther
	}
	switch {
	case strings.HasPrefix(st.Name, "cax"):
		return FamilyCAX
	case strings.HasPrefix(st.Name, "ccx"):
		return FamilyCCX
	case strings.HasPrefix(st.Name, "cpx"):
		return FamilyCPX
	case strings.HasPrefix(st.Name, "cx"):
		return FamilyCX
	default:
		return FamilyOther
	}
}

// Provider exposes Hetzner ServerTypes as Karpenter InstanceTypes.
type Provider struct {
	hcloud *hcloud.Client
	pricer *pricing.Provider
}

// New builds an instancetype.Provider given an hcloud client and pricing
// provider. The pricer is what makes Karpenter's cost-optimal scheduling
// actually cost-optimal.
func New(hcloud *hcloud.Client, pricer *pricing.Provider) *Provider {
	return &Provider{hcloud: hcloud, pricer: pricer}
}

// List returns the priced InstanceType list for a set of locations.
//
// TODO: implement. Karpenter calls this on every scheduling pass, so the
// implementation MUST cache the per-location offering catalog for the
// lifetime of this Provider and refresh only on demand (TTL).
func (p *Provider) List(ctx context.Context, locations []string) ([]*cloudprovider.InstanceType, error) {
	_ = ctx
	_ = locations
	return nil, fmt.Errorf("instancetype.List: not yet implemented (TODO: list hcloud.ServerTypes, build karpenter.InstanceType per type+location+arch combination, attach pricing)")
}

// MarkUnavailable records that a (server-type, location) combination had an
// insufficient-capacity error and should be skipped for a short cooldown.
//
// TODO: implement using an in-memory cache with a per-entry TTL so the
// scheduler does not retry a sold-out combination on every pass.
func (p *Provider) MarkUnavailable(serverType, location string) {
	_ = serverType
	_ = location
}

// buildRequirements assembles the nodeSelector-style requirements Karpenter
// uses to filter instance types. Architecture, capacity type (on-demand),
// and the family label are stable; per-offering requirements (zone) are
// layered on top.
func buildRequirements(st *hcloud.ServerType, arch string) scheduling.Requirements {
	req := scheduling.NewRequirements(
		scheduling.NewRequirement(corev1.LabelArchStable, corev1.NodeSelectorOpIn, arch),
		scheduling.NewRequirement(corev1.LabelInstanceTypeStable, corev1.NodeSelectorOpIn, st.Name),
		scheduling.NewRequirement(LabelServerFamily, corev1.NodeSelectorOpIn, string(ClassOf(st))),
		scheduling.NewRequirement(karpv1.CapacityTypeLabelKey, corev1.NodeSelectorOpIn, karpenterOnDemand),
	)
	return req
}

// LabelServerFamily is the requirement key for grouping Hetzner server types
// into families (cx, cpx, ccx, cax, ...). Karpenter NodePools may select
// against it to steer workloads onto the cheapest available family.
const LabelServerFamily = "karpenter.hetzner.cloud/server-family"

// karpenterOnDemand mirrors karpenter.sh/capacity-type=spot|on-demand.
// Hetzner Cloud has no spot market, so we hard-code "on-demand".
const karpenterOnDemand = "on-demand"

// toCapacity builds the ResourceList for the per-server CPU/Memory/Disk/Pods
// capacities Karpenter reports on the NodeClaim.
func toCapacity(st *hcloud.ServerType) corev1.ResourceList {
	memBytes := int64(float64(st.Memory) * 1024 * 1024 * 1024) // hcloud.Memory is GB float
	diskBytes := int64(st.Disk) * 1024 * 1024 * 1024
	return corev1.ResourceList{
		corev1.ResourceCPU:              *resource.NewMilliQuantity(int64(st.Cores)*1000, resource.DecimalSI),
		corev1.ResourceMemory:           *resource.NewQuantity(memBytes, resource.BinarySI),
		corev1.ResourceEphemeralStorage: *resource.NewQuantity(diskBytes, resource.BinarySI),
		corev1.ResourcePods:             *resource.NewQuantity(110, resource.DecimalSI),
	}
}
