package orchestrator

import (
	"context"
	"fmt"

	"github.com/dihmeetree/harbor/internal/config"
	"github.com/dihmeetree/harbor/internal/hetzner"
	"github.com/dihmeetree/harbor/pkg/models"
	"github.com/hetznercloud/hcloud-go/v2/hcloud"
)

// ExtractPrivateIP extracts the private IP from an hcloud.Server.
// Returns empty string if no private network is configured.
func ExtractPrivateIP(server *hcloud.Server) string {
	if len(server.PrivateNet) > 0 {
		return server.PrivateNet[0].IP.String()
	}
	return ""
}

// HcloudToModels converts an hcloud.Server to models.Server.
func HcloudToModels(hcloudServer *hcloud.Server) *models.Server {
	return &models.Server{
		Name:      hcloudServer.Name,
		PublicIP:  hcloudServer.PublicNet.IPv4.IP.String(),
		PrivateIP: ExtractPrivateIP(hcloudServer),
	}
}

// HcloudListToModels converts a slice of hcloud servers to models servers
func HcloudListToModels(servers []*hcloud.Server) []*models.Server {
	result := make([]*models.Server, len(servers))
	for i, server := range servers {
		result[i] = HcloudToModels(server)
	}
	return result
}

// RoleToLabel converts models.ServerRole to Hetzner label string
func RoleToLabel(role models.ServerRole) string {
	switch role {
	case models.RoleDataPlane:
		return "lb"
	case models.RoleApp:
		return "app"
	case models.RoleControlPlane:
		return "control"
	default:
		return string(role)
	}
}

// ApplyK6Defaults fills in default values for K6 configuration
func ApplyK6Defaults(k6 *config.K6Config) {
	if k6.PreallocatedVUs == 0 {
		k6.PreallocatedVUs = 10
	}
	if k6.MaxVUs == 0 {
		k6.MaxVUs = 100
	}
	if k6.Rate == 0 {
		k6.Rate = 10
	}
	if k6.Duration == "" {
		k6.Duration = "30s"
	}
	if k6.TargetPath == "" {
		k6.TargetPath = "/"
	}
	if k6.ConnectionTimeout == "" {
		k6.ConnectionTimeout = "10s"
	}
	if k6.RequestTimeout == "" {
		k6.RequestTimeout = "30s"
	}
	if k6.GracefulStop == "" {
		k6.GracefulStop = "30s"
	}
}

// CreateServer creates a server in Hetzner Cloud
func CreateServer(ctx context.Context, client *hetzner.Client, cfg config.ServerConfig, snapshotID int64, role models.ServerRole, network *hcloud.Network, firewall *hcloud.Firewall, sshKey *hcloud.SSHKey) (*hcloud.Server, error) {
	// Get server type
	serverType, err := client.GetServerType(ctx, cfg.Type)
	if err != nil {
		return nil, err
	}

	// Get Flatcar snapshot
	image, err := client.GetSnapshot(ctx, snapshotID)
	if err != nil {
		return nil, fmt.Errorf("failed to get snapshot %d: %w", snapshotID, err)
	}

	// Get location
	location, err := client.GetLocation(ctx, cfg.Location)
	if err != nil {
		return nil, err
	}

	// Determine role label
	roleLabel := RoleToLabel(role)

	// Create server
	opts := hcloud.ServerCreateOpts{
		Name:       cfg.Name,
		ServerType: serverType,
		Image:      image,
		Location:   location,
		SSHKeys:    []*hcloud.SSHKey{sshKey},
		Networks:   []*hcloud.Network{network},
		Firewalls:  []*hcloud.ServerCreateFirewall{{Firewall: *firewall}},
		Labels: map[string]string{
			"role":      roleLabel,
			"managed":   "harbor",
			"autoscale": "false", // Manually created servers
		},
	}

	server, err := client.CreateServer(ctx, opts)
	if err != nil {
		return nil, err
	}

	return server, nil
}
