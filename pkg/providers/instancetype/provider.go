// Package instancetype converts Hetzner Cloud server types to Karpenter
// InstanceType objects.
package instancetype

import (
	"context"
	"fmt"
	"strings"

	"github.com/hetznercloud/hcloud-go/v2/hcloud"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"

	"github.com/echohello-dev/karpenter-provider-hetzner/v1/pkg/providers/pricing"
)

// Family categorizes a server into a Karpenter-selectable well-known group
// (similar to AWS' family attribute). Karpenter can match against
// karpenter.hetzner.cloud/server-family as a NodePool requirement.
type Family string

const (
	// FamilyCX is the shared Intel x86 family (cx22, cx32, ...).
	FamilyCX Family = "cx"
	// FamilyCPX is the shared Intel x86 newer family (cpx11, cpx21, ...).
	FamilyCPX Family = "cpx"
	// FamilyCCX is the dedicated Intel x86 family (ccx13, ccx23, ...).
	FamilyCCX Family = "ccx"
	// FamilyCAX is the shared Ampere ARM family (cax11, cax21, ...).
	FamilyCAX Family = "cax"
	// FamilyOther is the fallback for any server name that doesn't match a known prefix.
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

// LabelServerFamily is the requirement key for grouping Hetzner server types
// into families (cx, cpx, ccx, cax, ...). Karpenter NodePools may select
// against it to steer workloads onto the cheapest available family.
const LabelServerFamily = "karpenter.hetzner.cloud/server-family"
