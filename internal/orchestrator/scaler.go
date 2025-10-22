package orchestrator

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/dihmeetree/harbor/internal/apisix"
	"github.com/dihmeetree/harbor/internal/config"
	"github.com/dihmeetree/harbor/internal/database"
	"github.com/dihmeetree/harbor/internal/hetzner"
	"github.com/dihmeetree/harbor/internal/ssh"
	"github.com/dihmeetree/harbor/pkg/models"
	"github.com/hetznercloud/hcloud-go/v2/hcloud"
)

// ManualScaler handles manual scaling operations via CLI
type ManualScaler struct {
	config        *config.Config
	hetznerToken  string
	hetznerClient *hetzner.Client
	apisixClient  *apisix.Client
	deployer      *Deployer
	sshClient     *ssh.Client
	db            *database.DB
}

// NewManualScaler creates a new manual scaler
func NewManualScaler(cfg *config.Config, hetznerToken string, controlPlaneIP string, apisixKey string, sshKeyPath string, db *database.DB) (*ManualScaler, error) {
	hetznerClient := hetzner.New(hetznerToken)
	deployer := NewDeployer(cfg, db, sshKeyPath)

	// Create SSH connection to control plane
	sshClient, err := ssh.New(controlPlaneIP, "root", sshKeyPath)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to control plane via SSH: %w", err)
	}

	// Create APISIX client that uses SSH tunnel to control plane's private IP
	apisixURL := "http://10.0.1.1:9180"
	apisixClient := apisix.NewWithSSH(apisixURL, apisixKey, sshClient)

	return &ManualScaler{
		config:        cfg,
		hetznerToken:  hetznerToken,
		hetznerClient: hetznerClient,
		apisixClient:  apisixClient,
		deployer:      deployer,
		sshClient:     sshClient,
		db:            db,
	}, nil
}

// Close closes the SSH connection
func (m *ManualScaler) Close() error {
	if m.sshClient != nil {
		return m.sshClient.Close()
	}
	return nil
}

// GetServerCount gets the current number of servers for a role from Hetzner
func (m *ManualScaler) GetServerCount(ctx context.Context, role string) (int, error) {
	// Query Hetzner API for actual server count (includes autoscaler-created servers)
	servers, err := m.hetznerClient.GetServersByLabel(ctx, "role", role)
	if err != nil {
		return 0, fmt.Errorf("failed to get servers from Hetzner: %w", err)
	}
	return len(servers), nil
}

