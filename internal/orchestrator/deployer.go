package orchestrator

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/dihmeetree/harbor/internal/apisix"
	"github.com/dihmeetree/harbor/internal/config"
	"github.com/dihmeetree/harbor/internal/docker"
	"github.com/dihmeetree/harbor/internal/hetzner"
	"github.com/dihmeetree/harbor/internal/ssh"
	"github.com/dihmeetree/harbor/pkg/models"
)

const (
	// MaxConcurrentDeployments limits the number of parallel deployments to prevent resource exhaustion
	MaxConcurrentDeployments = 10
)

// Deployer handles service deployment to servers
type Deployer struct {
	config         *config.Config
	hetzner        *hetzner.Client
	dockerClient   *docker.Installer
	privateKeyPath string
	sshUser        string // SSH username for connecting to servers
}

// NewDeployer creates a new Deployer instance for managing service deployments.
// The deployer uses the provided configuration, Hetzner API token, and SSH private key
// to deploy and manage services across the infrastructure.
func NewDeployer(cfg *config.Config, hetznerToken string, privateKeyPath string) *Deployer {
	return &Deployer{
		config:         cfg,
		hetzner:        hetzner.New(hetznerToken),
		dockerClient:   docker.New(),
		privateKeyPath: privateKeyPath,
		sshUser:        "core", // Flatcar Linux SSH user
	}
}

// DeployServicesOnly redeploys services to existing infrastructure without provisioning new servers.
// It queries Hetzner for current servers, stops existing containers, and redeploys all services
// (control plane, data planes, app servers) with the latest configuration from harbor.yaml.
// This is useful for updating service configuration, restarting services, or recovering from failures.
func (d *Deployer) DeployServicesOnly(ctx context.Context) error {
	d.log("info", "Starting service redeployment")

	// Get servers from Hetzner by role labels
	d.log("info", "Querying Hetzner API for current servers...")

	controlPlanes, dataPlanes, appServers, err := d.getServersFromHetzner(ctx)
	if err != nil {
		return fmt.Errorf("failed to get servers from Hetzner: %w", err)
	}

	if len(controlPlanes) == 0 {
		return fmt.Errorf("no control plane server found in Hetzner")
	}

	controlPlane := controlPlanes[0]
	d.log("info", fmt.Sprintf("Found: 1 control plane, %d data planes, %d app servers", len(dataPlanes), len(appServers)))

	// Stop and remove existing containers on all servers
	d.log("info", "Stopping existing containers...")
	allServers := append(append(controlPlanes, dataPlanes...), appServers...)
	for _, server := range allServers {
		sshClient, err := ssh.New(server.PublicIP, d.sshUser, d.privateKeyPath)
		if err != nil {
			d.log("warn", fmt.Sprintf("Failed to connect to %s: %v", server.Name, err))
			continue
		}
		// Stop containers and remove volumes
		_, _ = sshClient.Execute("cd /var/lib/harbor && docker compose down -v 2>/dev/null || true")
		// Also remove k6 container if it exists (not managed by compose)
		_, _ = sshClient.Execute("docker rm -f k6 2>/dev/null || true")
		sshClient.Close()
	}

	// Deploy control plane (pass data planes for k6 target configuration)
	d.log("info", "Deploying control plane services")
	if err := d.DeployControlPlaneWithServers(controlPlane, dataPlanes, appServers); err != nil {
		return fmt.Errorf("failed to deploy control plane: %w", err)
	}

	// Wait for control plane to be ready
	d.log("info", "Waiting for control plane to be ready...")
	if err := d.waitForControlPlane(controlPlane); err != nil {
		return fmt.Errorf("control plane failed to become ready: %w", err)
	}
	d.log("info", "✓ Control plane is ready")

	// Deploy data planes and app servers in parallel with concurrency limit
	d.log("info", fmt.Sprintf("Deploying %d data planes and %d app servers (max %d concurrent)", len(dataPlanes), len(appServers), MaxConcurrentDeployments))
	var wg sync.WaitGroup
	errChan := make(chan error, len(dataPlanes)+len(appServers))

	// Create semaphore to limit concurrent deployments
	semaphore := make(chan struct{}, MaxConcurrentDeployments)

	// Deploy data planes
	for _, server := range dataPlanes {
		wg.Add(1)
		go func(srv *models.Server) {
			defer wg.Done()
			// Acquire semaphore
			semaphore <- struct{}{}
			defer func() { <-semaphore }()

			d.log("info", fmt.Sprintf("Deploying data plane on %s", srv.Name))
			if err := d.DeployDataPlane(srv, controlPlane.PrivateIP); err != nil {
				errChan <- fmt.Errorf("failed to deploy data plane on %s: %w", srv.Name, err)
				return
			}
			d.log("info", fmt.Sprintf("✓ Data plane deployed on %s", srv.Name))
		}(server)
	}

	// Deploy app servers (includes monitoring)
	for _, server := range appServers {
		wg.Add(1)
		go func(srv *models.Server) {
			defer wg.Done()
			// Acquire semaphore
			semaphore <- struct{}{}
			defer func() { <-semaphore }()

			d.log("info", fmt.Sprintf("Deploying app on %s", srv.Name))
			if err := d.DeployAppServer(srv); err != nil {
				errChan <- fmt.Errorf("failed to deploy app on %s: %w", srv.Name, err)
				return
			}
			d.log("info", fmt.Sprintf("✓ App deployed on %s", srv.Name))

			// Deploy monitoring stack
			d.log("info", fmt.Sprintf("Deploying monitoring on %s", srv.Name))
			if err := d.DeployAppMonitoring(srv); err != nil {
				errChan <- fmt.Errorf("failed to deploy monitoring on %s: %w", srv.Name, err)
				return
			}
			d.log("info", fmt.Sprintf("✓ Monitoring deployed on %s", srv.Name))
		}(server)
	}

	// Wait for all deployments to complete
	wg.Wait()
	close(errChan)

	// Check for errors
	var errors []error
	for err := range errChan {
		errors = append(errors, err)
	}

	if len(errors) > 0 {
		return errors[0]
	}

	// Wait for data plane services to be ready
	d.log("info", "Waiting for data plane services to be ready...")
	if err := d.waitForDataPlanes(dataPlanes); err != nil {
		return fmt.Errorf("data planes failed to become ready: %w", err)
	}
	d.log("info", "✓ All data planes are ready")

	// Configure APISIX
	d.log("info", "Configuring APISIX")
	if err := d.ConfigureAPISIX(controlPlane, appServers); err != nil {
		return fmt.Errorf("failed to configure APISIX: %w", err)
	}

	d.log("info", "Service redeployment completed successfully")
	return nil
}

