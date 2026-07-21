// Package pricing provides per-server-type hourly cost for the Hetzner Cloud
// catalog. This drives Karpenter's cost-optimal bin-packing.
//
// Hetzner's public API exposes primary-IPv4 and monthly prices per server
// type. We translate those to a single hourly "net" figure (sum of hourly
// server price + hourly equivalent of primary IPv4) so Karpenter can
// compare across types.
package pricing

import (
	"context"
	"fmt"

	"github.com/hetznercloud/hcloud-go/v2/hcloud"
)

// Provider fetches and caches the Hetzner Cloud pricing catalog.
type Provider struct {
	hcloud *hcloud.Client
}

// New constructs a pricing.Provider. The catalog is fetched lazily on first
// call to Price so an empty token does not block construction.
func New(hcloud *hcloud.Client) *Provider {
	return &Provider{hcloud: hcloud}
}

// Price returns the hourly net price for a given server type, including the
// hourly equivalent of the primary IPv4 charge (which Hetzner bills
// separately).
//
// TODO: implement using hcloud Pricing.All and converting PriceVAT+PriceGross
// per-month to a per-hour figure.
func (p *Provider) Price(ctx context.Context, serverType *hcloud.ServerType) (float64, error) {
	_ = ctx
	_ = serverType
	return 0, fmt.Errorf("pricing.Price: not yet implemented (TODO: hcloud.Pricing.All + monthlyToHourly + IPv4 adjustment)")
}
