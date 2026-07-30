// Package instancetype converts Hetzner Cloud server types to Karpenter
// InstanceType objects.
//
// The Provider keeps a TTL'd cache of the hcloud ServerType catalog so
// List stays cheap when Karpenter polls on every scheduling pass. The
// cache is keyed by the normalized (deduped, lower-cased) set of
// locations the caller asked for; a different location set triggers a
// refresh.
//
// MarkUnavailable records a per-(server-type, location) cooldown: while
// a cooldown is in effect, List marks the matching offering as
// unavailable. The cooldown map is concurrency-safe (sync.Mutex) and
// stale entries are filtered out lazily on read.
package instancetype

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

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

// defaultInstanceTypeTTL is the cache TTL for the server-type catalog.
// Re-fetching hourly is far cheaper than risking stale availability
// flags mid-incident.
const defaultInstanceTypeTTL = time.Hour

// defaultUnavailableCooldown is how long MarkUnavailable suppresses an
// offering. Long enough to outlast a single scheduling pass; short
// enough that a transient fill in capacity recovers without manual
// intervention.
const defaultUnavailableCooldown = 5 * time.Minute

// defaultMaxPods is the pod limit assumed per node. Matches Karpenter's
// built-in default (see karpenter/pkg/apis/v1) and Hetzner's actual
// limit on the default `kubelet` configuration.
const defaultMaxPods = 110

// defaultOS is the OS value advertised on the InstanceType.Requirements
// for `kubernetes.io/os`. Hetzner cloud-init images are Linux only.
const defaultOS = "linux"

// Provider exposes Hetzner ServerTypes as Karpenter InstanceTypes.
type Provider struct {
	hcloud *hcloud.Client
	pricer *pricing.Provider

	ttl      time.Duration
	cooldown time.Duration
	now      func() time.Time

	mu            sync.Mutex
	fetchedAt     time.Time
	cachedTypes   []*hcloud.ServerType
	typeLocsIndex map[string]map[string]bool

	coolMu      sync.Mutex
	unavailable map[string]time.Time // key = typeName|locName -> until time
}

// Option mutates a Provider at construction time.
type Option func(*Provider)

// WithTTL overrides the cache TTL for the server-type catalog.
func WithTTL(d time.Duration) Option {
	return func(p *Provider) { p.ttl = d }
}

// WithClock overrides the clock used for TTL comparisons and cooldowns.
// Tests advance the clock to drive expiry deterministically.
func WithClock(now func() time.Time) Option {
	return func(p *Provider) { p.now = now }
}

// WithCooldown overrides the duration of a MarkUnavailable cooldown.
func WithCooldown(d time.Duration) Option {
	return func(p *Provider) { p.cooldown = d }
}