// RedeployAppServers redeploys only the app servers using blue-green deployment strategy.
// This performs a rolling deployment (one server at a time) with zero downtime by:
// 1. Deploying new container on alternate port (blue=80, green=8080)
// 2. Waiting for health checks to pass on new container
// 3. Updating APISIX to route traffic to new port
// 4. Stopping old container after traffic migration
//
// If serviceName is provided, only that specific service from docker-compose will be redeployed.
// If serviceName is empty, all services will be redeployed.
func (d *Deployer) RedeployAppServers(ctx context.Context, serviceName string) error {
	if serviceName != "" {
		d.log("info", fmt.Sprintf("Starting zero-downtime redeployment for service '%s' (blue-green strategy)", serviceName))
	} else {
		d.log("info", "Starting zero-downtime app server redeployment (blue-green strategy)")
	}

	// Get servers from Hetzner
	d.log("info", "Querying Hetzner API for app servers...")
	controlPlanes, _, appServers, err := d.getServersFromHetzner(ctx)
	if err != nil {
		return fmt.Errorf("failed to get servers from Hetzner: %w", err)
	}

	if len(appServers) == 0 {
		return fmt.Errorf("no app servers found in Hetzner")
	}

	if len(controlPlanes) == 0 {
		return fmt.Errorf("no control plane server found in Hetzner")
	}

	controlPlane := controlPlanes[0]
	d.log("info", fmt.Sprintf("Found %d app server(s) to redeploy (blue-green rolling deployment)", len(appServers)))

	// Log the compose file being used
	if d.config.App.ComposeFile != "" {
		d.log("info", fmt.Sprintf("Using docker-compose file: %s", d.config.App.ComposeFile))
	}

	// Track server:port mappings for all servers
	serverPorts := make(map[string]int)

	// Initialize with current server ports
	for _, server := range appServers {
		currentPort, err := d.discoverCurrentAppPort(server)
		if err != nil {
			d.log("warn", fmt.Sprintf("Failed to discover port for %s, defaulting to 80: %v", server.Name, err))
			currentPort = 80
		}
		if currentPort == 0 {
			// No container running, use port 80 as default
			currentPort = 80
		}
		serverPorts[server.PrivateIP] = currentPort
		d.log("info", fmt.Sprintf("  %s currently on port %d", server.Name, currentPort))
	}

	// Deploy app servers one at a time (blue-green rolling deployment)
	for i, server := range appServers {
		d.log("info", fmt.Sprintf("[%d/%d] Blue-green redeploying %s", i+1, len(appServers), server.Name))

		// Discover current port (blue)
		currentPort := serverPorts[server.PrivateIP]

		// Determine new port (green) - alternate between 80 and 8080
		var newPort int
		if currentPort == 80 {
			newPort = 8080
		} else {
			newPort = 80
		}

		d.log("info", fmt.Sprintf("  Current port (blue): %d, New port (green): %d", currentPort, newPort))

		// Step 1: Deploy new container on green port
		if serviceName != "" {
			d.log("info", fmt.Sprintf("  Deploying service '%s' on port %d (green)", serviceName, newPort))
		} else {
			d.log("info", fmt.Sprintf("  Deploying new version on port %d (green)", newPort))
		}
		if err := d.DeployAppServerOnPort(server, newPort, serviceName); err != nil {
			return fmt.Errorf("failed to deploy app on port %d for %s: %w", newPort, server.Name, err)
		}

		// Step 2: Wait for health check on new port
		d.log("info", fmt.Sprintf("  Waiting for health check on port %d...", newPort))
		if err := d.waitForServerHealthOnPort(server, newPort); err != nil {
			// Health check failed - cleanup new container and abort
			d.log("error", fmt.Sprintf("Health check failed on port %d: %v", newPort, err))
			d.log("info", fmt.Sprintf("  Cleaning up failed deployment on port %d", newPort))
			_ = d.stopAppContainersOnPort(server, newPort)
			return fmt.Errorf("server %s failed health check on port %d: %w", server.Name, newPort, err)
		}
		d.log("info", fmt.Sprintf("  ✓ Health check passed on port %d", newPort))

		// Step 3: Add new port to APISIX upstreams (additive update - both ports active)
		d.log("info", fmt.Sprintf("  Adding port %d (green) to APISIX upstreams alongside port %d (blue)", newPort, currentPort))

		// Update upstreams to include both ports (no service interruption)
		if err := d.updateAPISIXUpstreamsWithDualPorts(controlPlane, serverPorts, server.PrivateIP, currentPort, newPort); err != nil {
			// Failed to update APISIX - rollback by stopping new container
			d.log("error", fmt.Sprintf("Failed to add new upstream: %v", err))
			d.log("info", fmt.Sprintf("  Rolling back - stopping new container on port %d", newPort))
			_ = d.stopAppContainersOnPort(server, newPort)
			return fmt.Errorf("failed to update upstreams for %s: %w", server.Name, err)
		}

		// Step 4: Verify APISIX has detected the new backend
		d.log("info", fmt.Sprintf("  Verifying APISIX has detected backend on port %d (green)", newPort))
		if err := d.verifyAPISIXUpstreamHasBackend(controlPlane, server.PrivateIP, newPort); err != nil {
			d.log("warn", fmt.Sprintf("Failed to verify APISIX backend (non-critical): %v", err))
			// Continue anyway - we'll remove the old backend which will force traffic to new one
		}

		// Step 5: Remove old port from APISIX upstreams (only new port remains)
		d.log("info", fmt.Sprintf("  Removing port %d (blue) from APISIX upstreams", currentPort))
		serverPorts[server.PrivateIP] = newPort
		if err := d.updateAPISIXUpstreamsWithServerPorts(controlPlane, serverPorts); err != nil {
			d.log("warn", fmt.Sprintf("Failed to remove old upstream (non-critical): %v", err))
			// Don't fail - new backend is already serving traffic
		}

		// Step 6: Wait for connections to drain from old container
		d.log("info", fmt.Sprintf("  Waiting for connections to drain from port %d (blue)", currentPort))
		time.Sleep(3 * time.Second)

		// Step 7: Stop old container on blue port
		d.log("info", fmt.Sprintf("  Stopping old container on port %d (blue)", currentPort))
		if err := d.stopAppContainersOnPort(server, currentPort); err != nil {
			d.log("warn", fmt.Sprintf("  Failed to stop old container on port %d: %v", currentPort, err))
			// Continue anyway - new container is already serving traffic
		}

		d.log("info", fmt.Sprintf("✓ %s redeployed successfully (now on port %d)", server.Name, newPort))
	}

	d.log("info", "✓ All app servers redeployed successfully using blue-green strategy")
	return nil
}

// Deploy deploys all services to freshly provisioned infrastructure.
// It orchestrates the complete deployment workflow: waits for servers to be SSH-accessible,
// installs Docker on all servers, deploys the control plane, and then deploys data planes
// and app servers in parallel. Finally, it configures APISIX routes and upstreams.
// This method is typically called after Provision() creates the infrastructure.
func (d *Deployer) Deploy(ctx context.Context) error {
	d.log("info", "Starting service deployment")

	// Get servers from Hetzner
	controlPlanes, dataPlanes, appServers, err := d.getServersFromHetzner(ctx)
	if err != nil {
		return fmt.Errorf("failed to get servers from Hetzner: %w", err)
	}

	if len(controlPlanes) == 0 {
		return fmt.Errorf("no control plane server found")
	}

	allServers := append(append(controlPlanes, dataPlanes...), appServers...)

	// Wait for servers to be ready (SSH accessible)
	d.log("info", "Waiting for all servers to be SSH accessible...")
	if err := d.waitForServersReady(allServers); err != nil {
		return fmt.Errorf("servers failed to become ready: %w", err)
	}
	d.log("info", "✓ All servers are SSH accessible")

	// Install docker-compose on all servers in parallel
	d.log("info", "Installing docker-compose on all servers")
	if err := d.InstallDocker(allServers); err != nil {
		return fmt.Errorf("failed to install docker-compose: %w", err)
	}

	controlPlane := controlPlanes[0]

	// Deploy control plane
	d.log("info", "Deploying control plane services")
	if err := d.DeployControlPlane(controlPlane); err != nil {
		return fmt.Errorf("failed to deploy control plane: %w", err)
	}

	// Wait for control plane to be ready
	d.log("info", "Waiting for control plane to be ready...")
	if err := d.waitForControlPlane(controlPlane); err != nil {
		return fmt.Errorf("control plane failed to become ready: %w", err)
	}
	d.log("info", "✓ Control plane is ready")

	// Deploy data planes and app servers in parallel with concurrency limit
	d.log("info", fmt.Sprintf("Deploying %d data planes and %d app servers (max %d concurrent)", len(dataPlanes), len(appServers), MaxConcurrentDeployments))
	var wg sync.WaitGroup
	errChan := make(chan error, len(dataPlanes)+len(appServers))

	// Create semaphore to limit concurrent deployments
	semaphore := make(chan struct{}, MaxConcurrentDeployments)

	// Deploy data planes
	for _, server := range dataPlanes {
		wg.Add(1)
		go func(srv *models.Server) {
			defer wg.Done()
			// Acquire semaphore
			semaphore <- struct{}{}
			defer func() { <-semaphore }()

			d.log("info", fmt.Sprintf("Deploying data plane on %s", srv.Name))
			if err := d.DeployDataPlane(srv, controlPlane.PrivateIP); err != nil {
				errChan <- fmt.Errorf("failed to deploy data plane on %s: %w", srv.Name, err)
				return
			}
			d.log("info", fmt.Sprintf("✓ Data plane deployed on %s", srv.Name))
		}(server)
	}

	// Deploy app servers (includes monitoring)
	for _, server := range appServers {
		wg.Add(1)
		go func(srv *models.Server) {
			defer wg.Done()
			// Acquire semaphore
			semaphore <- struct{}{}
			defer func() { <-semaphore }()

			d.log("info", fmt.Sprintf("Deploying app on %s", srv.Name))
			if err := d.DeployAppServer(srv); err != nil {
				errChan <- fmt.Errorf("failed to deploy app on %s: %w", srv.Name, err)
				return
			}
			d.log("info", fmt.Sprintf("✓ App deployed on %s", srv.Name))

			// Deploy monitoring stack
			d.log("info", fmt.Sprintf("Deploying monitoring on %s", srv.Name))
			if err := d.DeployAppMonitoring(srv); err != nil {
				errChan <- fmt.Errorf("failed to deploy monitoring on %s: %w", srv.Name, err)
				return
			}
			d.log("info", fmt.Sprintf("✓ Monitoring deployed on %s", srv.Name))
		}(server)
	}

	// Wait for all deployments to complete
	wg.Wait()
	close(errChan)

	// Check for errors
	var errors []error
	for err := range errChan {
		errors = append(errors, err)
	}

	if len(errors) > 0 {
		return errors[0]
	}

	// Wait for data plane services to be ready
	d.log("info", "Waiting for data plane services to be ready...")
	if err := d.waitForDataPlanes(dataPlanes); err != nil {
		return fmt.Errorf("data planes failed to become ready: %w", err)
	}
	d.log("info", "✓ All data planes are ready")

	// Configure APISIX
	d.log("info", "Configuring APISIX")
	if err := d.ConfigureAPISIX(controlPlane, appServers); err != nil {
		return fmt.Errorf("failed to configure APISIX: %w", err)
	}

	d.log("info", "Service deployment completed successfully")
	return nil
}

