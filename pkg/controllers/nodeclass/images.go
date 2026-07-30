package nodeclass

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/hetznercloud/hcloud-go/v2/hcloud"

	apiv1 "github.com/echohello-dev/karpenter-provider-hetzner/v1/pkg/apis/v1"
)

// supportedArchitectures is the set of architectures the controller resolves
// images for. Iteration order is fixed (amd64 first, arm64 second) so the
// resulting ResolvedImages slice is stable across reconciles.
var supportedArchitectures = []hcloud.Architecture{
	hcloud.ArchitectureX86,
	hcloud.ArchitectureARM,
}

// reconcileImages resolves one snapshot image per supported architecture for
// the NodeClass's ImageSelector. On success it writes ResolvedImages and
// sets ImagesReady=True. On any per-architecture failure it sets
// ImagesReady=False with a reason that names the offending architecture and
// clears ResolvedImages so stale entries from a previous reconcile do not
// linger on the resource.
func (r *Reconciler) reconcileImages(ctx context.Context, nc *apiv1.HCloudNodeClass) {
	cs := nc.StatusConditions()
	if nc.Spec.ImageSelector.Family != "talos" && nc.Spec.ImageSelector.Family != "ubuntu" {
		cs.SetFalse(apiv1.ConditionTypeImagesReady, "ImageSelectorInvalid", fmt.Sprintf("unsupported image family %q", nc.Spec.ImageSelector.Family))
		nc.Status.ResolvedImages = nil
		return
	}

	resolved := make([]apiv1.ResolvedImage, 0, len(supportedArchitectures))
	for _, arch := range supportedArchitectures {
		img, err := r.resolveImage(ctx, nc.Spec.ImageSelector, arch)
		if err != nil {
			cs.SetFalse(
				apiv1.ConditionTypeImagesReady,
				"ImageResolutionFailed",
				fmt.Sprintf("listing %s images: %v", archLabel(arch), err),
			)
			nc.Status.ResolvedImages = nil
			return
		}
		if img == nil {
			cs.SetFalse(
				apiv1.ConditionTypeImagesReady,
				"ImageNotFound",
				fmt.Sprintf("no %s snapshot matches ImageSelector (family=%s version=%q)", archLabel(arch), nc.Spec.ImageSelector.Family, nc.Spec.ImageSelector.Version),
			)
			nc.Status.ResolvedImages = nil
			return
		}
		resolved = append(resolved, apiv1.ResolvedImage{
			Architecture: archLabel(arch),
			ImageID:      img.ID,
		})
	}
	sort.SliceStable(resolved, func(i, j int) bool {
		return resolved[i].Architecture < resolved[j].Architecture
	})
	nc.Status.ResolvedImages = resolved
	cs.SetTrueWithReason(apiv1.ConditionTypeImagesReady, "ImagesResolved", "amd64 and arm64 snapshots resolved")
}

// resolveImage picks the newest non-deprecated, non-deleted hcloud snapshot
// image matching sel for the given architecture. Hcloud-side filtering
// handles the label selector and the architecture/type/status; description
// filtering for the family and version substrings happens client-side after
// the API returns, since the hcloud image API does not expose description
// substring matches as a query parameter.
//
// Returns (nil, nil) — not an error — when the API call succeeds but no
// image matches the description filter. Callers distinguish "API failure"
// from "no match" and surface different condition reasons.
func (r *Reconciler) resolveImage(ctx context.Context, sel apiv1.ImageSelector, arch hcloud.Architecture) (*hcloud.Image, error) {
	opts := hcloud.ImageListOpts{
		ListOpts: hcloud.ListOpts{
			LabelSelector: buildLabelSelector(sel.Selector),
		},
		Architecture: []hcloud.Architecture{arch},
		Type:         []hcloud.ImageType{hcloud.ImageTypeSnapshot},
		Status:       []hcloud.ImageStatus{hcloud.ImageStatusAvailable},
	}
	images, err := r.hcloud.Image.AllWithOpts(ctx, opts)
	if err != nil {
		return nil, fmt.Errorf("listing images: %w", err)
	}

	wantFamily := strings.ToLower(strings.TrimSpace(string(sel.Family)))
	wantVersion := strings.ToLower(strings.TrimSpace(sel.Version))

	var newest *hcloud.Image
	for _, img := range images {
		if img == nil || img.Type != hcloud.ImageTypeSnapshot || img.Status != hcloud.ImageStatusAvailable || img.Architecture != arch || img.IsDeprecated() || img.IsDeleted() {
			continue
		}
		desc := strings.ToLower(img.Description)
		if wantFamily != "" && !strings.Contains(desc, wantFamily) {
			continue
		}
		if wantVersion != "" && !strings.Contains(desc, wantVersion) {
			continue
		}
		if newest == nil || img.Created.After(newest.Created) || (img.Created.Equal(newest.Created) && img.ID > newest.ID) {
			newest = img
		}
	}
	return newest, nil
}

// buildLabelSelector renders a Kubernetes-style equality label selector for
// the hcloud label_selector query parameter. An empty map yields an empty
// string so callers can pass it straight through to ListOpts.LabelSelector
// (the hcloud client only emits the query parameter when the value is
// non-empty).
//
// The label keys are sorted so the resulting selector is stable across
// reconciles — that stability matters when other controllers inspect the
// selector string for change detection.
func buildLabelSelector(selector map[string]string) string {
	if len(selector) == 0 {
		return ""
	}
	keys := make([]string, 0, len(selector))
	for k := range selector {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%s", k, selector[k]))
	}
	return strings.Join(parts, ",")
}
