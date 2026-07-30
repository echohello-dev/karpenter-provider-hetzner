// Package imagefamily resolves an OS snapshot in Hetzner Cloud based on the
// NodeClass's ImageSelector. Supports Talos and Ubuntu families, picked by
// substring match against the snapshot description or by hcloud label
// selector (preferred — pin an exact snapshot, including baked extensions).
package imagefamily

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/hetznercloud/hcloud-go/v2/hcloud"

	apiv1 "github.com/echohello-dev/karpenter-provider-hetzner/v1/pkg/apis/v1"
)

// ResolvedImage is what imagefamily.Resolve returns: the snapshot plus the
// architecture it was built for, so the cloudprovider can fail loudly when
// the NodeClaim requests a different architecture (silent wrong-arch boots
// were the kind of bug that prompted drift detection to be additive).
type ResolvedImage struct {
	Image        *hcloud.Image
	Architecture hcloud.Architecture
}

// Provider implements image resolution against the Hetzner Cloud API.
type Provider struct {
	hcloud *hcloud.Client
}

// New constructs an imagefamily.Provider.
func New(hcloud *hcloud.Client) *Provider {
	return &Provider{hcloud: hcloud}
}

// Family is the canonical OS family name. The kubebuilder XValidation on
// ImageSelector limits Family to these two values at admission; we re-validate
// defensively so the provider never silently matches something else.
const (
	FamilyTalos  apiv1.ImageFamily = "talos"
	FamilyUbuntu apiv1.ImageFamily = "ubuntu"
)

// Resolve lists available images matching the ImageSelector for the given
// target architecture and returns the newest match.
//
// Strategy:
//  1. Server-side: filter by architecture and an optional hcloud label
//     selector so the hcloud API only returns the candidate set. All pages
//     are consumed (via Image.AllWithOpts) so resolution is not bounded by
//     the per-page limit.
//  2. Client-side: filter by ImageSelector.Family (substring match against
//     the description) and ImageSelector.Version (substring match against
//     the description), AND-combined. Substring matches are case-insensitive
//     so user-supplied families and versions do not have to chase the
//     upstream casing convention.
//  3. Defensive: skip deprecated, deleted, non-available, and wrong-arch
//     images even if the API returns them — silent wrong-arch boots are
//     exactly what drift detection is for.
//
// When multiple images match, the most recently created wins and ties
// (identical Created) are broken by descending image ID. Both fields are
// monotonically non-decreasing on the Hetzner side, so the order is
// deterministic across runs.
func (p *Provider) Resolve(ctx context.Context, sel apiv1.ImageSelector, arch hcloud.Architecture) (*ResolvedImage, error) {
	family, err := normalizeFamily(sel.Family)
	if err != nil {
		return nil, err
	}
	if err := validateArchitecture(arch); err != nil {
		return nil, err
	}

	opts := hcloud.ImageListOpts{
		Architecture: []hcloud.Architecture{arch},
		Type:         []hcloud.ImageType{hcloud.ImageTypeSnapshot},
		Status:       []hcloud.ImageStatus{hcloud.ImageStatusAvailable},
	}
	if labelSel := formatLabelSelector(sel.Selector); labelSel != "" {
		opts.LabelSelector = labelSel
	}

	images, err := p.hcloud.Image.AllWithOpts(ctx, opts)
	if err != nil {
		return nil, fmt.Errorf("imagefamily.Resolve: listing hcloud images (arch=%s, label_selector=%q): %w", arch, opts.LabelSelector, err)
	}

	version := strings.ToLower(strings.TrimSpace(sel.Version))
	matches := make([]*hcloud.Image, 0, len(images))
	for _, img := range images {
		if usableImage(img, arch, family, version) {
			matches = append(matches, img)
		}
	}

	if len(matches) == 0 {
		return nil, fmt.Errorf("imagefamily.Resolve: no matching image for family=%s arch=%s version=%q selector=%v", sel.Family, arch, sel.Version, sel.Selector)
	}

	sortImageSelection(matches)

	chosen := matches[0]
	if chosen.Architecture != arch {
		return nil, fmt.Errorf("imagefamily.Resolve: newest match has architecture %s but arch=%s was requested (image id=%d)", chosen.Architecture, arch, chosen.ID)
	}
	return &ResolvedImage{Image: chosen, Architecture: chosen.Architecture}, nil
}

// usableImage reports whether an image is selectable: available, not
// deprecated, not deleted, matching the requested architecture, and
// matching the family plus optional version substring. Each check is a
// "skip"; fewer checks means more matches.
func usableImage(img *hcloud.Image, arch hcloud.Architecture, family, version string) bool {
	if img == nil {
		return false
	}
	if img.Type != hcloud.ImageTypeSnapshot || img.IsDeprecated() || img.IsDeleted() {
		return false
	}
	if img.Status != hcloud.ImageStatusAvailable {
		return false
	}
	if img.Architecture != arch {
		return false
	}
	desc := strings.ToLower(img.Description)
	if !strings.Contains(desc, family) {
		return false
	}
	if version != "" && !strings.Contains(desc, version) {
		return false
	}
	return true
}

// sortImageSelection orders images newest-first. Empty Created is treated
// as the oldest possible timestamp so real images always win the tie.
func sortImageSelection(images []*hcloud.Image) {
	sort.Slice(images, func(i, j int) bool {
		ci, cj := images[i].Created, images[j].Created
		switch {
		case ci.IsZero() && cj.IsZero():
		case ci.IsZero():
			return false
		case cj.IsZero():
			return true
		case ci.Equal(cj):
		default:
			return ci.After(cj)
		}
		return images[i].ID > images[j].ID
	})
}

// normalizeFamily validates the family and returns the lowercased substring
// used for description matching.
func normalizeFamily(family apiv1.ImageFamily) (string, error) {
	if family == "" {
		return "", fmt.Errorf("imagefamily.Resolve: ImageSelector.Family is required")
	}
	switch family {
	case FamilyTalos, FamilyUbuntu:
		return strings.ToLower(string(family)), nil
	default:
		return "", fmt.Errorf("imagefamily.Resolve: unsupported family %q (want %q or %q)", family, FamilyTalos, FamilyUbuntu)
	}
}

// validateArchitecture rejects empty and unsupported architectures. The
// only two valid values are hcloud.ArchitectureX86 and hcloud.ArchitectureARM.
func validateArchitecture(arch hcloud.Architecture) error {
	if arch == "" {
		return fmt.Errorf("imagefamily.Resolve: architecture is required")
	}
	switch arch {
	case hcloud.ArchitectureX86, hcloud.ArchitectureARM:
		return nil
	default:
		return fmt.Errorf("imagefamily.Resolve: unsupported architecture %q (want %q or %q)", arch, hcloud.ArchitectureX86, hcloud.ArchitectureARM)
	}
}

// formatLabelSelector renders a kube-style equality selector with stable
// (alphabetical) key ordering so the request signature is deterministic.
// Keys are joined with "=" and pairs are joined with "," so the resulting
// string matches the hcloud API's label_selector format.
func formatLabelSelector(m map[string]string) string {
	if len(m) == 0 {
		return ""
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%s", k, m[k]))
	}
	return strings.Join(parts, ",")
}