// InstallDocker installs Docker and docker-compose on the specified servers in parallel.
// It uses SSH to remotely execute the Docker installation script on Flatcar Linux servers.
// All installations run concurrently with error aggregation via error channel.
func (d *Deployer) InstallDocker(servers []*models.Server) error {
	var wg sync.WaitGroup
	errChan := make(chan error, len(servers))

	for _, server := range servers {
		wg.Add(1)
		go func(srv *models.Server) {
			defer wg.Done()

			d.log("info", fmt.Sprintf("Waiting for SSH on %s (%s)", srv.Name, srv.PublicIP))

			sshClient, err := ssh.WaitForConnection(srv.PublicIP, d.sshUser, d.privateKeyPath, 5*time.Minute)
			if err != nil {
				errChan <- fmt.Errorf("failed to connect to %s via SSH: %w", srv.Name, err)
				return
			}
			defer sshClient.Close()

			d.log("info", fmt.Sprintf("✓ SSH ready on %s", srv.Name))

			d.log("info", fmt.Sprintf("Installing docker-compose on %s", srv.Name))
			if err := d.dockerClient.Install(sshClient); err != nil {
				errChan <- fmt.Errorf("failed to install docker-compose on %s: %w", srv.Name, err)
				return
			}

			d.log("info", fmt.Sprintf("✓ docker-compose ready on %s", srv.Name))
		}(server)
	}

	// Wait for all installations to complete
	wg.Wait()
	close(errChan)

	// Check for errors
	var errors []error
	for err := range errChan {
		errors = append(errors, err)
	}

	if len(errors) > 0 {
		// Return the first error
		return errors[0]
	}

	d.log("info", "✓ docker-compose ready on all servers")
	return nil
}

// DeployControlPlane deploys all control plane services to the control plane server.
// It queries Hetzner for current data planes and app servers to configure k6 targets.
// This is a convenience wrapper around DeployControlPlaneWithServers.
func (d *Deployer) DeployControlPlane(server *models.Server) error {
	// Get servers from Hetzner
	ctx := context.Background()
	_, dataPlanes, appServers, err := d.getServersFromHetzner(ctx)
	if err != nil {
		return fmt.Errorf("failed to get servers from Hetzner: %w", err)
	}
	return d.DeployControlPlaneWithServers(server, dataPlanes, appServers)
}

// DeployControlPlaneWithServers deploys the complete control plane stack including APISIX,
// etcd, Prometheus, Grafana, autoscaler, and k6 load testing. It generates docker-compose
// configuration with Prometheus targets for all servers and k6 targets for data planes.
func (d *Deployer) DeployControlPlaneWithServers(server *models.Server, dataPlanes, appServers []*models.Server) error {
	sshClient, err := ssh.New(server.PublicIP, d.sshUser, d.privateKeyPath)
	if err != nil {
		return fmt.Errorf("failed to connect via SSH: %w", err)
	}
	defer sshClient.Close()

	// Create deployment directory
	if _, err := sshClient.Execute("sudo mkdir -p /var/lib/harbor && sudo chown -R $USER:$USER /var/lib/harbor"); err != nil {
		return fmt.Errorf("failed to create deployment directory: %w", err)
	}

	// Prepare k6 load balancer targets (all data plane private IPs)
	var k6LBTargets string
	if d.config.K6.Enabled {
		var targets []string
		for _, dp := range dataPlanes {
			targets = append(targets, fmt.Sprintf("http://%s", dp.PrivateIP))
		}
		// Join targets with comma separator for k6 script
		k6LBTargets = strings.Join(targets, ",")
	}

	// Apply k6 defaults
	k6Config := d.config.K6
	ApplyK6Defaults(&k6Config)

	// Prepare template data
	data := TemplateData{
		PrometheusPort:      d.config.Monitoring.Prometheus.Port,
		CAdvisorPort:        d.config.Monitoring.CAdvisor.Port,
		NodeExporterPort:    d.config.Monitoring.NodeExporter.Port,
		APIKey:              d.config.APISIX.APIKey,
		AutoscalerEnabled:   d.config.Autoscaler.Enabled,
		HetznerToken:        os.Getenv("HETZNER_API_TOKEN"),
		K6Enabled:           d.config.K6.Enabled,
		K6PreallocatedVUs:   k6Config.PreallocatedVUs,
		K6MaxVUs:            k6Config.MaxVUs,
		K6Rate:              k6Config.Rate,
		K6Duration:          k6Config.Duration,
		K6TargetPath:        k6Config.TargetPath,
		K6ConnectionTimeout: k6Config.ConnectionTimeout,
		K6RequestTimeout:    k6Config.RequestTimeout,
		K6GracefulStop:      k6Config.GracefulStop,
		K6LBTargets:         k6LBTargets,
		K6CPULimit:          k6Config.CPULimit,
		K6MemoryLimit:       k6Config.MemoryLimit,
	}

	// Render and deploy docker-compose
	composeContent, err := RenderTemplate(ControlPlaneTemplate, data)
	if err != nil {
		return fmt.Errorf("failed to render control plane template: %w", err)
	}

	if err := sshClient.WriteFile("/var/lib/harbor/docker-compose.yml", composeContent); err != nil {
		return fmt.Errorf("failed to write docker-compose.yml: %w", err)
	}

	// Render and deploy APISIX config
	apisixConfig, err := RenderTemplate(APISIXControlPlaneConfigTemplate, data)
	if err != nil {
		return fmt.Errorf("failed to render APISIX config: %w", err)
	}

	if err := sshClient.WriteFile("/var/lib/harbor/apisix-control.yaml", apisixConfig); err != nil {
		return fmt.Errorf("failed to write APISIX config: %w", err)
	}

	// Render and deploy Prometheus config
	// Use the dataPlanes and appServers passed as parameters
	dataPlaneIPs := make([]string, len(dataPlanes))
	for i, dp := range dataPlanes {
		dataPlaneIPs[i] = dp.PrivateIP
	}

	appServerIPs := make([]string, len(appServers))
	for i, as := range appServers {
		appServerIPs[i] = as.PrivateIP
	}

	prometheusData := TemplateData{
		DataPlaneIPs: dataPlaneIPs,
		AppServerIPs: appServerIPs,
		DataPlanes:   dataPlanes,
		AppServers:   appServers,
	}

	prometheusConfig, err := RenderTemplate(PrometheusConfigTemplate, prometheusData)
	if err != nil {
		return fmt.Errorf("failed to render Prometheus config: %w", err)
	}

	if err := sshClient.WriteFile("/var/lib/harbor/prometheus.yml", prometheusConfig); err != nil {
		return fmt.Errorf("failed to write Prometheus config: %w", err)
	}

	// Copy Grafana configuration files
	if _, err := os.Stat("grafana"); err == nil {
		d.log("info", "Copying Grafana configuration to control plane...")

		// Create Grafana directories
		if _, err := sshClient.Execute("sudo mkdir -p /var/lib/harbor/grafana/config && sudo chown -R $USER:$USER /var/lib/harbor"); err != nil {
			return fmt.Errorf("failed to create grafana config directory: %w", err)
		}

		// Copy grafana.ini
		if err := sshClient.CopyFile("grafana/config/grafana.ini", "/var/lib/harbor/grafana/config/grafana.ini"); err != nil {
			return fmt.Errorf("failed to copy grafana.ini: %w", err)
		}

		// Copy provisioning directory
		if err := sshClient.CopyDir("grafana/provisioning", "/var/lib/harbor/grafana/provisioning"); err != nil {
			return fmt.Errorf("failed to copy grafana provisioning directory: %w", err)
		}

		// Copy dashboards directory
		if err := sshClient.CopyDir("grafana/dashboards", "/var/lib/harbor/grafana/dashboards"); err != nil {
			return fmt.Errorf("failed to copy grafana dashboards directory: %w", err)
		}

		d.log("info", "✓ Grafana configuration copied to control plane")
	}

	// Copy harbor config for autoscaler
	if d.config.Autoscaler.Enabled {
		configPath := "harbor.yaml"
		if _, err := os.Stat(configPath); err == nil {
			d.log("info", "Copying harbor config for autoscaler...")
			if err := sshClient.CopyFile(configPath, "/var/lib/harbor/config.yaml"); err != nil {
				return fmt.Errorf("failed to copy config file: %w", err)
			}
			d.log("info", "✓ Config copied to control plane")
		}
	}

	// Copy k6 load test script
	if d.config.K6.Enabled {
		k6ScriptPath := "k6/loadtest.js"
		if _, err := os.Stat(k6ScriptPath); err == nil {
			d.log("info", "Copying k6 load test script to control plane...")
			// Create k6 directory
			if _, err := sshClient.Execute("sudo mkdir -p /var/lib/harbor/k6 && sudo chown -R $USER:$USER /var/lib/harbor"); err != nil {
				return fmt.Errorf("failed to create k6 directory: %w", err)
			}
			if err := sshClient.CopyFile(k6ScriptPath, "/var/lib/harbor/k6/loadtest.js"); err != nil {
				return fmt.Errorf("failed to copy k6 script: %w", err)
			}
			d.log("info", "✓ k6 script copied to control plane")
		} else {
			d.log("warn", "k6 enabled but k6/loadtest.js not found")
		}
	}

	// Copy APISIX plugins directory
	if _, err := os.Stat("apisix/plugins"); err == nil {
		d.log("info", "Copying APISIX plugins to control plane...")
		if err := sshClient.CopyDir("apisix/plugins", "/var/lib/harbor/apisix/plugins"); err != nil {
			return fmt.Errorf("failed to copy plugins directory: %w", err)
		}
		d.log("info", "✓ Plugins copied to control plane")
	} else {
		d.log("warn", "No apisix/plugins directory found, skipping plugin copy")
	}

	// Start services
	if err := d.dockerClient.ComposeUp(sshClient, "/var/lib/harbor"); err != nil {
		return fmt.Errorf("failed to start services: %w", err)
	}

	return nil
}

