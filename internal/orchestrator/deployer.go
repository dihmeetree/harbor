package orchestrator

import (
	"context"
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

	// Deploy data planes in parallel
	d.log("info", fmt.Sprintf("Deploying %d data planes in parallel", len(dataPlanes)))
	var wg sync.WaitGroup
	errChan := make(chan error, len(dataPlanes)+len(appServers))

	for _, server := range dataPlanes {
		wg.Add(1)
		go func(srv *models.Server) {
			defer wg.Done()
			d.log("info", fmt.Sprintf("Deploying data plane on %s", srv.Name))
			if err := d.DeployDataPlane(srv, controlPlane.PrivateIP); err != nil {
				errChan <- fmt.Errorf("failed to deploy data plane on %s: %w", srv.Name, err)
				return
			}
			d.log("info", fmt.Sprintf("✓ Data plane deployed on %s", srv.Name))
		}(server)
	}

	// Deploy app servers in parallel (includes monitoring)
	d.log("info", fmt.Sprintf("Deploying %d app servers in parallel", len(appServers)))
	for _, server := range appServers {
		wg.Add(1)
		go func(srv *models.Server) {
			defer wg.Done()
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

// RedeployAppServers redeploys only the app servers by stopping containers and redeploying with docker-compose.
// This performs a rolling deployment (one server at a time) to ensure zero downtime.
func (d *Deployer) RedeployAppServers(ctx context.Context) error {
	d.log("info", "Starting zero-downtime app server redeployment")

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
	d.log("info", fmt.Sprintf("Found %d app server(s) to redeploy (rolling deployment)", len(appServers)))

	// Log the compose file being used
	if d.config.App.ComposeFile != "" {
		d.log("info", fmt.Sprintf("Using docker-compose file: %s", d.config.App.ComposeFile))
	}

	// Deploy app servers one at a time (rolling deployment for zero downtime)
	for i, server := range appServers {
		d.log("info", fmt.Sprintf("[%d/%d] Redeploying %s", i+1, len(appServers), server.Name))

		// Remove this server from APISIX upstreams (so no traffic is routed to it)
		d.log("info", fmt.Sprintf("  Removing %s from APISIX upstreams", server.Name))
		remainingServers := make([]*models.Server, 0, len(appServers)-1)
		for _, s := range appServers {
			if s.Name != server.Name {
				remainingServers = append(remainingServers, s)
			}
		}
		if err := d.updateAPISIXUpstreamsWithServers(controlPlane, remainingServers); err != nil {
			return fmt.Errorf("failed to remove %s from upstreams: %w", server.Name, err)
		}

		// Wait a moment for connections to drain
		time.Sleep(2 * time.Second)

		// Connect to server
		sshClient, err := ssh.New(server.PublicIP, d.sshUser, d.privateKeyPath)
		if err != nil {
			return fmt.Errorf("failed to connect to %s: %w", server.Name, err)
		}

		// Stop containers on this server
		d.log("info", fmt.Sprintf("  Stopping containers on %s", server.Name))
		_, _ = sshClient.Execute("cd /var/lib/harbor && docker compose down -v 2>/dev/null || true")
		_, _ = sshClient.Execute("docker stop $(docker ps -q) 2>/dev/null || true")
		sshClient.Close()

		// Deploy to this server
		d.log("info", fmt.Sprintf("  Deploying new version to %s", server.Name))
		if err := d.DeployAppServer(server); err != nil {
			return fmt.Errorf("failed to deploy app on %s: %w", server.Name, err)
		}

		// Wait for health check before moving to next server
		d.log("info", fmt.Sprintf("  Waiting for %s to be healthy...", server.Name))
		if err := d.waitForServerHealth(server); err != nil {
			return fmt.Errorf("server %s failed health check: %w", server.Name, err)
		}

		// Add this server back to APISIX upstreams
		d.log("info", fmt.Sprintf("  Adding %s back to APISIX upstreams", server.Name))
		// Include all servers up to and including the current one (all previously deployed + current)
		deployedServers := appServers[:i+1]
		if err := d.updateAPISIXUpstreamsWithServers(controlPlane, deployedServers); err != nil {
			return fmt.Errorf("failed to add %s back to upstreams: %w", server.Name, err)
		}

		d.log("info", fmt.Sprintf("✓ %s redeployed successfully", server.Name))
	}

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

	// Deploy data planes in parallel
	d.log("info", fmt.Sprintf("Deploying %d data planes in parallel", len(dataPlanes)))
	var wg sync.WaitGroup
	errChan := make(chan error, len(dataPlanes)+len(appServers))

	for _, server := range dataPlanes {
		wg.Add(1)
		go func(srv *models.Server) {
			defer wg.Done()
			d.log("info", fmt.Sprintf("Deploying data plane on %s", srv.Name))
			if err := d.DeployDataPlane(srv, controlPlane.PrivateIP); err != nil {
				errChan <- fmt.Errorf("failed to deploy data plane on %s: %w", srv.Name, err)
				return
			}
			d.log("info", fmt.Sprintf("✓ Data plane deployed on %s", srv.Name))
		}(server)
	}

	// Deploy app servers in parallel (includes monitoring)
	d.log("info", fmt.Sprintf("Deploying %d app servers in parallel", len(appServers)))
	for _, server := range appServers {
		wg.Add(1)
		go func(srv *models.Server) {
			defer wg.Done()
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

		cert, err := os.ReadFile(d.config.APISIX.SSL.CertPath)
		if err != nil {
			d.log("warn", fmt.Sprintf("Failed to read SSL cert: %v", err))
		} else {
			key, err := os.ReadFile(d.config.APISIX.SSL.KeyPath)
			if err != nil {
				d.log("warn", fmt.Sprintf("Failed to read SSL key: %v", err))
			} else {
				var clientCA string
				if d.config.APISIX.SSL.ClientCAPath != "" {
					caBytes, err := os.ReadFile(d.config.APISIX.SSL.ClientCAPath)
					if err != nil {
						d.log("warn", fmt.Sprintf("Failed to read client CA: %v", err))
					} else {
						clientCA = string(caBytes)
					}
				}

				if err := client.CreateSSL(d.config.APISIX.SSL, string(cert), string(key), clientCA); err != nil {
					d.log("warn", fmt.Sprintf("Failed to configure SSL: %v", err))
				}
			}
		}
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

// updateAPISIXUpstreamsWithServers updates APISIX upstreams with a specific list of app servers
// This is useful for removing/adding servers during rolling deployments
func (d *Deployer) updateAPISIXUpstreamsWithServers(controlPlane *models.Server, appServers []*models.Server) error {
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

	// Update upstreams with the provided app servers
	for _, upstream := range d.config.APISIX.Upstreams {
		// Skip non-app upstreams
		if upstream.ID == "grafana" {
			continue
		}

		// Build nodes map with provided app servers
		nodes := make(map[string]int)
		for _, server := range appServers {
			nodes[fmt.Sprintf("%s:80", server.PrivateIP)] = 1
		}

		if err := client.CreateUpstream(upstream, nodes); err != nil {
			return fmt.Errorf("failed to update upstream %s: %w", upstream.Name, err)
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

// waitForServerHealth waits for a single app server to respond to HTTP requests on port 80
func (d *Deployer) waitForServerHealth(server *models.Server) error {
	sshClient, err := ssh.New(server.PublicIP, d.sshUser, d.privateKeyPath)
	if err != nil {
		return fmt.Errorf("failed to connect via SSH: %w", err)
	}
	defer sshClient.Close()

	// Try to connect to the app server for up to 60 seconds
	for i := 0; i < 30; i++ {
		// Check if the service is responding on port 80 via private IP
		cmd := fmt.Sprintf("curl -s -o /dev/null -w '%%{http_code}' http://%s:80 --connect-timeout 2", server.PrivateIP)
		output, err := sshClient.Execute(cmd)
		if err == nil && (output == "200" || output == "301" || output == "302") {
			return nil // Health check passed
		}
		time.Sleep(2 * time.Second)
	}

	return fmt.Errorf("server did not become healthy after 60 seconds")
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