// ScaleUp adds the specified number of servers
func (m *ManualScaler) ScaleUp(ctx context.Context, role string, poolName string, count int) error {
	// Get existing servers from Hetzner to determine next index
	var dbRole models.ServerRole
	switch role {
	case "lb":
		dbRole = models.RoleDataPlane
	case "app":
		dbRole = models.RoleApp
	default:
		return fmt.Errorf("unsupported role: %s", role)
	}

	// Query Hetzner for existing servers (includes autoscaler-created servers)
	existingHetznerServers, err := m.hetznerClient.GetServersByLabel(ctx, "role", role)
	if err != nil {
		return fmt.Errorf("failed to get existing servers from Hetzner: %w", err)
	}

	startIndex := len(existingHetznerServers) + 1
	serverRepo := database.NewServerRepository(m.db)

	// Extract base name from control plane server name
	baseName := m.config.Server.Name
	if idx := strings.LastIndex(baseName, "-"); idx != -1 {
		baseName = baseName[:idx]
	}

	// Get network from database
	netRepo := database.NewNetworkRepository(m.db)
	networks, err := netRepo.GetAll()
	if err != nil || len(networks) == 0 {
		return fmt.Errorf("failed to get network: %w", err)
	}
	network, err := m.hetznerClient.GetNetwork(ctx, networks[0].HetznerID)
	if err != nil {
		return fmt.Errorf("failed to get hetzner network: %w", err)
	}

	// Get firewall from database
	fwRepo := database.NewFirewallRepository(m.db)
	firewalls, err := fwRepo.GetAll()
	if err != nil || len(firewalls) == 0 {
		return fmt.Errorf("failed to get firewall: %w", err)
	}
	firewall, err := m.hetznerClient.GetFirewall(ctx, firewalls[0].HetznerID)
	if err != nil {
		return fmt.Errorf("failed to get hetzner firewall: %w", err)
	}

	// Get SSH key from database
	sshKeyRepo := database.NewSSHKeyRepository(m.db)
	sshKeys, err := sshKeyRepo.GetAll()
	if err != nil || len(sshKeys) == 0 {
		return fmt.Errorf("failed to get SSH key: %w", err)
	}
	sshKey, err := m.hetznerClient.GetSSHKey(ctx, sshKeys[0].HetznerID)
	if err != nil {
		return fmt.Errorf("failed to get hetzner SSH key: %w", err)
	}

	// Create servers and track them
	var newServers []*models.Server
	for i := range count {
		// Determine server configuration based on role
		var serverCfg config.ServerConfig
		switch role {
		case "lb":
			serverCfg = config.ServerConfig{
				Name:     fmt.Sprintf("%s-%s-%d", baseName, role, startIndex+i),
				Type:     m.config.LoadBalancer.ServerType,
				Location: m.config.LoadBalancer.Location,
				Image:    m.config.LoadBalancer.Image,
			}
		case "app":
			serverCfg = config.ServerConfig{
				Name:     fmt.Sprintf("%s-%s-%d", baseName, role, startIndex+i),
				Type:     m.config.App.ServerType,
				Location: m.config.App.Location,
				Image:    m.config.App.Image,
			}
		}

		// Create server using hetzner client
		server, err := m.createServerViaHetzner(ctx, serverCfg, dbRole, network, firewall, sshKey)
		if err != nil {
			return fmt.Errorf("failed to create server: %w", err)
		}

		// Get the database entry for this server
		dbServers, err := serverRepo.GetByRole(dbRole)
		if err != nil {
			return fmt.Errorf("failed to get server from database: %w", err)
		}

		// Find the newly created server in database
		for _, dbServer := range dbServers {
			if dbServer.HetznerID == server.ID {
				newServers = append(newServers, dbServer)
				break
			}
		}

		fmt.Printf("[info] %s server created: %s (%s)\n", poolName, server.Name, server.PublicNet.IPv4.IP.String())
	}

	// Install Docker on new servers
	fmt.Printf("[info] Waiting for %d new server(s) to be ready...\n", len(newServers))
	if err := m.deployer.InstallDocker(0, newServers); err != nil {
		return fmt.Errorf("failed to install Docker: %w", err)
	}

	// Deploy services to new servers based on role
	fmt.Printf("[info] Deploying %d %s server(s)\n", len(newServers), poolName)

	// Get control plane IP for data planes
	controlPlanes, _ := serverRepo.GetByRole(models.RoleControlPlane)
	var controlPlaneIP string
	if len(controlPlanes) > 0 {
		controlPlaneIP = controlPlanes[0].PrivateIP
	}

	for _, server := range newServers {
		switch role {
		case "lb":
			if err := m.deployer.DeployDataPlane(0, server, controlPlaneIP); err != nil {
				return fmt.Errorf("failed to deploy data plane on %s: %w", server.Name, err)
			}
			fmt.Printf("[info] ✓ Data plane deployed on %s\n", server.Name)
		case "app":
			if err := m.deployer.DeployAppServer(0, server); err != nil {
				return fmt.Errorf("failed to deploy app server %s: %w", server.Name, err)
			}
			fmt.Printf("[info] ✓ App deployed on %s\n", server.Name)
		}
	}

	// Update configurations based on role
	if role == "app" {
		fmt.Println("[info] Updating APISIX upstreams")

		// Query Hetzner for ALL app servers (including autoscaler-created ones)
		hetznerServers, err := m.hetznerClient.GetServersByLabel(ctx, "role", "app")
		if err != nil {
			return fmt.Errorf("failed to get app servers from Hetzner: %w", err)
		}

		// Convert to models.Server format for UpdateAPISIXUpstreams
		var allAppServers []*models.Server
		for _, hs := range hetznerServers {
			privateIP := ""
			if len(hs.PrivateNet) > 0 {
				privateIP = hs.PrivateNet[0].IP.String()
			}
			allAppServers = append(allAppServers, &models.Server{
				Name:      hs.Name,
				PrivateIP: privateIP,
				PublicIP:  hs.PublicNet.IPv4.IP.String(),
			})
		}

		if err := m.deployer.UpdateAPISIXUpstreams(0, controlPlanes[0], allAppServers); err != nil {
			return fmt.Errorf("failed to update APISIX upstreams: %w", err)
		}
	}

	// Update Prometheus configuration for all roles
	fmt.Println("[info] Updating Prometheus configuration")

	// Query Hetzner for ALL servers by role
	allDataPlanes, err := m.hetznerClient.GetServersByLabel(ctx, "role", "lb")
	if err != nil {
		return fmt.Errorf("failed to get data plane servers from Hetzner: %w", err)
	}

	allAppServers, err := m.hetznerClient.GetServersByLabel(ctx, "role", "app")
	if err != nil {
		return fmt.Errorf("failed to get app servers from Hetzner: %w", err)
	}

	// Convert to models.Server format
	var dataPlanes []*models.Server
	for _, hs := range allDataPlanes {
		privateIP := ""
		if len(hs.PrivateNet) > 0 {
			privateIP = hs.PrivateNet[0].IP.String()
		}
		dataPlanes = append(dataPlanes, &models.Server{
			Name:      hs.Name,
			PrivateIP: privateIP,
			PublicIP:  hs.PublicNet.IPv4.IP.String(),
		})
	}

	var appServers []*models.Server
	for _, hs := range allAppServers {
		privateIP := ""
		if len(hs.PrivateNet) > 0 {
			privateIP = hs.PrivateNet[0].IP.String()
		}
		appServers = append(appServers, &models.Server{
			Name:      hs.Name,
			PrivateIP: privateIP,
			PublicIP:  hs.PublicNet.IPv4.IP.String(),
		})
	}

	if err := m.deployer.UpdatePrometheusConfig(0, controlPlanes[0], dataPlanes, appServers); err != nil {
		return fmt.Errorf("failed to update Prometheus configuration: %w", err)
	}

	// Update k6 targets if k6 is enabled and we scaled load balancers
	if m.config.K6.Enabled && role == "lb" {
		fmt.Println("[info] Updating k6 load balancer targets")
		if err := m.updateK6Targets(ctx, dataPlanes); err != nil {
			fmt.Printf("[warn] Failed to update k6 targets: %v\n", err)
		} else {
			fmt.Println("[info] ✓ k6 targets updated")
		}
	}

	fmt.Printf("[info] ✓ Successfully scaled %s to %d servers\n", poolName, startIndex+count-1)

	return nil
}