// DeployDataPlane deploys APISIX data plane services to a load balancer server.
// It configures the data plane to connect to the control plane's etcd instance and
// deploys monitoring agents (cAdvisor, node-exporter) for metrics collection.
func (d *Deployer) DeployDataPlane(server *models.Server, controlPlaneIP string) error {
	sshClient, err := ssh.New(server.PublicIP, d.sshUser, d.privateKeyPath)
	if err != nil {
		return fmt.Errorf("failed to connect via SSH: %w", err)
	}
	defer sshClient.Close()

	// Create deployment directory
	if _, err := sshClient.Execute("sudo mkdir -p /var/lib/harbor && sudo chown -R $USER:$USER /var/lib/harbor"); err != nil {
		return fmt.Errorf("failed to create deployment directory: %w", err)
	}

	// Prepare template data
	data := TemplateData{
		CAdvisorPort:     d.config.Monitoring.CAdvisor.Port,
		NodeExporterPort: d.config.Monitoring.NodeExporter.Port,
		ControlPlaneIP:   controlPlaneIP,
	}

	// Render and deploy docker-compose
	composeContent, err := RenderTemplate(DataPlaneTemplate, data)
	if err != nil {
		return fmt.Errorf("failed to render data plane template: %w", err)
	}

	if err := sshClient.WriteFile("/var/lib/harbor/docker-compose.yml", composeContent); err != nil {
		return fmt.Errorf("failed to write docker-compose.yml: %w", err)
	}

	// Render and deploy APISIX config
	apisixConfig, err := RenderTemplate(APISIXDataPlaneConfigTemplate, data)
	if err != nil {
		return fmt.Errorf("failed to render APISIX config: %w", err)
	}

	if err := sshClient.WriteFile("/var/lib/harbor/apisix-data.yaml", apisixConfig); err != nil {
		return fmt.Errorf("failed to write APISIX config: %w", err)
	}

	// Copy APISIX plugins directory
	if _, err := os.Stat("apisix/plugins"); err == nil {
		d.log("info", fmt.Sprintf("Copying APISIX plugins to %s...", server.Name))
		if err := sshClient.CopyDir("apisix/plugins", "/var/lib/harbor/apisix/plugins"); err != nil {
			return fmt.Errorf("failed to copy plugins directory: %w", err)
		}
		d.log("info", fmt.Sprintf("✓ Plugins copied to %s", server.Name))
	}

	// Start services
	if err := d.dockerClient.ComposeUp(sshClient, "/var/lib/harbor"); err != nil {
		return fmt.Errorf("failed to start services: %w", err)
	}

	return nil
}

// DeployAppServer deploys the user's application container to an app server.
// If a custom docker-compose.yml file is specified in the config, it will be used
// and all volume-mounted files will be copied to the server.
// This method does NOT deploy monitoring - use DeployAppMonitoring for that.
func (d *Deployer) DeployAppServer(server *models.Server) error {
	sshClient, err := ssh.New(server.PublicIP, d.sshUser, d.privateKeyPath)
	if err != nil {
		return fmt.Errorf("failed to connect via SSH: %w", err)
	}
	defer sshClient.Close()

	// Create deployment directory
	if _, err := sshClient.Execute("sudo mkdir -p /var/lib/harbor && sudo chown -R $USER:$USER /var/lib/harbor"); err != nil {
		return fmt.Errorf("failed to create deployment directory: %w", err)
	}

	// Check if custom docker-compose file is specified
	if d.config.App.ComposeFile == "" {
		return fmt.Errorf("app.compose_file is required in config")
	}

	// Copy docker-compose.yml and all volume files
	if err := CopyComposeFilesAndVolumes(sshClient, d.config.App.ComposeFile, "/var/lib/harbor", server.Name); err != nil {
		return fmt.Errorf("failed to copy compose files and volumes: %w", err)
	}

	// Start user services
	if err := d.dockerClient.ComposeUp(sshClient, "/var/lib/harbor"); err != nil {
		return fmt.Errorf("failed to start services: %w", err)
	}

	return nil
}

// DeployAppMonitoring deploys the monitoring stack (cAdvisor + node-exporter) to an app server.
// This should be called during initial deployment and when scaling up, but not during redeploy-app.
func (d *Deployer) DeployAppMonitoring(server *models.Server) error {
	sshClient, err := ssh.New(server.PublicIP, d.sshUser, d.privateKeyPath)
	if err != nil {
		return fmt.Errorf("failed to connect via SSH: %w", err)
	}
	defer sshClient.Close()

	// Render monitoring compose template
	monitoringCompose, err := RenderAppMonitoringTemplate(d.config.Monitoring.CAdvisor.Port, d.config.Monitoring.NodeExporter.Port)
	if err != nil {
		return fmt.Errorf("failed to render monitoring template: %w", err)
	}

	// Write monitoring compose file
	if err := sshClient.WriteFile("/var/lib/harbor/docker-compose.monitoring.yml", monitoringCompose); err != nil {
		return fmt.Errorf("failed to write monitoring compose file: %w", err)
	}

	// Start monitoring services
	composeCmd := "cd /var/lib/harbor && PATH=/opt/bin:$PATH docker-compose -f docker-compose.monitoring.yml up -d"
	if _, err := sshClient.Execute(composeCmd); err != nil {
		return fmt.Errorf("failed to start monitoring services: %w", err)
	}

	return nil
}

