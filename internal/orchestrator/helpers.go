package orchestrator

import (
	"context"
	"fmt"
	"time"

	"github.com/dihmeetree/harbor/internal/config"
	"github.com/dihmeetree/harbor/internal/database"
	"github.com/dihmeetree/harbor/internal/hetzner"
	"github.com/dihmeetree/harbor/pkg/models"
	"github.com/hetznercloud/hcloud-go/v2/hcloud"
)

// HcloudToModels converts an hcloud.Server to models.Server
func HcloudToModels(hcloudServer *hcloud.Server) *models.Server {
	privateIP := ""
	if len(hcloudServer.PrivateNet) > 0 {
		privateIP = hcloudServer.PrivateNet[0].IP.String()
	}
	return &models.Server{
		Name:      hcloudServer.Name,
		PublicIP:  hcloudServer.PublicNet.IPv4.IP.String(),
		PrivateIP: privateIP,
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

// CreateServerWithDatabase creates a server and persists it to database
func CreateServerWithDatabase(ctx context.Context, client *hetzner.Client, db *database.DB, cfg config.ServerConfig, role models.ServerRole, network *hcloud.Network, firewall *hcloud.Firewall, sshKey *hcloud.SSHKey) (*hcloud.Server, error) {
	// Get server type
	serverType, err := client.GetServerType(ctx, cfg.Type)
	if err != nil {
		return nil, err
	}

	// Get image
	image, err := client.GetImage(ctx, cfg.Image)
	if err != nil {
		return nil, err
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

	// Get private IP
	var privateIP string
	for _, privateNet := range server.PrivateNet {
		if privateNet.Network.ID == network.ID {
			privateIP = privateNet.IP.String()
			break
		}
	}

	// Get database network ID
	netRepo := database.NewNetworkRepository(db)
	dbNetwork, err := netRepo.GetByHetznerID(network.ID)
	if err != nil {
		return nil, fmt.Errorf("failed to get network from database: %w", err)
	}
	if dbNetwork == nil {
		return nil, fmt.Errorf("network not found in database")
	}

	// Save to database
	serverRepo := database.NewServerRepository(db)
	dbServer := &models.Server{
		HetznerID: server.ID,
		Name:      server.Name,
		Type:      cfg.Type,
		Role:      role,
		PublicIP:  server.PublicNet.IPv4.IP.String(),
		PrivateIP: privateIP,
		Location:  cfg.Location,
		Image:     cfg.Image,
		Status:    string(server.Status),
		NetworkID: dbNetwork.ID,
		CreatedAt: time.Now(),
	}
	if err := serverRepo.Create(dbServer); err != nil {
		return nil, err
	}

	return server, nil
}
