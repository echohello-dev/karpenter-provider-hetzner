package nodeclass

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/hetznercloud/hcloud-go/v2/hcloud"

	apiv1 "github.com/echohello-dev/karpenter-provider-hetzner/v1/pkg/apis/v1"
	"github.com/echohello-dev/karpenter-provider-hetzner/v1/pkg/providers/imagefamily"
)

// supportedArchitectures is the set of architectures the controller resolves
// images for. Iteration order is fixed (amd64 first, arm64 second) so the
// resulting ResolvedImages slice is stable across reconciles.
var supportedArchitectures = []hcloud.Architecture{
	hcloud.ArchitectureX86,
	hcloud.ArchitectureARM,
}

// reconcileImages resolves one snapshot image per supported architecture for
// the NodeClass's ImageSelector and records the IDs in status.resolvedImages
// for the cloudprovider to boot from. On success it sets ImagesReady=True.
// On failure it sets ImagesReady=False with a reason that names the
// offending architecture.
//
// Resolution is sticky: each architecture keeps the image already recorded
// in status while that image still satisfies the selector, so a newly
// published snapshot matching the same selector does not move the
// resolution. Only a spec edit that invalidates the recorded image (or the
// image disappearing) re-resolves — which is what makes image drift a
// response to operator intent rather than to background activity in the
// Hetzner project. A fresh NodeClass (no recorded image) resolves to the
// newest match as before.
//
// status.resolvedImages is cleared only on definitive answers — an
// unsupported family, or a selector that matches nothing. An API failure
// leaves the previous resolution in place: the recorded images are still
// valid, and dropping them would make the next successful pass re-pick the
// newest match, churning nodes over a transient error.
//
// The matching itself lives in imagefamily.Provider — the same code the
// cloudprovider falls back to at create time — so the image the controller
// validated and the image a node boots cannot drift apart in behaviour.
func (r *Reconciler) reconcileImages(ctx context.Context, nc *apiv1.HCloudNodeClass) {
	cs := nc.StatusConditions()
	if err := imagefamily.ValidateFamily(nc.Spec.ImageSelector.Family); err != nil {
		cs.SetFalse(apiv1.ConditionTypeImagesReady, "ImageSelectorInvalid", err.Error())
		nc.Status.ResolvedImages = nil
		return
	}

	previous := make(map[string]int64, len(nc.Status.ResolvedImages))
	for _, resolved := range nc.Status.ResolvedImages {
		previous[resolved.Architecture] = resolved.ImageID
	}

	resolved := make([]apiv1.ResolvedImage, 0, len(supportedArchitectures))
	for _, arch := range supportedArchitectures {
		name := archLabel(arch)
		img, err := r.images.Resolve(ctx, nc.Spec.ImageSelector, arch, imagefamily.WithPreferredID(previous[name]))
		switch {
		case errors.Is(err, imagefamily.ErrNoMatch):
			cs.SetFalse(
				apiv1.ConditionTypeImagesReady,
				"ImageNotFound",
				fmt.Sprintf("no %s snapshot matches ImageSelector (family=%s version=%q)", name, nc.Spec.ImageSelector.Family, nc.Spec.ImageSelector.Version),
			)
			nc.Status.ResolvedImages = nil
			return
		case err != nil:
			cs.SetFalse(
				apiv1.ConditionTypeImagesReady,
				"ImageResolutionFailed",
				fmt.Sprintf("resolving %s images: %v", name, err),
			)
			return
		}
		resolved = append(resolved, apiv1.ResolvedImage{
			Architecture: name,
			ImageID:      img.Image.ID,
		})
	}
	sort.SliceStable(resolved, func(i, j int) bool {
		return resolved[i].Architecture < resolved[j].Architecture
	})
	nc.Status.ResolvedImages = resolved
	cs.SetTrueWithReason(apiv1.ConditionTypeImagesReady, "ImagesResolved", "amd64 and arm64 snapshots resolved")
}