// ConfigureAPISIX configures APISIX routes, upstreams, global rules, and SSL certificates.
// It waits for the APISIX Admin API to be ready, then creates upstreams pointing to app servers,
// configures routes from harbor.yaml, and optionally sets up SSL/TLS certificates.
func (d *Deployer) ConfigureAPISIX(controlPlane *models.Server, appServers []*models.Server) error {
	// Connect to control plane via SSH to run curl commands locally
	sshClient, err := ssh.New(controlPlane.PublicIP, d.sshUser, d.privateKeyPath)
	if err != nil {
		return fmt.Errorf("failed to connect to control plane via SSH: %w", err)
	}
	defer sshClient.Close()

	// Wait for APISIX to be ready
	d.log("info", "Waiting for APISIX Admin API to be ready...")
	adminURL := "http://127.0.0.1:9180"
	apiKey := d.config.APISIX.APIKey

	for i := range 30 {
		cmd := fmt.Sprintf("curl -s -o /dev/null -w '%%{http_code}' %s/apisix/admin/routes -H 'X-API-KEY: %s'", adminURL, apiKey)
		output, err := sshClient.Execute(cmd)
		if err == nil && output == "200" {
			d.log("info", "✓ APISIX Admin API is ready")
			break
		}
		if i == 29 {
			return fmt.Errorf("APISIX Admin API did not become ready after 60 seconds")
		}
		time.Sleep(2 * time.Second)
	}

	// Create APISIX client that executes via SSH
	client := apisix.NewWithSSH(adminURL, apiKey, sshClient)

	// Create upstreams
	for _, upstream := range d.config.APISIX.Upstreams {
		d.log("info", fmt.Sprintf("Creating upstream: %s", upstream.Name))

		// Build nodes map based on upstream type
		nodes := make(map[string]int)

		switch upstream.ID {
		case "grafana":
			// Grafana runs on control plane at port 3000
			nodes[fmt.Sprintf("%s:3000", controlPlane.PrivateIP)] = 1
		default:
			// Default: use app servers on port 80
			for _, server := range appServers {
				nodes[fmt.Sprintf("%s:80", server.PrivateIP)] = 1
			}
		}

		if err := client.CreateUpstream(upstream, nodes); err != nil {
			return fmt.Errorf("failed to create upstream %s: %w", upstream.Name, err)
		}
	}

	// Create global rules
	for _, rule := range d.config.APISIX.GlobalRules {
		d.log("info", fmt.Sprintf("Creating global rule: %s", rule.ID))
		if err := client.CreateGlobalRule(rule); err != nil {
			return fmt.Errorf("failed to create global rule %s: %w", rule.ID, err)
		}
	}

	// Create routes
	for _, route := range d.config.APISIX.Routes {
		d.log("info", fmt.Sprintf("Creating route: %s", route.Name))
		if err := client.CreateRoute(route); err != nil {
			return fmt.Errorf("failed to create route %s: %w", route.Name, err)
		}
	}

	// Create SSL certificates (if configured)
	if d.config.APISIX.SSL.CertPath != "" {
		d.log("info", "Configuring SSL certificates")

		certPEM, err := os.ReadFile(d.config.APISIX.SSL.CertPath)
		if err != nil {
			return fmt.Errorf("failed to read SSL cert: %w", err)
		}

		keyPEM, err := os.ReadFile(d.config.APISIX.SSL.KeyPath)
		if err != nil {
			return fmt.Errorf("failed to read SSL key: %w", err)
		}

		// Validate certificate and key
		if err := validateSSLCertAndKey(certPEM, keyPEM); err != nil {
			return fmt.Errorf("SSL certificate validation failed: %w", err)
		}

		var clientCA string
		if d.config.APISIX.SSL.ClientCAPath != "" {
			caBytes, err := os.ReadFile(d.config.APISIX.SSL.ClientCAPath)
			if err != nil {
				return fmt.Errorf("failed to read client CA: %w", err)
			}

			// Validate CA certificate
			if err := validateCACert(caBytes); err != nil {
				return fmt.Errorf("client CA validation failed: %w", err)
			}

			clientCA = string(caBytes)
		}

		if err := client.CreateSSL(d.config.APISIX.SSL, string(certPEM), string(keyPEM), clientCA); err != nil {
			return fmt.Errorf("failed to configure SSL: %w", err)
		}

		d.log("info", "✓ SSL certificates validated and configured")
	}

	d.log("info", "APISIX configuration completed")
	return nil
}

// UpdateAPISIXUpstreams updates only the upstream backend nodes in APISIX.
// This is used during scaling operations to add or remove app servers from the load balancer
// without recreating routes or other APISIX configuration.
func (d *Deployer) UpdateAPISIXUpstreams(controlPlane *models.Server, appServers []*models.Server) error {
	// Connect to control plane via SSH
	sshClient, err := ssh.New(controlPlane.PublicIP, d.sshUser, d.privateKeyPath)
	if err != nil {
		return fmt.Errorf("failed to connect to control plane: %w", err)
	}
	defer sshClient.Close()

	// Create APISIX client that executes via SSH
	adminURL := "http://127.0.0.1:9180"
	apiKey := d.config.APISIX.APIKey
	client := apisix.NewWithSSH(adminURL, apiKey, sshClient)

	// Update upstreams only
	for _, upstream := range d.config.APISIX.Upstreams {
		d.log("info", fmt.Sprintf("Updating upstream: %s", upstream.Name))

		// Build nodes map based on upstream type
		nodes := make(map[string]int)

		switch upstream.ID {
		case "grafana":
			// Grafana runs on control plane at port 3000
			nodes[fmt.Sprintf("%s:3000", controlPlane.PrivateIP)] = 1
		default:
			// Default: use app servers on port 80
			for _, server := range appServers {
				nodes[fmt.Sprintf("%s:80", server.PrivateIP)] = 1
			}
		}

		if err := client.CreateUpstream(upstream, nodes); err != nil {
			return fmt.Errorf("failed to update upstream %s: %w", upstream.Name, err)
		}
	}

	d.log("info", "✓ Upstreams updated")
	return nil
}

// updateAPISIXUpstreamsWithServerPorts updates APISIX upstreams with a specific list of app servers
// and their custom port mappings. This is used during blue-green deployments.
func (d *Deployer) updateAPISIXUpstreamsWithServerPorts(controlPlane *models.Server, serverPorts map[string]int) error {
	// Connect to control plane via SSH
	sshClient, err := ssh.New(controlPlane.PublicIP, d.sshUser, d.privateKeyPath)
	if err != nil {
		return fmt.Errorf("failed to connect to control plane: %w", err)
	}
	defer sshClient.Close()

	// Create APISIX client that executes via SSH
	adminURL := "http://127.0.0.1:9180"
	apiKey := d.config.APISIX.APIKey
	client := apisix.NewWithSSH(adminURL, apiKey, sshClient)

	// Update upstreams with the provided server:port mappings
	for _, upstream := range d.config.APISIX.Upstreams {
		// Skip non-app upstreams
		if upstream.ID == "grafana" {
			continue
		}

		// Build nodes map with custom ports
		nodes := make(map[string]int)
		for serverIP, port := range serverPorts {
			nodes[fmt.Sprintf("%s:%d", serverIP, port)] = 1
		}

		if err := client.CreateUpstream(upstream, nodes); err != nil {
			return fmt.Errorf("failed to update upstream %s: %w", upstream.Name, err)
		}
	}

	return nil
}

// updateAPISIXUpstreamsWithDualPorts updates APISIX upstreams to include both old and new ports
// for a specific server during blue-green deployment. This ensures zero downtime by having
// both backends available during the transition.
func (d *Deployer) updateAPISIXUpstreamsWithDualPorts(controlPlane *models.Server, serverPorts map[string]int, transitioningIP string, oldPort, newPort int) error {
	// Connect to control plane via SSH
	sshClient, err := ssh.New(controlPlane.PublicIP, d.sshUser, d.privateKeyPath)
	if err != nil {
		return fmt.Errorf("failed to connect to control plane: %w", err)
	}
	defer sshClient.Close()

	// Create APISIX client that executes via SSH
	adminURL := "http://127.0.0.1:9180"
	apiKey := d.config.APISIX.APIKey
	client := apisix.NewWithSSH(adminURL, apiKey, sshClient)

	// Update upstreams with both old and new ports for the transitioning server
	for _, upstream := range d.config.APISIX.Upstreams {
		// Skip non-app upstreams
		if upstream.ID == "grafana" {
			continue
		}

		// Build nodes map with all servers
		nodes := make(map[string]int)
		for serverIP, port := range serverPorts {
			if serverIP == transitioningIP {
				// For the transitioning server, add both old and new ports
				nodes[fmt.Sprintf("%s:%d", serverIP, oldPort)] = 1
				nodes[fmt.Sprintf("%s:%d", serverIP, newPort)] = 1
			} else {
				// For other servers, use their current port
				nodes[fmt.Sprintf("%s:%d", serverIP, port)] = 1
			}
		}

		if err := client.CreateUpstream(upstream, nodes); err != nil {
			return fmt.Errorf("failed to update upstream %s with dual ports: %w", upstream.Name, err)
		}
	}

	return nil
}

