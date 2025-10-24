package hetzner

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/hetznercloud/hcloud-go/v2/hcloud"
)

// Client wraps the Hetzner Cloud API client
type Client struct {
	hcloud *hcloud.Client
}

// New creates a new Hetzner Cloud client
func New(apiToken string) *Client {
	return &Client{
		hcloud: hcloud.NewClient(hcloud.WithToken(apiToken)),
	}
}

// CreateNetwork creates a new private network
func (c *Client) CreateNetwork(ctx context.Context, name, ipRange, subnetRange, location string) (*hcloud.Network, error) {
	_, ipNet, err := net.ParseCIDR(ipRange)
	if err != nil {
		return nil, fmt.Errorf("invalid IP range: %w", err)
	}

	_, subnetIPNet, err := net.ParseCIDR(subnetRange)
	if err != nil {
		return nil, fmt.Errorf("invalid subnet range: %w", err)
	}

	// Get network zone from location
	networkZone := getNetworkZoneFromLocation(location)

	opts := hcloud.NetworkCreateOpts{
		Name:    name,
		IPRange: ipNet,
		Subnets: []hcloud.NetworkSubnet{
			{
				Type:        hcloud.NetworkSubnetTypeCloud,
				IPRange:     subnetIPNet,
				NetworkZone: networkZone,
			},
		},
	}

	network, _, err := c.hcloud.Network.Create(ctx, opts)
	if err != nil {
		return nil, fmt.Errorf("failed to create network: %w", err)
	}

	return network, nil
}

// getNetworkZoneFromLocation maps a location to its network zone
func getNetworkZoneFromLocation(location string) hcloud.NetworkZone {
	// Map of location codes to network zones
	// Reference: https://docs.hetzner.cloud/#network-zones
	locationToZone := map[string]hcloud.NetworkZone{
		// Europe zones
		"fsn1": hcloud.NetworkZoneEUCentral, // Falkenstein
		"nbg1": hcloud.NetworkZoneEUCentral, // Nuremberg
		"hel1": hcloud.NetworkZoneEUCentral, // Helsinki

		// US zones
		"ash": hcloud.NetworkZoneUSEast, // Ashburn
		"hil": hcloud.NetworkZoneUSWest, // Hillsboro
	}

	// Default to eu-central if location not found
	if zone, ok := locationToZone[location]; ok {
		return zone
	}
	return hcloud.NetworkZoneEUCentral
}

// GetNetwork retrieves a network by ID
func (c *Client) GetNetwork(ctx context.Context, id int64) (*hcloud.Network, error) {
	network, _, err := c.hcloud.Network.GetByID(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("failed to get network: %w", err)
	}
	return network, nil
}

// DeleteNetwork deletes a network
func (c *Client) DeleteNetwork(ctx context.Context, id int64) error {
	network, _, err := c.hcloud.Network.GetByID(ctx, id)
	if err != nil {
		return fmt.Errorf("failed to get network: %w", err)
	}

	if network == nil {
		return nil // Already deleted
	}

	_, err = c.hcloud.Network.Delete(ctx, network)
	if err != nil {
		return fmt.Errorf("failed to delete network: %w", err)
	}

	return nil
}

// CreateFirewall creates a new firewall
func (c *Client) CreateFirewall(ctx context.Context, name string, rules []hcloud.FirewallRule) (*hcloud.Firewall, error) {
	opts := hcloud.FirewallCreateOpts{
		Name:  name,
		Rules: rules,
	}

	result, _, err := c.hcloud.Firewall.Create(ctx, opts)
	if err != nil {
		return nil, fmt.Errorf("failed to create firewall: %w", err)
	}

	return result.Firewall, nil
}

// DeleteFirewall deletes a firewall
func (c *Client) DeleteFirewall(ctx context.Context, id int64) error {
	firewall, _, err := c.hcloud.Firewall.GetByID(ctx, id)
	if err != nil {
		return fmt.Errorf("failed to get firewall: %w", err)
	}

	if firewall == nil {
		return nil // Already deleted
	}

	_, err = c.hcloud.Firewall.Delete(ctx, firewall)
	if err != nil {
		return fmt.Errorf("failed to delete firewall: %w", err)
	}

	return nil
}