// createServerViaHetzner creates a server using the hetzner client (same pattern as provisioner)
func (m *ManualScaler) createServerViaHetzner(ctx context.Context, cfg config.ServerConfig, role models.ServerRole, network *hcloud.Network, firewall *hcloud.Firewall, sshKey *hcloud.SSHKey) (*hcloud.Server, error) {
	// Get server type
	serverType, err := m.hetznerClient.GetServerType(ctx, cfg.Type)
	if err != nil {
		return nil, err
	}

	// Get image
	image, err := m.hetznerClient.GetImage(ctx, cfg.Image)
	if err != nil {
		return nil, err
	}

	// Get location
	location, err := m.hetznerClient.GetLocation(ctx, cfg.Location)
	if err != nil {
		return nil, err
	}

	// Determine role label
	var roleLabel string
	switch role {
	case models.RoleDataPlane:
		roleLabel = "lb"
	case models.RoleApp:
		roleLabel = "app"
	default:
		roleLabel = string(role)
	}

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

	server, err := m.hetznerClient.CreateServer(ctx, opts)
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
	netRepo := database.NewNetworkRepository(m.db)
	dbNetwork, err := netRepo.GetByHetznerID(network.ID)
	if err != nil {
		return nil, fmt.Errorf("failed to get network from database: %w", err)
	}
	if dbNetwork == nil {
		return nil, fmt.Errorf("network not found in database")
	}

	// Save to database
	serverRepo := database.NewServerRepository(m.db)
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

// ScaleDown removes the specified number of servers
func (m *ManualScaler) ScaleDown(ctx context.Context, role string, poolName string, count int) error {
	// Get all servers with this role from Hetzner
	hetznerServers, err := m.hetznerClient.GetServersByLabel(ctx, "role", role)
	if err != nil {
		return fmt.Errorf("failed to get servers from Hetzner: %w", err)
	}

	if len(hetznerServers) < count {
		return fmt.Errorf("cannot remove %d servers, only %d exist", count, len(hetznerServers))
	}

	serverRepo := database.NewServerRepository(m.db)

	fmt.Printf("[info] Removing %d %s server(s)\n", count, poolName)

	// Remove the newest servers first (highest Hetzner IDs)
	for range count {
		// Find server with highest Hetzner ID
		var serverToRemove *hcloud.Server
		for _, srv := range hetznerServers {
			if serverToRemove == nil || srv.ID > serverToRemove.ID {
				serverToRemove = srv
			}
		}

		// Delete server from Hetzner
		if err := m.hetznerClient.DeleteServer(ctx, serverToRemove.ID); err != nil {
			return fmt.Errorf("failed to delete server %s from Hetzner: %w", serverToRemove.Name, err)
		}

		// Try to delete from database if it exists
		dbServers, _ := serverRepo.GetByRole(models.RoleApp)
		for _, dbSrv := range dbServers {
			if dbSrv.HetznerID == serverToRemove.ID {
				_ = serverRepo.Delete(dbSrv.ID)
				break
			}
		}

		fmt.Printf("[info] ✓ Server %s removed\n", serverToRemove.Name)

		// Remove from servers list for next iteration
		var newServers []*hcloud.Server
		for _, srv := range hetznerServers {
			if srv.ID != serverToRemove.ID {
				newServers = append(newServers, srv)
			}
		}
		hetznerServers = newServers
	}

	// Get control plane from database
	controlPlanes, _ := serverRepo.GetByRole(models.RoleControlPlane)
	if len(controlPlanes) == 0 {
		return fmt.Errorf("no control plane found")
	}

	// Update configurations based on role
	if role == "app" {
		fmt.Println("[info] Updating APISIX upstreams")

		// Query Hetzner for ALL remaining app servers (including autoscaler-created ones)
		hetznerServers, err := m.hetznerClient.GetServersByLabel(ctx, "role", "app")
		if err != nil {
			return fmt.Errorf("failed to get app servers from Hetzner: %w", err)
		}

		// Convert to models.Server format for UpdateAPISIXUpstreams
		var remainingAppServers []*models.Server
		for _, hs := range hetznerServers {
			privateIP := ""
			if len(hs.PrivateNet) > 0 {
				privateIP = hs.PrivateNet[0].IP.String()
			}
			remainingAppServers = append(remainingAppServers, &models.Server{
				Name:      hs.Name,
				PrivateIP: privateIP,
				PublicIP:  hs.PublicNet.IPv4.IP.String(),
			})
		}

		if err := m.deployer.UpdateAPISIXUpstreams(0, controlPlanes[0], remainingAppServers); err != nil {
			return fmt.Errorf("failed to update APISIX upstreams: %w", err)
		}
	}

	// Update Prometheus configuration for all roles
	fmt.Println("[info] Updating Prometheus configuration")

	// Query Hetzner for ALL servers by role
	allDataPlanes, err := m.hetznerClient.GetServersByLabel(ctx, "role", "lb")
	if err != nil {
		return fmt.Errorf("failed to get data plane servers from Hetzner: %w", err)
	}

	allAppServers, err := m.hetznerClient.GetServersByLabel(ctx, "role", "app")
	if err != nil {
		return fmt.Errorf("failed to get app servers from Hetzner: %w", err)
	}

	// Convert to models.Server format
	var dataPlanes []*models.Server
	for _, hs := range allDataPlanes {
		privateIP := ""
		if len(hs.PrivateNet) > 0 {
			privateIP = hs.PrivateNet[0].IP.String()
		}
		dataPlanes = append(dataPlanes, &models.Server{
			Name:      hs.Name,
			PrivateIP: privateIP,
			PublicIP:  hs.PublicNet.IPv4.IP.String(),
		})
	}

	var appServers []*models.Server
	for _, hs := range allAppServers {
		privateIP := ""
		if len(hs.PrivateNet) > 0 {
			privateIP = hs.PrivateNet[0].IP.String()
		}
		appServers = append(appServers, &models.Server{
			Name:      hs.Name,
			PrivateIP: privateIP,
			PublicIP:  hs.PublicNet.IPv4.IP.String(),
		})
	}

	if err := m.deployer.UpdatePrometheusConfig(0, controlPlanes[0], dataPlanes, appServers); err != nil {
		return fmt.Errorf("failed to update Prometheus configuration: %w", err)
	}

	// Update k6 targets if k6 is enabled and we scaled load balancers
	if m.config.K6.Enabled && role == "lb" {
		fmt.Println("[info] Updating k6 load balancer targets")
		if err := m.updateK6Targets(ctx, dataPlanes); err != nil {
			fmt.Printf("[warn] Failed to update k6 targets: %v\n", err)
		} else {
			fmt.Println("[info] ✓ k6 targets updated")
		}
	}

	fmt.Printf("[info] ✓ Successfully scaled %s to %d servers\n", poolName, len(hetznerServers))

	return nil
}

// updateK6Targets updates k6 load balancer targets by recreating the container
func (m *ManualScaler) updateK6Targets(ctx context.Context, dataPlanes []*models.Server) error {
	// Build LB_TARGETS comma-separated list
	var targets []string
	for _, dp := range dataPlanes {
		if dp.PrivateIP != "" {
			targets = append(targets, fmt.Sprintf("http://%s", dp.PrivateIP))
		}
	}
	lbTargets := strings.Join(targets, ",")

	// Get the actual docker network name (docker compose prefixes it)
	networkCmd := "docker network ls --filter name=apisix --format '{{.Name}}' | head -1"
	networkName, err := m.sshClient.Execute(networkCmd)
	if err != nil || networkName == "" {
		networkName = "harbor_apisix" // Default fallback
	}
	networkName = strings.TrimSpace(networkName)

	// Remove existing k6 container
	removeCmd := "docker rm -f k6 2>/dev/null || true"
	if _, err := m.sshClient.Execute(removeCmd); err != nil {
		return fmt.Errorf("failed to remove k6 container: %w", err)
	}

	// Set defaults for k6 config
	rate := m.config.K6.Rate
	if rate == 0 {
		rate = 10
	}
	duration := m.config.K6.Duration
	if duration == "" {
		duration = "30s"
	}
	preallocatedVUs := m.config.K6.PreallocatedVUs
	if preallocatedVUs == 0 {
		preallocatedVUs = 10
	}
	maxVUs := m.config.K6.MaxVUs
	if maxVUs == 0 {
		maxVUs = 100
	}
	targetPath := m.config.K6.TargetPath
	if targetPath == "" {
		targetPath = "/"
	}
	connectionTimeout := m.config.K6.ConnectionTimeout
	if connectionTimeout == "" {
		connectionTimeout = "10s"
	}
	requestTimeout := m.config.K6.RequestTimeout
	if requestTimeout == "" {
		requestTimeout = "30s"
	}
	gracefulStop := m.config.K6.GracefulStop
	if gracefulStop == "" {
		gracefulStop = "30s"
	}

	// Run k6 with updated targets
	runCmd := fmt.Sprintf(`docker run -d --name k6 \
		--network %s \
		--restart always \
		-e "LB_TARGETS=%s" \
		-e "RATE=%d" \
		-e "DURATION=%s" \
		-e "PREALLOCATED_VUS=%d" \
		-e "MAX_VUS=%d" \
		-e "TARGET_PATH=%s" \
		-e "CONNECTION_TIMEOUT=%s" \
		-e "REQUEST_TIMEOUT=%s" \
		-e "GRACEFUL_STOP=%s" \
		-v /opt/harbor/k6:/scripts:ro \
		grafana/k6:latest run /scripts/loadtest.js`,
		networkName,
		lbTargets,
		rate,
		duration,
		preallocatedVUs,
		maxVUs,
		targetPath,
		connectionTimeout,
		requestTimeout,
		gracefulStop)

	output, err := m.sshClient.Execute(runCmd)
	if err != nil {
		return fmt.Errorf("failed to start k6 container: %w (output: %s)", err, output)
	}

	return nil
}