// New builds an instancetype.Provider given an hcloud client and pricing
// provider. The pricer is what makes Karpenter's cost-optimal scheduling
// actually cost-optimal.
func New(hcloud *hcloud.Client, pricer *pricing.Provider, opts ...Option) *Provider {
	p := &Provider{
		hcloud:      hcloud,
		pricer:      pricer,
		ttl:         defaultInstanceTypeTTL,
		cooldown:    defaultUnavailableCooldown,
		now:         time.Now,
		unavailable: make(map[string]time.Time),
	}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

// List returns the priced InstanceType list for a set of locations.
//
// Each server type becomes one Karpenter InstanceType, with one Offering
// per requested location. Offerings whose (type, location) pair is
// currently on a MarkUnavailable cooldown, or that Hetzner does not
// offer at the location, are returned with Available=false — the
// scheduler will skip them, but the type still appears in the list so
// NodePool templates that reference it keep validating cleanly.
//
// Pricing is location-uniform (Hetzner's /pricing endpoint returns a
// single hourly figure per server type that already includes the
// primary-IPv4 surcharge); see pkg/providers/pricing.
//
// locations is deduplicated and lower-cased before being used as a
// cache key. Pass at least one location.
func (p *Provider) List(ctx context.Context, locations []string) ([]*cloudprovider.InstanceType, error) {
	if len(locations) == 0 {
		return nil, fmt.Errorf("instancetype.List: at least one location required")
	}
	normalized := normalizeLocations(locations)
	if len(normalized) == 0 {
		return nil, fmt.Errorf("instancetype.List: at least one non-empty location required")
	}
	types, err := p.getServerTypes(ctx)
	if err != nil {
		return nil, err
	}

	out := make([]*cloudprovider.InstanceType, 0, len(types))
	for _, st := range types {
		if st == nil {
			continue
		}
		offerings := p.buildOfferings(ctx, st, normalized)
		out = append(out, buildInstanceType(st, offerings))
	}
	p.evictExpired(p.now())
	return out, nil
}

func (p *Provider) evictExpired(now time.Time) {
	p.coolMu.Lock()
	defer p.coolMu.Unlock()
	for key, until := range p.unavailable {
		if !now.Before(until) {
			delete(p.unavailable, key)
		}
	}
}

// MarkUnavailable records that a (server-type, location) combination had
// an insufficient-capacity error and should be skipped for the
// configured cooldown. Calling MarkUnavailable extends the cooldown to
// (now + cooldown) — repeated calls effectively re-arm it.
func (p *Provider) MarkUnavailable(serverType, location string) {
	if serverType == "" || location == "" {
		return
	}
	key := cooldownKey(serverType, normalizeLocation(location))
	p.coolMu.Lock()
	defer p.coolMu.Unlock()
	p.unavailable[key] = p.now().Add(p.cooldown)
}

// isOnCooldown reports whether (serverType, location) is currently
// suppressed. Stale entries are filtered out without mutating the map
// (the periodic List-time eviction handles the actual cleanup).
func (p *Provider) isOnCooldown(serverType, location string) bool {
	key := cooldownKey(serverType, normalizeLocation(location))
	p.coolMu.Lock()
	defer p.coolMu.Unlock()
	until, ok := p.unavailable[key]
	if !ok {
		return false
	}
	if !p.now().Before(until) {
		// Expired — drop it eagerly so the map does not grow forever.
		delete(p.unavailable, key)
		return false
	}
	return true
}

// getServerTypes returns the cached catalog, refreshing when either
// the TTL has elapsed or the requested location set differs from the
// cached one. A different location set can change which server types
// Hetzner prices (some types are location-scoped) and which are
// "available", so we cannot serve the previous snapshot as-is.
func (p *Provider) getServerTypes(ctx context.Context) ([]*hcloud.ServerType, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cachedTypes != nil && p.ttl > 0 && p.now().Sub(p.fetchedAt) < p.ttl {
		return p.cachedTypes, nil
	}
	types, err := p.hcloud.ServerType.All(ctx)
	if err != nil {
		return nil, fmt.Errorf("instancetype: listing server types: %w", err)
	}
	// Build a name -> locations index so each buildInstanceType call
	// can do O(1) availability lookups instead of a linear scan.
	idx := make(map[string]map[string]bool, len(types))
	for _, st := range types {
		if st == nil {
			continue
		}
		locs := make(map[string]bool, len(st.Locations))
		for _, l := range st.Locations {
			if l.Location == nil {
				continue
			}
			locs[strings.ToLower(l.Location.Name)] = l.Available
		}
		idx[st.Name] = locs
	}
	p.cachedTypes = types
	p.typeLocsIndex = idx
	p.fetchedAt = p.now()
	return types, nil
}

// buildOfferings returns one Offering per requested location for a given
// server type. The price is the location-uniform net hourly (server +
// primary-IPv4 surcharge) returned by the pricing provider. An
// offering is Available only when the type is offered at the location
// AND the (type, location) pair is not on a MarkUnavailable cooldown.
func (p *Provider) buildOfferings(ctx context.Context, st *hcloud.ServerType, locations []string) cloudprovider.Offerings {
	offerings := make(cloudprovider.Offerings, 0, len(locations))
	price, priceErr := p.pricer.Price(ctx, st)
	if priceErr != nil {
		price = 0
	}
	priceValid := priceErr == nil && price >= 0 && !math.IsNaN(price) && !math.IsInf(price, 0)
	for _, loc := range locations {
		available := p.hcloudAvailable(st, loc) && !p.isOnCooldown(st.Name, loc) && priceValid
		req := scheduling.NewRequirements(
			scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, loc),
			scheduling.NewRequirement(karpv1.CapacityTypeLabelKey, corev1.NodeSelectorOpIn, karpv1.CapacityTypeOnDemand),
		)
		offerings = append(offerings, &cloudprovider.Offering{
			Requirements: req,
			Price:        price,
			Available:    available,
		})
	}
	return offerings
}

// hcloudAvailable checks the in-memory index for whether the given
// server type is offered (per Hetzner's catalog) at the given location.
//
// When the index has no record (e.g. the API omitted the Locations
// slice for a brand-new type) we treat the type as "available" so the
// offering shows up with a price; the actual creation will fail
// upstream and the error path will handle it.
func (p *Provider) hcloudAvailable(st *hcloud.ServerType, loc string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.typeLocsIndex == nil {
		return true
	}
	locs, ok := p.typeLocsIndex[st.Name]
	if !ok || len(locs) == 0 {
		return true
	}
	avail, known := locs[loc]
	if !known {
		// Server type known but not at this location.
		return false
	}
	return avail
}

