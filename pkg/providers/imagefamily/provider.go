// Package imagefamily resolves an OS snapshot in Hetzner Cloud based on the
// NodeClass's ImageSelector. Supports Talos and Ubuntu families, picked by
// substring match against the snapshot description or by hcloud label
// selector (preferred — pin an exact snapshot, including baked extensions).
package imagefamily

import (
	"context"
	"fmt"

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

// Resolve lists available images matching the ImageSelector for the given
// target architecture. Returns the newest matching image when no version or
// label selector is supplied.
//
// TODO: implement. Plan:
//  1. Call hcloud Image.AllWithOpts with a ListOpts carrying the label
//     selector and an Architecture filter.
//  2. If the caller supplied version as a substring, apply a description
//     Contains filter on top.
//  3. Reject mismatched architecture (return an error rather than boot a
//     wrong-arch node).
func (p *Provider) Resolve(ctx context.Context, sel apiv1.ImageSelector, arch hcloud.Architecture) (*ResolvedImage, error) {
	_ = sel
	return nil, fmt.Errorf("imagefamily.Resolve: not yet implemented (TODO: ListWithOpts + label+arch+version filter + mismatch guard)")
}