// UpdatePrometheusConfig updates the Prometheus scrape targets configuration.
// This is used during scaling operations to add or remove servers from Prometheus monitoring.
// After updating the configuration file, it triggers a hot reload of Prometheus.
func (d *Deployer) UpdatePrometheusConfig(controlPlane *models.Server, dataPlanes []*models.Server, appServers []*models.Server) error {
	// Connect to control plane via SSH
	sshClient, err := ssh.New(controlPlane.PublicIP, d.sshUser, d.privateKeyPath)
	if err != nil {
		return fmt.Errorf("failed to connect to control plane: %w", err)
	}
	defer sshClient.Close()

	// Build IP lists for template
	var dataPlaneIPs []string
	for _, server := range dataPlanes {
		dataPlaneIPs = append(dataPlaneIPs, server.PrivateIP)
	}

	var appServerIPs []string
	for _, server := range appServers {
		appServerIPs = append(appServerIPs, server.PrivateIP)
	}

	// Render Prometheus config
	prometheusData := TemplateData{
		DataPlaneIPs: dataPlaneIPs,
		AppServerIPs: appServerIPs,
		DataPlanes:   dataPlanes,
		AppServers:   appServers,
	}

	prometheusConfig, err := RenderTemplate(PrometheusConfigTemplate, prometheusData)
	if err != nil {
		return fmt.Errorf("failed to render Prometheus config: %w", err)
	}

	// Write new config to server
	if err := sshClient.WriteFile("/var/lib/harbor/prometheus.yml", prometheusConfig); err != nil {
		return fmt.Errorf("failed to write Prometheus config: %w", err)
	}

	// Reload Prometheus configuration
	d.log("info", "Reloading Prometheus configuration")
	reloadCmd := "curl -X POST http://localhost:9090/-/reload"
	if _, err := sshClient.Execute(reloadCmd); err != nil {
		return fmt.Errorf("failed to reload Prometheus: %w", err)
	}

	d.log("info", "✓ Prometheus configuration updated")
	return nil
}

// RestartK6 restarts the k6 load testing container with the latest configuration.
// It stops the existing k6 container, updates the configuration with current data plane targets,
// and starts a new k6 container with parameters from harbor.yaml (rate, duration, VUs, etc).
func (d *Deployer) RestartK6(ctx context.Context) error {
	// Get servers from Hetzner
	controlPlanes, dataPlanes, _, err := d.getServersFromHetzner(ctx)
	if err != nil {
		return fmt.Errorf("failed to get servers from Hetzner: %w", err)
	}
	if len(controlPlanes) == 0 {
		return fmt.Errorf("no control plane server found")
	}

	if len(dataPlanes) == 0 {
		return fmt.Errorf("no data plane servers found")
	}

	controlPlane := controlPlanes[0]

	// Connect to control plane via SSH
	sshClient, err := ssh.New(controlPlane.PublicIP, d.sshUser, d.privateKeyPath)
	if err != nil {
		return fmt.Errorf("failed to connect to control plane: %w", err)
	}
	defer sshClient.Close()

	// Build LB_TARGETS comma-separated list
	var targets []string
	for _, dp := range dataPlanes {
		if dp.PrivateIP != "" {
			targets = append(targets, fmt.Sprintf("http://%s", dp.PrivateIP))
		}
	}
	lbTargets := strings.Join(targets, ",")

	fmt.Printf("[info] Targeting %d data plane(s): %s\n", len(targets), lbTargets)

	// Get the actual docker network name (docker compose prefixes it)
	networkCmd := "docker network ls --filter name=apisix --format '{{.Name}}' | head -1"
	networkName, err := sshClient.Execute(networkCmd)
	if err != nil || networkName == "" {
		networkName = "harbor_apisix" // Default fallback
	}
	networkName = strings.TrimSpace(networkName)

	// Remove existing k6 container
	fmt.Println("[info] Stopping existing k6 container...")
	removeCmd := "docker rm -f k6 2>/dev/null || true"
	if _, err := sshClient.Execute(removeCmd); err != nil {
		return fmt.Errorf("failed to remove k6 container: %w", err)
	}

	// Apply defaults for k6 config
	k6Config := d.config.K6
	ApplyK6Defaults(&k6Config)

	// Run k6 with updated configuration
	fmt.Println("[info] Starting k6 with updated configuration...")
	fmt.Printf("[info]   Rate: %d req/s | VUs: %d-%d | Duration: %s | Path: %s\n",
		k6Config.Rate, k6Config.PreallocatedVUs, k6Config.MaxVUs, k6Config.Duration, k6Config.TargetPath)

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

	output, err := sshClient.Execute(runCmd)
	if err != nil {
		return fmt.Errorf("failed to start k6 container: %w (output: %s)", err, output)
	}

	return nil
}

// StopK6 stops and removes the k6 load testing container from the control plane.
// If the k6 container is not running, it returns without error. This is useful for
// temporarily halting load testing without removing configuration.
func (d *Deployer) StopK6(ctx context.Context) error {
	// Get control plane from Hetzner
	controlPlanes, _, _, err := d.getServersFromHetzner(ctx)
	if err != nil {
		return fmt.Errorf("failed to get control plane: %w", err)
	}
	if len(controlPlanes) == 0 {
		return fmt.Errorf("no control plane server found")
	}

	controlPlane := controlPlanes[0]

	// Connect to control plane via SSH
	sshClient, err := ssh.New(controlPlane.PublicIP, d.sshUser, d.privateKeyPath)
	if err != nil {
		return fmt.Errorf("failed to connect to control plane: %w", err)
	}
	defer sshClient.Close()

	// Check if k6 container exists
	checkCmd := "docker ps -a --filter name=k6 --format '{{.Names}}'"
	output, err := sshClient.Execute(checkCmd)
	if err != nil {
		return fmt.Errorf("failed to check k6 container: %w", err)
	}

	if strings.TrimSpace(output) == "" {
		fmt.Println("[info] k6 container not found (already stopped or never started)")
		return nil
	}

	// Stop and remove k6 container
	removeCmd := "docker rm -f k6"
	if _, err := sshClient.Execute(removeCmd); err != nil {
		return fmt.Errorf("failed to remove k6 container: %w", err)
	}

	return nil
}

// waitForControlPlane waits for control plane services to be ready
func (d *Deployer) waitForControlPlane(server *models.Server) error {
	sshClient, err := ssh.New(server.PublicIP, d.sshUser, d.privateKeyPath)
	if err != nil {
		return fmt.Errorf("failed to connect via SSH: %w", err)
	}
	defer sshClient.Close()

	// Wait for etcd to be ready
	d.log("info", "Checking etcd...")
	if err := d.waitForService(sshClient, "curl -sf http://127.0.0.1:2379/health", 60, 2*time.Second); err != nil {
		return fmt.Errorf("etcd failed to become ready: %w", err)
	}
	d.log("info", "✓ etcd is ready")

	// Wait for APISIX control plane to be ready
	d.log("info", "Checking APISIX control plane...")
	apiKey := d.config.APISIX.APIKey
	checkCmd := fmt.Sprintf("curl -sf -H 'X-API-KEY: %s' http://127.0.0.1:9180/apisix/admin/routes", apiKey)
	if err := d.waitForService(sshClient, checkCmd, 60, 2*time.Second); err != nil {
		return fmt.Errorf("APISIX control plane failed to become ready: %w", err)
	}
	d.log("info", "✓ APISIX control plane is ready")

	// Wait for Prometheus to be ready
	d.log("info", "Checking Prometheus...")
	if err := d.waitForService(sshClient, "curl -sf http://127.0.0.1:9090/-/ready", 30, 2*time.Second); err != nil {
		return fmt.Errorf("prometheus failed to become ready: %w", err)
	}
	d.log("info", "✓ Prometheus is ready")

	return nil
}

// waitForService polls a service health check command until it succeeds or times out
func (d *Deployer) waitForService(sshClient *ssh.Client, healthCheckCmd string, maxAttempts int, interval time.Duration) error {
	for range maxAttempts {
		_, err := sshClient.Execute(healthCheckCmd)
		if err == nil {
			return nil
		}
		time.Sleep(interval)
	}
	return fmt.Errorf("service did not become ready after %d attempts", maxAttempts)
}

// waitForServersReady waits for all servers to be SSH accessible
func (d *Deployer) waitForServersReady(servers []*models.Server) error {
	var wg sync.WaitGroup
	errChan := make(chan error, len(servers))

	for _, server := range servers {
		wg.Add(1)
		go func(srv *models.Server) {
			defer wg.Done()
			d.log("info", fmt.Sprintf("Waiting for SSH on %s...", srv.Name))

			// Try to connect with timeout
			client, err := ssh.WaitForConnection(srv.PublicIP, d.sshUser, d.privateKeyPath, 10*time.Minute)
			if err != nil {
				errChan <- fmt.Errorf("server %s not ready: %w", srv.Name, err)
				return
			}
			client.Close()

			d.log("info", fmt.Sprintf("✓ %s is SSH accessible", srv.Name))
		}(server)
	}

	wg.Wait()
	close(errChan)

	// Check for errors
	var errors []error
	for err := range errChan {
		errors = append(errors, err)
	}

	if len(errors) > 0 {
		return errors[0]
	}

	return nil
}

