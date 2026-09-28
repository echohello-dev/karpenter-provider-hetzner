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
// On any per-architecture failure it sets ImagesReady=False with a reason
// that names the offending architecture and clears ResolvedImages so stale
// entries from a previous reconcile do not linger on the resource.
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

	resolved := make([]apiv1.ResolvedImage, 0, len(supportedArchitectures))
	for _, arch := range supportedArchitectures {
		img, err := r.images.Resolve(ctx, nc.Spec.ImageSelector, arch)
		switch {
		case errors.Is(err, imagefamily.ErrNoMatch):
			cs.SetFalse(
				apiv1.ConditionTypeImagesReady,
				"ImageNotFound",
				fmt.Sprintf("no %s snapshot matches ImageSelector (family=%s version=%q)", archLabel(arch), nc.Spec.ImageSelector.Family, nc.Spec.ImageSelector.Version),
			)
			nc.Status.ResolvedImages = nil
			return
		case err != nil:
			cs.SetFalse(
				apiv1.ConditionTypeImagesReady,
				"ImageResolutionFailed",
				fmt.Sprintf("resolving %s images: %v", archLabel(arch), err),
			)
			nc.Status.ResolvedImages = nil
			return
		}
		resolved = append(resolved, apiv1.ResolvedImage{
			Architecture: archLabel(arch),
			ImageID:      img.Image.ID,
		})
	}
	sort.SliceStable(resolved, func(i, j int) bool {
		return resolved[i].Architecture < resolved[j].Architecture
	})
	nc.Status.ResolvedImages = resolved
	cs.SetTrueWithReason(apiv1.ConditionTypeImagesReady, "ImagesResolved", "amd64 and arm64 snapshots resolved")
}
