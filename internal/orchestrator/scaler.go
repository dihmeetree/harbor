package orchestrator

import (
	"context"
	"fmt"
	"strings"

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

	// Create SSH connection to control plane (Flatcar uses 'core' user)
	sshClient, err := ssh.New(controlPlaneIP, "core", sshKeyPath)
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
	baseName := m.config.Control.Name
	if idx := strings.LastIndex(baseName, "-"); idx != -1 {
		baseName = baseName[:idx]
	}

	// Get network from database
	netRepo := database.NewNetworkRepository(m.db)
	networks, err := netRepo.GetAll()
	if err != nil {
		return fmt.Errorf("failed to query networks from database: %w", err)
	}
	if len(networks) == 0 {
		return fmt.Errorf("no network found in database - infrastructure may not be properly deployed. Run 'harbor deploy' first")
	}
	network, err := m.hetznerClient.GetNetwork(ctx, networks[0].HetznerID)
	if err != nil {
		return fmt.Errorf("failed to get network (ID: %d) from Hetzner: %w", networks[0].HetznerID, err)
	}

	// Get firewall from database
	fwRepo := database.NewFirewallRepository(m.db)
	firewalls, err := fwRepo.GetAll()
	if err != nil {
		return fmt.Errorf("failed to query firewalls from database: %w", err)
	}
	if len(firewalls) == 0 {
		return fmt.Errorf("no firewall found in database - infrastructure may not be properly deployed. Run 'harbor deploy' first")
	}
	firewall, err := m.hetznerClient.GetFirewall(ctx, firewalls[0].HetznerID)
	if err != nil {
		return fmt.Errorf("failed to get firewall (ID: %d) from Hetzner: %w", firewalls[0].HetznerID, err)
	}

	// Get SSH key from database
	sshKeyRepo := database.NewSSHKeyRepository(m.db)
	sshKeys, err := sshKeyRepo.GetAll()
	if err != nil {
		return fmt.Errorf("failed to query SSH keys from database: %w", err)
	}
	if len(sshKeys) == 0 {
		return fmt.Errorf("no SSH key found in database - infrastructure may not be properly deployed. Run 'harbor deploy' first")
	}
	sshKey, err := m.hetznerClient.GetSSHKey(ctx, sshKeys[0].HetznerID)
	if err != nil {
		return fmt.Errorf("failed to get SSH key (ID: %d) from Hetzner: %w", sshKeys[0].HetznerID, err)
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
			}
		case "app":
			serverCfg = config.ServerConfig{
				Name:     fmt.Sprintf("%s-%s-%d", baseName, role, startIndex+i),
				Type:     m.config.App.ServerType,
				Location: m.config.App.Location,
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
	controlPlanes, err := serverRepo.GetByRole(models.RoleControlPlane)
	if err != nil {
		return fmt.Errorf("failed to get control plane from database: %w", err)
	}
	if len(controlPlanes) == 0 {
		return fmt.Errorf("no control plane found in database - infrastructure may not be properly deployed")
	}
	controlPlaneIP := controlPlanes[0].PrivateIP

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
		allAppServers := HcloudListToModels(hetznerServers)

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
	dataPlanes := HcloudListToModels(allDataPlanes)
	appServers := HcloudListToModels(allAppServers)

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

// createServerViaHetzner creates a server using the hetzner client
func (m *ManualScaler) createServerViaHetzner(ctx context.Context, cfg config.ServerConfig, role models.ServerRole, network *hcloud.Network, firewall *hcloud.Firewall, sshKey *hcloud.SSHKey) (*hcloud.Server, error) {
	return CreateServerWithDatabase(ctx, m.hetznerClient, m.db, cfg, m.config.SnapshotID, role, network, firewall, sshKey)
}

// ScaleDown removes the specified number of servers
func (m *ManualScaler) ScaleDown(ctx context.Context, role string, poolName string, count int) error {
	// Get all servers with this role from Hetzner
	hetznerServers, err := m.hetznerClient.GetServersByLabel(ctx, "role", role)
	if err != nil {
		return fmt.Errorf("failed to get servers from Hetzner: %w", err)
	}

	currentCount := len(hetznerServers)
	if currentCount < count {
		return fmt.Errorf("cannot remove %d servers, only %d exist", count, currentCount)
	}

	// Check minimum replica constraints from autoscaler config
	if m.config.Autoscaler.Enabled {
		remainingCount := currentCount - count
		if remainingCount < m.config.Autoscaler.MinReplicas {
			return fmt.Errorf("cannot scale down to %d servers: would violate minimum replica count of %d (configured in autoscaler settings)", remainingCount, m.config.Autoscaler.MinReplicas)
		}
	}

	// Determine database role for the given label
	var dbRole models.ServerRole
	switch role {
	case "lb":
		dbRole = models.RoleDataPlane
	case "app":
		dbRole = models.RoleApp
	default:
		return fmt.Errorf("unsupported role: %s", role)
	}

	serverRepo := database.NewServerRepository(m.db)

	fmt.Printf("[info] Removing %d %s server(s) (%d -> %d)\n", count, poolName, currentCount, currentCount-count)

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

		// Try to delete from database if it exists (using correct role)
		dbServers, err := serverRepo.GetByRole(dbRole)
		if err != nil {
			fmt.Printf("[warn] Failed to query database for server cleanup: %v\n", err)
		} else {
			found := false
			for _, dbSrv := range dbServers {
				if dbSrv.HetznerID == serverToRemove.ID {
					if err := serverRepo.Delete(dbSrv.ID); err != nil {
						fmt.Printf("[warn] Failed to delete server %s from database: %v\n", serverToRemove.Name, err)
					}
					found = true
					break
				}
			}
			if !found {
				fmt.Printf("[info] Server %s not found in database (may have been created by autoscaler)\n", serverToRemove.Name)
			}
		}

		fmt.Printf("[info] ✓ Server %s removed from Hetzner\n", serverToRemove.Name)

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
	controlPlanes, err := serverRepo.GetByRole(models.RoleControlPlane)
	if err != nil {
		return fmt.Errorf("failed to get control plane from database: %w", err)
	}
	if len(controlPlanes) == 0 {
		return fmt.Errorf("no control plane found in database")
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
		remainingAppServers := HcloudListToModels(hetznerServers)

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
	dataPlanes := HcloudListToModels(allDataPlanes)
	appServers := HcloudListToModels(allAppServers)

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

	// Apply defaults for k6 config
	k6Config := m.config.K6
	ApplyK6Defaults(&k6Config)

	// Run k6 with updated targets
	runCmd := fmt.Sprintf(`docker run -d --name k6 \
		--network %s \
		--restart always \
		-e "K6_PROMETHEUS_RW_SERVER_URL=http://prometheus:9090/api/v1/write" \
		-e "K6_PROMETHEUS_RW_TREND_STATS=p(95),p(99),min,max,avg" \
		-e "K6_PROMETHEUS_RW_PUSH_INTERVAL=5s" \
		-e "LB_TARGETS=%s" \
		-e "RATE=%d" \
		-e "DURATION=%s" \
		-e "PREALLOCATED_VUS=%d" \
		-e "MAX_VUS=%d" \
		-e "TARGET_PATH=%s" \
		-e "CONNECTION_TIMEOUT=%s" \
		-e "REQUEST_TIMEOUT=%s" \
		-e "GRACEFUL_STOP=%s" \
		-v /var/lib/harbor/k6:/scripts:ro \
		grafana/k6:latest run -o experimental-prometheus-rw /scripts/loadtest.js`,
		networkName,
		lbTargets,
		k6Config.Rate,
		k6Config.Duration,
		k6Config.PreallocatedVUs,
		k6Config.MaxVUs,
		k6Config.TargetPath,
		k6Config.ConnectionTimeout,
		k6Config.RequestTimeout,
		k6Config.GracefulStop)

	output, err := m.sshClient.Execute(runCmd)
	if err != nil {
		return fmt.Errorf("failed to start k6 container: %w (output: %s)", err, output)
	}

	return nil
}