// CreateSSHKey creates a new SSH key
func (c *Client) CreateSSHKey(ctx context.Context, name, publicKey string) (*hcloud.SSHKey, error) {
	opts := hcloud.SSHKeyCreateOpts{
		Name:      name,
		PublicKey: publicKey,
	}

	key, _, err := c.hcloud.SSHKey.Create(ctx, opts)
	if err != nil {
		return nil, fmt.Errorf("failed to create SSH key: %w", err)
	}

	return key, nil
}

// GetSSHKeyByName retrieves an SSH key by name
func (c *Client) GetSSHKeyByName(ctx context.Context, name string) (*hcloud.SSHKey, error) {
	key, _, err := c.hcloud.SSHKey.GetByName(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("failed to get SSH key: %w", err)
	}
	return key, nil
}

// GetSSHKey retrieves an SSH key by ID
func (c *Client) GetSSHKey(ctx context.Context, id int64) (*hcloud.SSHKey, error) {
	key, _, err := c.hcloud.SSHKey.GetByID(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("failed to get SSH key: %w", err)
	}
	return key, nil
}

// GetFirewall retrieves a firewall by ID
func (c *Client) GetFirewall(ctx context.Context, id int64) (*hcloud.Firewall, error) {
	firewall, _, err := c.hcloud.Firewall.GetByID(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("failed to get firewall: %w", err)
	}
	return firewall, nil
}

// DeleteSSHKey deletes an SSH key
func (c *Client) DeleteSSHKey(ctx context.Context, id int64) error {
	key, _, err := c.hcloud.SSHKey.GetByID(ctx, id)
	if err != nil {
		return fmt.Errorf("failed to get SSH key: %w", err)
	}

	if key == nil {
		return nil // Already deleted
	}

	_, err = c.hcloud.SSHKey.Delete(ctx, key)
	if err != nil {
		return fmt.Errorf("failed to delete SSH key: %w", err)
	}

	return nil
}

// CreateServer creates a new server
func (c *Client) CreateServer(ctx context.Context, opts hcloud.ServerCreateOpts) (*hcloud.Server, error) {
	result, _, err := c.hcloud.Server.Create(ctx, opts)
	if err != nil {
		return nil, fmt.Errorf("failed to create server: %w", err)
	}

	return result.Server, nil
}

// GetServer retrieves a server by ID
func (c *Client) GetServer(ctx context.Context, id int64) (*hcloud.Server, error) {
	server, _, err := c.hcloud.Server.GetByID(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("failed to get server: %w", err)
	}
	return server, nil
}

// GetServersByLabel retrieves all servers with a specific label
func (c *Client) GetServersByLabel(ctx context.Context, labelKey, labelValue string) ([]*hcloud.Server, error) {
	servers, err := c.hcloud.Server.AllWithOpts(ctx, hcloud.ServerListOpts{
		ListOpts: hcloud.ListOpts{
			LabelSelector: fmt.Sprintf("%s=%s", labelKey, labelValue),
		},
	})
	if err != nil {
		return nil, fmt.Errorf("failed to get servers by label: %w", err)
	}
	return servers, nil
}

// DeleteServer deletes a server
func (c *Client) DeleteServer(ctx context.Context, id int64) error {
	server, _, err := c.hcloud.Server.GetByID(ctx, id)
	if err != nil {
		return fmt.Errorf("failed to get server: %w", err)
	}

	if server == nil {
		return nil // Already deleted
	}

	_, _, err = c.hcloud.Server.DeleteWithResult(ctx, server)
	if err != nil {
		return fmt.Errorf("failed to delete server: %w", err)
	}

	return nil
}

// GetServerType retrieves a server type by name
func (c *Client) GetServerType(ctx context.Context, name string) (*hcloud.ServerType, error) {
	serverType, _, err := c.hcloud.ServerType.GetByName(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("failed to get server type: %w", err)
	}

	if serverType == nil {
		return nil, fmt.Errorf("server type %s not found", name)
	}

	return serverType, nil
}

