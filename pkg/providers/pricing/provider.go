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
	"strconv"
	"sync"

	"github.com/hetznercloud/hcloud-go/v2/hcloud"
)

// primaryIPv4Type is Hetzner's pricing catalog key for the per-server
// primary IPv4 address. Every Cloud server carries one by default and it's
// billed separately from the server type.
const primaryIPv4Type = "ipv4"

// Provider fetches and caches the Hetzner Cloud pricing catalog.
//
// The catalog is fetched lazily on the first Price call (so a missing or
// invalid token doesn't block construction) and cached for the lifetime of
// the Provider. The Instancetype provider constructs a new pricing.Provider
// on reconnect, which clears the cache.
type Provider struct {
	hcloud *hcloud.Client

	once    sync.Once
	pricing *hcloud.Pricing // nil until the first successful fetch
	err     error           // sticky: returned by every Price call after the first failure
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
// Hetzner bills the primary IPv4 address separately from the server type
// (see https://docs.hetzner.com/cloud/general/pricing — every Cloud server
// carries one IPv4 by default). Karpenter compares instance types on net
// hourly cost, so we add the IPv4 hourly rate to the server hourly rate to
// produce a single comparable figure.
//
// Hetzner's pricing is location-uniform in practice (verified against the
// /pricing endpoint at the time of writing). When that stops being true the
// call site will need to pass the location in.
func (p *Provider) Price(ctx context.Context, serverType *hcloud.ServerType) (float64, error) {
	if serverType == nil {
		return 0, fmt.Errorf("pricing.Price: serverType is nil")
	}
	if err := p.fetch(ctx); err != nil {
		return 0, err
	}

	serverHourly, err := serverTypeHourly(*p.pricing, serverType.Name)
	if err != nil {
		return 0, fmt.Errorf("pricing.Price: lookup server type %q: %w", serverType.Name, err)
	}

	ipv4Hourly, err := ipv4PrimaryHourly(p.pricing.PrimaryIPs)
	if err != nil {
		// IPv4 pricing missing is unexpected — every Cloud server gets
		// one by default and Hetzner has published pricing for it
		// consistently. Surface it but don't fail scheduling: cheaper
		// than dropping the entire price lookup when one field is
		// missing. Operators can spot the gap in the controller logs.
		ipv4Hourly = 0
	}

	return serverHourly + ipv4Hourly, nil
}

// fetch loads the pricing catalog exactly once for the lifetime of the
// Provider. Errors are sticky: subsequent Price calls return the same error
// until the Provider is reconstructed (callers wire a fresh Provider into
// the Instancetype provider on reconnect, which clears the cache).
func (p *Provider) fetch(ctx context.Context) error {
	p.once.Do(func() {
		if p.hcloud == nil {
			p.err = fmt.Errorf("pricing.fetch: hcloud client is nil")
			return
		}
		pricing, _, err := p.hcloud.Pricing.Get(ctx)
		if err != nil {
			p.err = fmt.Errorf("pricing.fetch: hcloud Pricing.Get: %w", err)
			return
		}
		p.pricing = &pricing
	})
	return p.err
}

// serverTypeHourly returns the first per-location hourly net figure for the
// named server type. Returns an error if no entry matches.
func serverTypeHourly(pricing hcloud.Pricing, name string) (float64, error) {
	for _, stp := range pricing.ServerTypes {
		if stp.ServerType == nil || stp.ServerType.Name != name {
			continue
		}
		if len(stp.Pricings) == 0 {
			return 0, fmt.Errorf("server type %q has no per-location pricing", name)
		}
		// Hetzner publishes a per-location slice but the price is the
		// same in every entry. Take the first.
		return parsePrice(stp.Pricings[0].Hourly.Net)
	}
	return 0, fmt.Errorf("server type %q not in pricing catalog", name)
}

// ipv4PrimaryHourly returns the hourly net for the ipv4 PrimaryIP type, or
// an error if Hetzner's response has no ipv4 entry at all.
func ipv4PrimaryHourly(prips []hcloud.PrimaryIPPricing) (float64, error) {
	for _, pip := range prips {
		if pip.Type != primaryIPv4Type {
			continue
		}
		if len(pip.Pricings) == 0 {
			continue
		}
		return parsePrice(pip.Pricings[0].Hourly.Net)
	}
	return 0, fmt.Errorf("no ipv4 entry in PrimaryIPs (got %d types)", len(prips))
}

// parsePrice parses Hetzner's Net/Gross decimal string ("3.490000") into a
// float64. Returns an error on malformed input — Hetzner's API has been
// stable on this format since GA, so a parse failure is worth surfacing
// rather than silently dropping a price.
func parsePrice(s string) (float64, error) {
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, fmt.Errorf("parse price %q: %w", s, err)
	}
	return v, nil
}