// waitForServerHealthOnPort waits for a single app server to respond to HTTP requests on a specific port
func (d *Deployer) waitForServerHealthOnPort(server *models.Server, port int) error {
	sshClient, err := ssh.New(server.PublicIP, d.sshUser, d.privateKeyPath)
	if err != nil {
		return fmt.Errorf("failed to connect via SSH: %w", err)
	}
	defer sshClient.Close()

	// Try to connect to the app server for up to 60 seconds
	for i := 0; i < 30; i++ {
		// Check if the service is responding on the specified port via private IP
		cmd := fmt.Sprintf("curl -s -o /dev/null -w '%%{http_code}' http://%s:%d --connect-timeout 2", server.PrivateIP, port)
		output, err := sshClient.Execute(cmd)
		if err == nil && (output == "200" || output == "301" || output == "302") {
			return nil // Health check passed
		}
		time.Sleep(2 * time.Second)
	}

	return fmt.Errorf("server did not become healthy on port %d after 60 seconds", port)
}

// discoverCurrentAppPort discovers which port the current app container is running on.
// Returns the port number (80 or 8080), or 0 if no container is running.
func (d *Deployer) discoverCurrentAppPort(server *models.Server) (int, error) {
	sshClient, err := ssh.New(server.PublicIP, d.sshUser, d.privateKeyPath)
	if err != nil {
		return 0, fmt.Errorf("failed to connect via SSH: %w", err)
	}
	defer sshClient.Close()

	// Check for containers running on port 80 or 8080
	// Use docker ps with format to get container ports
	checkCmd := `docker ps --format '{{.Ports}}' | grep -E '0.0.0.0:(80|8080)->' | head -1 | sed -E 's/.*0.0.0.0:([0-9]+).*/\1/'`
	output, err := sshClient.Execute(checkCmd)
	if err != nil || strings.TrimSpace(output) == "" {
		// No app container running
		return 0, nil
	}

	port := strings.TrimSpace(output)

	// Validate port is numeric and in valid range
	var portNum int
	if _, err := fmt.Sscanf(port, "%d", &portNum); err != nil {
		return 0, fmt.Errorf("invalid port format: %s", port)
	}

	// Only accept ports 80 or 8080 (our expected ports)
	switch portNum {
	case 80:
		return 80, nil
	case 8080:
		return 8080, nil
	default:
		return 0, fmt.Errorf("unexpected port discovered: %d (expected 80 or 8080)", portNum)
	}
}

// DeployAppServerOnPort deploys the user's application container to an app server on a specific port.
// This is used for blue-green deployments where we need to run the new container on an alternate port.
func (d *Deployer) DeployAppServerOnPort(server *models.Server, port int, serviceName string) error {
	sshClient, err := ssh.New(server.PublicIP, d.sshUser, d.privateKeyPath)
	if err != nil {
		return fmt.Errorf("failed to connect via SSH: %w", err)
	}
	defer sshClient.Close()

	// Create deployment directory
	if _, err := sshClient.Execute("sudo mkdir -p /var/lib/harbor && sudo chown -R $USER:$USER /var/lib/harbor"); err != nil {
		return fmt.Errorf("failed to create deployment directory: %w", err)
	}

	// Check if custom docker-compose file is specified
	if d.config.App.ComposeFile == "" {
		return fmt.Errorf("app.compose_file is required in config")
	}

	// Validate port is safe (80 or 8080 only)
	if port != 80 && port != 8080 {
		return fmt.Errorf("invalid port %d: only ports 80 and 8080 are supported", port)
	}

	// Copy docker-compose.yml and all volume files
	if err := CopyComposeFilesAndVolumes(sshClient, d.config.App.ComposeFile, "/var/lib/harbor", server.Name); err != nil {
		return fmt.Errorf("failed to copy compose files and volumes: %w", err)
	}

	// Modify the docker-compose.yml to use the specified port (in Go to avoid sed injection risks)
	d.log("info", fmt.Sprintf("  Modifying compose file to use port %d", port))

	// Read the compose file
	content, err := sshClient.Execute("cat /var/lib/harbor/docker-compose.yml")
	if err != nil {
		return fmt.Errorf("failed to read compose file: %w", err)
	}

	// Replace all port mappings (safe string replacement - no shell injection)
	modifiedContent := strings.ReplaceAll(content, `"80:80"`, fmt.Sprintf(`"%d:80"`, port))
	modifiedContent = strings.ReplaceAll(modifiedContent, `'80:80'`, fmt.Sprintf(`'%d:80'`, port))
	modifiedContent = strings.ReplaceAll(modifiedContent, `- 80:80`, fmt.Sprintf(`- %d:80`, port))

	// Write the modified content back
	if err := sshClient.WriteFile("/var/lib/harbor/docker-compose.yml", modifiedContent); err != nil {
		return fmt.Errorf("failed to write modified compose file: %w", err)
	}

	// Start user services (optionally filtered by service name)
	if serviceName != "" {
		// Deploy only the specified service using docker-compose up --no-deps
		d.log("info", fmt.Sprintf("  Starting service: %s", serviceName))
		composeCmd := fmt.Sprintf("cd /var/lib/harbor && PATH=/opt/bin:$PATH docker-compose up -d --no-deps --force-recreate %s", serviceName)
		if _, err := sshClient.Execute(composeCmd); err != nil {
			return fmt.Errorf("failed to start service %s: %w", serviceName, err)
		}
	} else {
		// Start all services
		if err := d.dockerClient.ComposeUp(sshClient, "/var/lib/harbor"); err != nil {
			return fmt.Errorf("failed to start services: %w", err)
		}
	}

	return nil
}

// stopAppContainersOnPort gracefully stops app containers running on a specific port.
// It sends SIGTERM to allow cleanup, waits for graceful shutdown, then removes containers.
func (d *Deployer) stopAppContainersOnPort(server *models.Server, port int) error {
	sshClient, err := ssh.New(server.PublicIP, d.sshUser, d.privateKeyPath)
	if err != nil {
		return fmt.Errorf("failed to connect via SSH: %w", err)
	}
	defer sshClient.Close()

	// Find container IDs listening on the specified port
	findCmd := fmt.Sprintf(`docker ps --format '{{.ID}} {{.Ports}}' | grep '0.0.0.0:%d->' | awk '{print $1}'`, port)
	containerIDs, err := sshClient.Execute(findCmd)
	if err != nil || strings.TrimSpace(containerIDs) == "" {
		// No containers found, nothing to do
		return nil
	}

	// Send SIGTERM for graceful shutdown (timeout: 10 seconds)
	d.log("info", fmt.Sprintf("  Sending SIGTERM to containers on port %d for graceful shutdown", port))
	stopCmd := fmt.Sprintf(`docker stop -t 10 %s`, strings.TrimSpace(containerIDs))
	if _, err := sshClient.Execute(stopCmd); err != nil {
		d.log("warn", fmt.Sprintf("  Failed to gracefully stop containers on port %d: %v", port, err))
		// Force kill as fallback
		d.log("info", fmt.Sprintf("  Force killing containers on port %d", port))
		killCmd := fmt.Sprintf(`docker kill %s`, strings.TrimSpace(containerIDs))
		_, _ = sshClient.Execute(killCmd)
	}

	// Wait a moment to ensure containers are fully stopped
	time.Sleep(1 * time.Second)

	// Verify containers are stopped
	checkCmd := fmt.Sprintf(`docker ps --format '{{.ID}}' | grep -E '%s'`, strings.ReplaceAll(strings.TrimSpace(containerIDs), "\n", "|"))
	stillRunning, _ := sshClient.Execute(checkCmd)
	if strings.TrimSpace(stillRunning) != "" {
		d.log("warn", fmt.Sprintf("  Some containers still running on port %d after stop", port))
	}

	// Remove stopped containers
	d.log("info", fmt.Sprintf("  Removing stopped containers on port %d", port))
	removeCmd := fmt.Sprintf(`docker rm -f %s`, strings.TrimSpace(containerIDs))
	if _, err := sshClient.Execute(removeCmd); err != nil {
		d.log("warn", fmt.Sprintf("  Failed to remove containers on port %d: %v", port, err))
	}

	return nil
}