// GetImage retrieves an image by name
func (c *Client) GetImage(ctx context.Context, name string) (*hcloud.Image, error) {
	image, _, err := c.hcloud.Image.GetByNameAndArchitecture(ctx, name, hcloud.ArchitectureX86)
	if err != nil {
		return nil, fmt.Errorf("failed to get image: %w", err)
	}

	if image == nil {
		return nil, fmt.Errorf("image %s not found", name)
	}

	return image, nil
}

// GetSnapshot retrieves a snapshot by ID
func (c *Client) GetSnapshot(ctx context.Context, id int64) (*hcloud.Image, error) {
	image, _, err := c.hcloud.Image.GetByID(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("failed to get snapshot: %w", err)
	}

	if image == nil {
		return nil, fmt.Errorf("snapshot with ID %d not found", id)
	}

	return image, nil
}

// GetLocation retrieves a location by name
func (c *Client) GetLocation(ctx context.Context, name string) (*hcloud.Location, error) {
	location, _, err := c.hcloud.Location.GetByName(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("failed to get location: %w", err)
	}

	if location == nil {
		return nil, fmt.Errorf("location %s not found", name)
	}

	return location, nil
}

// AttachServerToNetwork attaches a server to a network
func (c *Client) AttachServerToNetwork(ctx context.Context, serverID, networkID int64) error {
	server, _, err := c.hcloud.Server.GetByID(ctx, serverID)
	if err != nil {
		return fmt.Errorf("failed to get server: %w", err)
	}

	network, _, err := c.hcloud.Network.GetByID(ctx, networkID)
	if err != nil {
		return fmt.Errorf("failed to get network: %w", err)
	}

	opts := hcloud.ServerAttachToNetworkOpts{
		Network: network,
	}

	_, _, err = c.hcloud.Server.AttachToNetwork(ctx, server, opts)
	if err != nil {
		return fmt.Errorf("failed to attach server to network: %w", err)
	}

	return nil
}

// ApplyFirewallToServer applies a firewall to a server
func (c *Client) ApplyFirewallToServer(ctx context.Context, firewallID, serverID int64) error {
	firewall, _, err := c.hcloud.Firewall.GetByID(ctx, firewallID)
	if err != nil {
		return fmt.Errorf("failed to get firewall: %w", err)
	}

	server, _, err := c.hcloud.Server.GetByID(ctx, serverID)
	if err != nil {
		return fmt.Errorf("failed to get server: %w", err)
	}

	opts := hcloud.FirewallResource{
		Type:   hcloud.FirewallResourceTypeServer,
		Server: &hcloud.FirewallResourceServer{ID: server.ID},
	}

	actions, _, err := c.hcloud.Firewall.ApplyResources(ctx, firewall, []hcloud.FirewallResource{opts})
	if err != nil {
		return fmt.Errorf("failed to apply firewall: %w", err)
	}

	// Wait for actions to complete
	for _, action := range actions {
		if err := c.WaitForAction(ctx, action.ID); err != nil {
			return fmt.Errorf("failed waiting for firewall action: %w", err)
		}
	}

	return nil
}

// WaitForAction waits for an action to complete
func (c *Client) WaitForAction(ctx context.Context, actionID int64) error {
	action, _, err := c.hcloud.Action.GetByID(ctx, actionID)
	if err != nil {
		return fmt.Errorf("failed to get action: %w", err)
	}

	// Poll for action completion
	for {
		action, _, err := c.hcloud.Action.GetByID(ctx, action.ID)
		if err != nil {
			return fmt.Errorf("failed to get action status: %w", err)
		}

		if action.Status == hcloud.ActionStatusSuccess {
			return nil
		}

		if action.Status == hcloud.ActionStatusError {
			return fmt.Errorf("action failed: %v", action.Error())
		}

		// Wait a bit before checking again
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(1 * time.Second):
			continue
		}
	}
}