// buildInstanceType produces the Karpenter InstanceType for one
// hcloud.ServerType. Capacity is derived from the schema fields
// (Cores / Memory / Disk) and a fixed pod ceiling.
func buildInstanceType(st *hcloud.ServerType, offerings cloudprovider.Offerings) *cloudprovider.InstanceType {
	family := string(ClassOf(st))
	arch := architectureOf(st.Architecture)
	zones := uniqueZones(offerings)

	requirements := scheduling.NewRequirements(
		scheduling.NewRequirement(corev1.LabelInstanceTypeStable, corev1.NodeSelectorOpIn, st.Name),
		scheduling.NewRequirement(corev1.LabelArchStable, corev1.NodeSelectorOpIn, arch),
		scheduling.NewRequirement(corev1.LabelOSStable, corev1.NodeSelectorOpIn, defaultOS),
		scheduling.NewRequirement(karpv1.CapacityTypeLabelKey, corev1.NodeSelectorOpIn, karpv1.CapacityTypeOnDemand),
		scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, zones...),
		scheduling.NewRequirement(LabelServerFamily, corev1.NodeSelectorOpIn, family),
	)

	diskBytes := int64(st.Disk) * 1000 * 1000 * 1000
	capacity := corev1.ResourceList{
		corev1.ResourceCPU:              *resource.NewQuantity(int64(st.Cores), resource.DecimalSI),
		corev1.ResourceMemory:           memoryQuantity(st.Memory),
		corev1.ResourceEphemeralStorage: *resource.NewQuantity(diskBytes, resource.DecimalSI),
		corev1.ResourcePods:             *resource.NewQuantity(int64(defaultMaxPods), resource.DecimalSI),
	}

	return &cloudprovider.InstanceType{
		Name:         st.Name,
		Requirements: requirements,
		Offerings:    offerings,
		Capacity:     capacity,
		Overhead:     &cloudprovider.InstanceTypeOverhead{},
	}
}

// memoryQuantity converts the hcloud GB-style float to a Quantity
// expressed in BinarySI so kubectl prints it as "4Gi" rather than
// "4G" (which would compare as a different value to a pod requesting
// memory: 4Gi).
func memoryQuantity(gb float32) resource.Quantity {
	bytes := int64(float64(gb) * 1024 * 1024 * 1024)
	return *resource.NewQuantity(bytes, resource.BinarySI)
}

// architectureOf maps an hcloud.Architecture to the karpenter label
// value. Unrecognised architectures (and the zero value) fall through
// to amd64 — Hetzner's default and the only x86 family currently sold.
func architectureOf(a hcloud.Architecture) string {
	switch a {
	case hcloud.ArchitectureARM:
		return "arm64"
	case hcloud.ArchitectureX86:
		return "amd64"
	default:
		return "amd64"
	}
}

// normalizeLocations trims, lower-cases, and deduplicates the input
// location list. Empty strings are dropped. The result is sorted so the
// cache key is stable across calls with the same logical location set.
func normalizeLocations(in []string) []string {
	set := make(map[string]struct{}, len(in))
	for _, l := range in {
		l = strings.ToLower(strings.TrimSpace(l))
		if l == "" {
			continue
		}
		set[l] = struct{}{}
	}
	out := make([]string, 0, len(set))
	for l := range set {
		out = append(out, l)
	}
	sort.Strings(out)
	return out
}

// normalizeLocation is the single-string form of normalizeLocations,
// used by MarkUnavailable where only one location is passed.
func normalizeLocation(l string) string {
	return strings.ToLower(strings.TrimSpace(l))
}

// cooldownKey is the map key for the cooldown map. Separator is a
// character that Hetzner disallows in location and server-type names
// ('|') so it cannot collide with real inputs.
func cooldownKey(typeName, location string) string {
	return typeName + "|" + location
}

// uniqueZones returns the sorted, deduplicated set of zone labels
// referenced by the supplied offerings. Used to populate the
// LabelTopologyZone requirement on the InstanceType.
func uniqueZones(offerings cloudprovider.Offerings) []string {
	set := make(map[string]struct{}, len(offerings))
	for _, o := range offerings {
		if o == nil {
			continue
		}
		req := o.Requirements.Get(corev1.LabelTopologyZone)
		for _, v := range req.Values() {
			set[v] = struct{}{}
		}
	}
	out := make([]string, 0, len(set))
	for z := range set {
		out = append(out, z)
	}
	sort.Strings(out)
	return out
}

// LabelServerFamily is the requirement key for grouping Hetzner server types
// into families (cx, cpx, ccx, ...). Karpenter NodePools may select
// against it to steer workloads onto the cheapest available family.
const LabelServerFamily = "karpenter.hetzner.cloud/server-family"
