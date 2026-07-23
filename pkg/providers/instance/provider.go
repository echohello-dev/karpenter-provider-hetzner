// Package instance wraps Hetzner Cloud server lifecycle operations (Create,
// Get, List, Delete) and translates between hcloud.Server and Karpenter
// identifiers.
package instance

import (
	"context"
	"fmt"

	"github.com/hetznercloud/hcloud-go/v2/hcloud"
)

// Provider wraps the hcloud client with Hetzner-specific Karpenter glue.
//
// All Karpenter-owned servers carry the karpenter.sh/cluster=<CLUSTER_NAME>
// label, so List and Delete can be scoped by cluster to keep multiple
// clusters safe to share a single Hetzner project.
type Provider struct {
	client      *hcloud.Client
	clusterName string
}

// New constructs a Provider. clusterName is REQUIRED and scopes all server
// operations; the Provider fails fast when it is unset, because an empty
// clusterName label would mean "all servers in the project" — an obvious
// outage waiting to happen.
func New(client *hcloud.Client, clusterName string) (*Provider, error) {
	if client == nil {
		return nil, fmt.Errorf("instance: hcloud client is nil")
	}
	if clusterName == "" {
		return nil, fmt.Errorf("instance: cluster name is empty (would create servers unscoped across clusters)")
	}
	return &Provider{client: client, clusterName: clusterName}, nil
}

// CreateOpts captures the parameters for creating a Hetzner server for a
// node. One struct rather than positional args: hcloud.ServerCreateOpts has
// many fields and we deliberately only forward the ones this provider owns.
type CreateOpts struct {
	Name                   string
	ServerType             string
	Location               string
	Image                  *hcloud.Image
	NetworkID              int64
	FirewallIDs            []int64
	SSHKeyIDs              []int64
	Labels                 map[string]string
	UserData               string
	NodeClaim              string
	NodePool               string
	PlacementGroupStrategy PlacementGroupStrategy
	EnablePublicIPv4       bool
	EnablePublicIPv6       bool
}

// PlacementGroupStrategy is reproduced here to keep this package independent
// from apis/v1. Use apis.PublicIPv4Enabled(...) to derive it where needed.
type PlacementGroupStrategy string

// Create provisions an hcloud server and tags it for Karpenter.
//
// TODO: implement using hcloud.ServerCreateOpts with Labels merged from
// NodeClaim+NodePool, the right PlacementGroup selected by Strategy, and
// PublicNet computed from EnablePublicIPv4/IPv6.
func (p *Provider) Create(ctx context.Context, opts CreateOpts) (*hcloud.Server, error) {
	return nil, fmt.Errorf("instance.Create: not yet implemented (TODO: hcloud.ServerCreateOpts + LabelMerge + PlacementGroup)")
}

// Get fetches a server by its hcloud-formatted provider ID (hcloud://<id>).
//
// Returns (nil, nil) when the server is absent so callers can distinguish
// "missing" from "got an error".
func (p *Provider) Get(ctx context.Context, providerID string) (*hcloud.Server, error) {
	_, _, err := ParseProviderID(providerID)
	if err != nil {
		return nil, err
	}
	// TODO: implement using hcloud Client.Server.GetByID.
	_ = ctx
	return nil, fmt.Errorf("instance.Get: not yet implemented")
}

// List returns all servers owned by this cluster.
//
// Scoped by the karpenter.sh/cluster=<CLUSTER_NAME> label so two clusters
// sharing one Hetzner project never see each other's servers.
func (p *Provider) List(ctx context.Context) ([]*hcloud.Server, error) {
	_ = ctx
	return nil, fmt.Errorf("instance.List: not yet implemented (TODO: ListOpts with label selector)")
}

// Delete terminates a server.
//
// Idempotent: deleting a missing server returns nil so Karpenter's disruption
// loop can safely retry on transient hcloud errors.
func (p *Provider) Delete(ctx context.Context, providerID string) error {
	_, _, err := ParseProviderID(providerID)
	if err != nil {
		return err
	}
	// TODO: implement using hcloud Client.Server.Delete.
	_ = ctx
	return fmt.Errorf("instance.Delete: not yet implemented")
}

// clusterLabelKey is the hcloud-label key applied to every server we create.
const clusterLabelKey = "karpenter.sh/cluster"

// ClusterLabel returns the (key, value) tuple this Provider uses to tag
// servers it owns. Exposed so other providers (e.g. instance-type) can keep
// label application consistent.
func (p *Provider) ClusterLabel() (string, string) {
	return clusterLabelKey, p.clusterName
}
