// Package instance wraps Hetzner Cloud server lifecycle operations (Create,
// Get, List, Delete) and translates between hcloud.Server and Karpenter
// identifiers.
package instance

import (
	"context"
	"crypto/sha256"
	"errors"
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
// from apis/v1.
type PlacementGroupStrategy string

// Create provisions an hcloud server and tags it for Karpenter.
func (p *Provider) Create(ctx context.Context, opts CreateOpts) (*hcloud.Server, error) {
	var placementGroup *hcloud.PlacementGroup
	if opts.PlacementGroupStrategy != "none" {
		var err error
		placementGroup, err = p.placementGroup(ctx, opts.PlacementGroupStrategy)
		if err != nil {
			return nil, err
		}
	}

	result, _, err := p.client.Server.Create(ctx, hcloud.ServerCreateOpts{
		Name:           opts.Name,
		ServerType:     &hcloud.ServerType{Name: opts.ServerType},
		Image:          opts.Image,
		SSHKeys:        sshKeys(opts.SSHKeyIDs),
		Location:       &hcloud.Location{Name: opts.Location},
		UserData:       opts.UserData,
		Labels:         p.serverLabels(opts),
		Networks:       networks(opts.NetworkID),
		Firewalls:      firewalls(opts.FirewallIDs),
		PlacementGroup: placementGroup,
		PublicNet: &hcloud.ServerCreatePublicNet{
			EnableIPv4: opts.EnablePublicIPv4,
			EnableIPv6: opts.EnablePublicIPv6,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("instance: create server %q: %w", opts.Name, err)
	}
	return result.Server, nil
}

// Get fetches a server by its hcloud-formatted provider ID (hcloud://<id>).
//
// Returns (nil, nil) when the server is absent so callers can distinguish
// "missing" from "got an error".
func (p *Provider) Get(ctx context.Context, providerID string) (*hcloud.Server, error) {
	id, _, err := ParseProviderID(providerID)
	if err != nil {
		return nil, err
	}
	server, _, err := p.client.Server.GetByID(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("instance: get server %d: %w", id, err)
	}
	return server, nil
}

// List returns all servers owned by this cluster.
//
// Scoped by the karpenter.sh/cluster=<CLUSTER_NAME> label so two clusters
// sharing one Hetzner project never see each other's servers.
func (p *Provider) List(ctx context.Context) ([]*hcloud.Server, error) {
	servers, err := p.client.Server.AllWithOpts(ctx, hcloud.ServerListOpts{
		ListOpts: hcloud.ListOpts{LabelSelector: clusterLabelKey + "=" + p.clusterName},
	})
	if err != nil {
		return nil, fmt.Errorf("instance: list servers for cluster %q: %w", p.clusterName, err)
	}
	return servers, nil
}

// Delete terminates a server.
//
// Idempotent: deleting a missing server returns nil so Karpenter's disruption
// loop can safely retry on transient hcloud errors.
func (p *Provider) Delete(ctx context.Context, providerID string) error {
	id, _, err := ParseProviderID(providerID)
	if err != nil {
		return err
	}
	server, _, err := p.client.Server.GetByID(ctx, id)
	if err != nil {
		return fmt.Errorf("instance: get server %d before delete: %w", id, err)
	}
	if server == nil {
		return nil
	}
	if server.Labels[clusterLabelKey] != p.clusterName {
		return fmt.Errorf("instance: server %d is not owned by cluster %q", id, p.clusterName)
	}
	if _, _, err := p.client.Server.DeleteWithResult(ctx, server); err != nil {
		var apiErr hcloud.Error
		if errors.As(err, &apiErr) && apiErr.Code == hcloud.ErrorCodeNotFound {
			return nil
		}
		return fmt.Errorf("instance: delete server %d: %w", id, err)
	}
	return nil
}

func (p *Provider) placementGroup(ctx context.Context, strategy PlacementGroupStrategy) (*hcloud.PlacementGroup, error) {
	switch strategy {
	case "", "spread":
	default:
		return nil, fmt.Errorf("instance: unsupported placement group strategy %q", strategy)
	}

	name := placementGroupName(p.clusterName)
	group, _, err := p.client.PlacementGroup.GetByName(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("instance: get placement group %q: %w", name, err)
	}
	if group != nil {
		if err := p.validatePlacementGroup(group); err != nil {
			return nil, err
		}
		return group, nil
	}

	result, _, createErr := p.client.PlacementGroup.Create(ctx, hcloud.PlacementGroupCreateOpts{
		Name:   name,
		Labels: map[string]string{clusterLabelKey: p.clusterName},
		Type:   hcloud.PlacementGroupTypeSpread,
	})
	if createErr == nil {
		return result.PlacementGroup, nil
	}

	group, _, getErr := p.client.PlacementGroup.GetByName(ctx, name)
	if getErr == nil && group != nil {
		if err := p.validatePlacementGroup(group); err != nil {
			return nil, err
		}
		return group, nil
	}
	return nil, fmt.Errorf("instance: create placement group %q: %w", name, createErr)
}

func (p *Provider) validatePlacementGroup(group *hcloud.PlacementGroup) error {
	if group.Type != hcloud.PlacementGroupTypeSpread || group.Labels[clusterLabelKey] != p.clusterName {
		return fmt.Errorf("instance: placement group %q is not a spread group owned by cluster %q", group.Name, p.clusterName)
	}
	return nil
}

func (p *Provider) serverLabels(opts CreateOpts) map[string]string {
	labels := make(map[string]string, len(opts.Labels)+3)
	for key, value := range opts.Labels {
		labels[key] = value
	}
	if opts.NodeClaim != "" {
		labels[nodeClaimLabelKey] = opts.NodeClaim
	}
	if opts.NodePool != "" {
		labels[nodePoolLabelKey] = opts.NodePool
	}
	labels[clusterLabelKey] = p.clusterName
	return labels
}

func placementGroupName(clusterName string) string {
	sum := sha256.Sum256([]byte(clusterName))
	return fmt.Sprintf("karpenter-%x", sum[:8])
}

func networks(id int64) []*hcloud.Network {
	if id == 0 {
		return nil
	}
	return []*hcloud.Network{{ID: id}}
}

func firewalls(ids []int64) []*hcloud.ServerCreateFirewall {
	result := make([]*hcloud.ServerCreateFirewall, 0, len(ids))
	for _, id := range ids {
		result = append(result, &hcloud.ServerCreateFirewall{Firewall: hcloud.Firewall{ID: id}})
	}
	return result
}

func sshKeys(ids []int64) []*hcloud.SSHKey {
	result := make([]*hcloud.SSHKey, 0, len(ids))
	for _, id := range ids {
		result = append(result, &hcloud.SSHKey{ID: id})
	}
	return result
}

const (
	clusterLabelKey   = "karpenter.sh/cluster"
	nodeClaimLabelKey = "karpenter.sh/nodeclaim"
	nodePoolLabelKey  = "karpenter.sh/nodepool"
)

// ClusterLabel returns the (key, value) tuple this Provider uses to tag
// servers it owns. Exposed so other providers (e.g. instance-type) can keep
// label application consistent.
func (p *Provider) ClusterLabel() (string, string) {
	return clusterLabelKey, p.clusterName
}