// verifyAPISIXUpstreamHasBackend verifies that APISIX upstream includes the specified backend
// by checking the upstream configuration via the Admin API. Retries for up to 30 seconds.
func (d *Deployer) verifyAPISIXUpstreamHasBackend(controlPlane *models.Server, serverIP string, port int) error {
	// Connect to control plane via SSH
	sshClient, err := ssh.New(controlPlane.PublicIP, d.sshUser, d.privateKeyPath)
	if err != nil {
		return fmt.Errorf("failed to connect to control plane: %w", err)
	}
	defer sshClient.Close()

	// Create APISIX client that executes via SSH
	adminURL := "http://127.0.0.1:9180"
	apiKey := d.config.APISIX.APIKey
	client := apisix.NewWithSSH(adminURL, apiKey, sshClient)

	// Retry up to 30 seconds checking all app upstreams
	expectedBackend := fmt.Sprintf("%s:%d", serverIP, port)
	maxAttempts := 15
	interval := 2 * time.Second

	// Retry until backend is found in at least one upstream or timeout
	for i := range maxAttempts {
		foundInAnyUpstream := false

		// Check all app upstreams
		for _, upstream := range d.config.APISIX.Upstreams {
			// Skip non-app upstreams
			if upstream.ID == "grafana" {
				continue
			}

			upstreamData, err := client.GetUpstream(upstream.ID)
			if err != nil {
				// Failed to get this upstream, try next one
				continue
			}

			// Extract nodes from upstream data
			// Response format can be either:
			// 1. {"node": {"value": {"nodes": {...}}}} (etcd format)
			// 2. {"value": {"nodes": {...}}} (direct format)
			var nodes map[string]any

			// Try etcd format first
			if node, ok := upstreamData["node"].(map[string]interface{}); ok {
				if value, ok := node["value"].(map[string]interface{}); ok {
					if nodesMap, ok := value["nodes"].(map[string]interface{}); ok {
						nodes = nodesMap
					}
				}
			}

			// Try direct format
			if nodes == nil {
				if value, ok := upstreamData["value"].(map[string]interface{}); ok {
					if nodesMap, ok := value["nodes"].(map[string]interface{}); ok {
						nodes = nodesMap
					}
				}
			}

			// Check if our expected backend exists in nodes
			if nodes != nil {
				if _, exists := nodes[expectedBackend]; exists {
					d.log("info", fmt.Sprintf("  ✓ APISIX upstream %s has backend %s", upstream.ID, expectedBackend))
					foundInAnyUpstream = true
					break // Found in this upstream, no need to check others
				}
			}
		}

		// If we found the backend in at least one upstream, we're done
		if foundInAnyUpstream {
			return nil
		}

		// Not found yet, retry
		if i < maxAttempts-1 {
			time.Sleep(interval)
		}
	}

	// Timed out waiting for backend to appear
	return fmt.Errorf("backend %s not found in any app upstream after %d seconds", expectedBackend, maxAttempts*2)
}

// waitForDataPlanes waits for all data plane APISIX instances to be ready
func (d *Deployer) waitForDataPlanes(dataPlanes []*models.Server) error {
	var wg sync.WaitGroup
	errChan := make(chan error, len(dataPlanes))

	for _, server := range dataPlanes {
		wg.Add(1)
		go func(srv *models.Server) {
			defer wg.Done()
			d.log("info", fmt.Sprintf("Checking APISIX data plane on %s...", srv.Name))

			sshClient, err := ssh.New(srv.PublicIP, d.sshUser, d.privateKeyPath)
			if err != nil {
				errChan <- fmt.Errorf("failed to connect to %s: %w", srv.Name, err)
				return
			}
			defer sshClient.Close()

			// Wait for APISIX data plane to be ready (check if it's listening on port 80)
			checkCmd := "curl -sf -o /dev/null -w '%{http_code}' http://127.0.0.1:80/ || true"
			if err := d.waitForService(sshClient, checkCmd, 60, 2*time.Second); err != nil {
				errChan <- fmt.Errorf("APISIX data plane on %s failed to become ready: %w", srv.Name, err)
				return
			}

			d.log("info", fmt.Sprintf("✓ APISIX data plane on %s is ready", srv.Name))
		}(server)
	}

	wg.Wait()
	close(errChan)

	// Check for errors
	var errors []error
	for err := range errChan {
		errors = append(errors, err)
	}

	if len(errors) > 0 {
		return errors[0]
	}

	return nil
}

// DeployToServer deploys services to a single server based on its role.
// This method is primarily used by the autoscaler when adding new servers dynamically.
// It installs Docker and deploys either data plane or app services depending on the role label.
func (d *Deployer) DeployToServer(serverIP string, roleLabel string, controlPlaneIP string) error {
	// Connect via SSH
	sshClient, err := ssh.New(serverIP, d.sshUser, d.privateKeyPath)
	if err != nil {
		return fmt.Errorf("failed to connect via SSH: %w", err)
	}
	defer sshClient.Close()

	// Install Docker
	d.log("info", fmt.Sprintf("Installing Docker on %s...", serverIP))
	if err := d.dockerClient.Install(sshClient); err != nil {
		return fmt.Errorf("failed to install Docker: %w", err)
	}

	// Create a temporary server model for deployment
	server := &models.Server{
		PublicIP: serverIP,
		Name:     serverIP,
	}

	// Deploy services based on role
	switch roleLabel {
	case "lb":
		d.log("info", fmt.Sprintf("Deploying data plane services on %s...", serverIP))
		if err := d.DeployDataPlane(server, controlPlaneIP); err != nil {
			return fmt.Errorf("failed to deploy data plane: %w", err)
		}
	case "app":
		d.log("info", fmt.Sprintf("Deploying app services on %s...", serverIP))
		if err := d.DeployAppServer(server); err != nil {
			return fmt.Errorf("failed to deploy app server: %w", err)
		}

		// Deploy monitoring stack
		d.log("info", fmt.Sprintf("Deploying monitoring on %s...", serverIP))
		if err := d.DeployAppMonitoring(server); err != nil {
			return fmt.Errorf("failed to deploy monitoring: %w", err)
		}
	default:
		return fmt.Errorf("unsupported role: %s", roleLabel)
	}

	d.log("info", fmt.Sprintf("✓ Services deployed on %s", serverIP))
	return nil
}

func (d *Deployer) log(level, message string) {
	fmt.Printf("[%s] %s\n", level, message)
}

// getServersFromHetzner queries Hetzner API for servers by role labels
func (d *Deployer) getServersFromHetzner(ctx context.Context) (controlPlanes, dataPlanes, appServers []*models.Server, err error) {
	// Get control plane servers
	controlHetzner, err := d.hetzner.GetServersByLabel(ctx, "role", "control")
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to get control planes: %w", err)
	}

	controlPlanes = HcloudListToModels(controlHetzner)

	// Get data plane (load balancer) servers
	lbHetzner, err := d.hetzner.GetServersByLabel(ctx, "role", "lb")
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to get data planes: %w", err)
	}

	dataPlanes = HcloudListToModels(lbHetzner)

	// Get app servers
	appHetzner, err := d.hetzner.GetServersByLabel(ctx, "role", "app")
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to get app servers: %w", err)
	}

	appServers = HcloudListToModels(appHetzner)

	return controlPlanes, dataPlanes, appServers, nil
}

// validateSSLCertAndKey validates that the certificate and private key are valid and match each other.
// It also checks that the certificate is not expired.
func validateSSLCertAndKey(certPEM, keyPEM []byte) error {
	// Parse certificate
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return fmt.Errorf("failed to decode PEM certificate")
	}

	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return fmt.Errorf("failed to parse certificate: %w", err)
	}

	// Check if certificate is expired
	now := time.Now()
	if now.Before(cert.NotBefore) {
		return fmt.Errorf("certificate is not yet valid (valid from %s)", cert.NotBefore)
	}
	if now.After(cert.NotAfter) {
		return fmt.Errorf("certificate has expired (expired on %s)", cert.NotAfter)
	}

	// Verify certificate and key match
	_, err = tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return fmt.Errorf("certificate and private key do not match: %w", err)
	}

	return nil
}

// validateCACert validates that the CA certificate is valid and not expired.
func validateCACert(caPEM []byte) error {
	// Parse CA certificate
	block, _ := pem.Decode(caPEM)
	if block == nil {
		return fmt.Errorf("failed to decode PEM CA certificate")
	}

	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return fmt.Errorf("failed to parse CA certificate: %w", err)
	}

	// Check if certificate is expired
	now := time.Now()
	if now.Before(cert.NotBefore) {
		return fmt.Errorf("CA certificate is not yet valid (valid from %s)", cert.NotBefore)
	}
	if now.After(cert.NotAfter) {
		return fmt.Errorf("CA certificate has expired (expired on %s)", cert.NotAfter)
	}

	// Verify it's a CA certificate
	if !cert.IsCA {
		return fmt.Errorf("certificate is not a CA certificate")
	}

	return nil
}
