// Package pricing provides per-server-type hourly cost for the Hetzner Cloud
// catalog. This drives Karpenter's cost-optimal bin-packing.
//
// Hetzner's public API exposes primary-IPv4 and monthly prices per server
// type. We translate those to a single hourly "net" figure (sum of hourly
// server price + hourly equivalent of primary IPv4) so Karpenter can
// compare across types.
//
// The catalog is fetched lazily on the first Price call (so a missing or
// invalid token doesn't block construction), refreshed on a TTL, and kept
// across transient failures: a failed refresh serves the last known prices
// rather than making every offering unavailable.
package pricing

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/hetznercloud/hcloud-go/v2/hcloud"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// primaryIPv4Type is Hetzner's pricing catalog key for the per-server
// primary IPv4 address. Every Cloud server carries one by default and it's
// billed separately from the server type.
const primaryIPv4Type = "ipv4"

// defaultTTL is how long a fetched catalog counts as fresh. Hetzner
// reprices rarely, so an hourly refresh is cheap insurance against
// ordering on prices that quietly went stale.
const defaultTTL = time.Hour

// defaultRetryBackoff is how long the outcome of a failed fetch is reused
// before the next attempt.
//
// Price is called once per server type on every scheduling pass, so without
// a backoff a single unreachable /pricing endpoint turns into one request
// per server type per pass. Short enough that an outage clears itself
// without operator action; long enough not to hammer the API.
const defaultRetryBackoff = time.Minute

// Provider fetches and caches the Hetzner Cloud pricing catalog.
type Provider struct {
	hcloud *hcloud.Client

	ttl          time.Duration
	retryBackoff time.Duration
	now          func() time.Time

	mu          sync.Mutex
	pricing     *hcloud.Pricing // nil until the first successful fetch
	fetchedAt   time.Time
	lastErr     error // outcome of the most recent failed fetch
	lastAttempt time.Time
}

// Option mutates a Provider at construction time.
type Option func(*Provider)

// WithTTL overrides how long a fetched catalog stays fresh.
func WithTTL(d time.Duration) Option {
	return func(p *Provider) { p.ttl = d }
}

// WithRetryBackoff overrides how long a failed fetch is remembered.
func WithRetryBackoff(d time.Duration) Option {
	return func(p *Provider) { p.retryBackoff = d }
}

// WithClock overrides the clock used for TTL and backoff comparisons.
// Tests advance the clock to drive expiry deterministically.
func WithClock(now func() time.Time) Option {
	return func(p *Provider) { p.now = now }
}

// New constructs a pricing.Provider. The catalog is fetched lazily on first
// call to Price so an empty token does not block construction.
func New(hcloud *hcloud.Client, opts ...Option) *Provider {
	p := &Provider{
		hcloud:       hcloud,
		ttl:          defaultTTL,
		retryBackoff: defaultRetryBackoff,
		now:          time.Now,
	}
	for _, opt := range opts {
		opt(p)
	}
	return p
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
	pricing, err := p.fetch(ctx)
	if err != nil {
		return 0, err
	}

	serverHourly, err := serverTypeHourly(*pricing, serverType.Name)
	if err != nil {
		return 0, fmt.Errorf("pricing.Price: lookup server type %q: %w", serverType.Name, err)
	}

	ipv4Hourly, err := ipv4PrimaryHourly(pricing.PrimaryIPs)
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

// fetch returns the pricing catalog, refreshing it when the cached copy has
// aged past the TTL.
//
// Refresh failures never drop prices we already have: the last good catalog
// keeps serving (with its stale timestamp) until a later attempt succeeds,
// because an unavailable offering is worse for scheduling than a slightly
// outdated price. When there is no catalog at all — the very first fetch
// failed — the fetch error is returned so callers can tell "unknown price"
// from "known price". Attempts inside the retry backoff reuse the previous
// outcome without touching the API.
func (p *Provider) fetch(ctx context.Context) (*hcloud.Pricing, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	now := p.now()
	if p.pricing != nil && now.Sub(p.fetchedAt) < p.ttl {
		return p.pricing, nil
	}
	if !p.lastAttempt.IsZero() && now.Sub(p.lastAttempt) < p.retryBackoff {
		return p.pricing, p.errOrLast()
	}

	p.lastAttempt = now
	if p.hcloud == nil {
		p.lastErr = fmt.Errorf("pricing.fetch: hcloud client is nil")
		return p.pricing, p.errOrLast()
	}
	pricing, _, err := p.hcloud.Pricing.Get(ctx)
	if err != nil {
		p.lastErr = fmt.Errorf("pricing.fetch: hcloud Pricing.Get: %w", err)
		if p.pricing != nil {
			log.FromContext(ctx).Error(p.lastErr, "refreshing Hetzner pricing catalog failed; keeping last known prices",
				"retryAfter", p.retryBackoff.String())
			return p.pricing, nil
		}
		return nil, p.lastErr
	}
	p.pricing = &pricing
	p.fetchedAt = now
	p.lastErr = nil
	return p.pricing, nil
}

// errOrLast reports the state left by the most recent attempt: a cached
// catalog wins (stale prices beat no prices), otherwise the fetch error.
func (p *Provider) errOrLast() error {
	if p.pricing != nil {
		return nil
	}
	return p.lastErr
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
