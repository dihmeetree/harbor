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
	"github.com/dihmeetree/harbor/internal/database"
	"github.com/dihmeetree/harbor/internal/docker"
	"github.com/dihmeetree/harbor/internal/ssh"
	"github.com/dihmeetree/harbor/pkg/models"
	"github.com/hetznercloud/hcloud-go/v2/hcloud"
)

// Deployer handles service deployment to servers
type Deployer struct {
	config         *config.Config
	db             *database.DB
	serverRepo     *database.ServerRepository
	deployRepo     *database.DeploymentRepository
	dockerClient   *docker.Installer
	privateKeyPath string
}

// NewDeployer creates a new deployer
func NewDeployer(cfg *config.Config, db *database.DB, privateKeyPath string) *Deployer {
	return &Deployer{
		config:         cfg,
		db:             db,
		serverRepo:     database.NewServerRepository(db),
		deployRepo:     database.NewDeploymentRepository(db),
		dockerClient:   docker.New(),
		privateKeyPath: privateKeyPath,
	}
}

// DeployServicesOnly redeploys services to existing infrastructure without provisioning
func (d *Deployer) DeployServicesOnly(ctx context.Context) error {
	// Create a new deployment entry
	deployment := &models.Deployment{
		Status:    "in_progress",
		StartedAt: time.Now(),
	}

	if err := d.deployRepo.Create(deployment); err != nil {
		return fmt.Errorf("failed to create deployment: %w", err)
	}

	deploymentID := deployment.ID
	d.log(deploymentID, "info", "Starting service redeployment")

	// Query Hetzner API directly for current servers (ignore local database)
	// This ensures we only redeploy to servers that actually exist
	hetznerToken := os.Getenv("HETZNER_API_TOKEN")
	if hetznerToken == "" {
		_ = d.deployRepo.UpdateStatus(deploymentID, "failed")
		return fmt.Errorf("HETZNER_API_TOKEN environment variable is required")
	}

	// Get servers from Hetzner by role labels
	d.log(deploymentID, "info", "Querying Hetzner API for current servers...")

	controlPlanes, dataPlanes, appServers, err := d.getServersFromHetzner(ctx, hetznerToken)
	if err != nil {
		_ = d.deployRepo.UpdateStatus(deploymentID, "failed")
		return fmt.Errorf("failed to get servers from Hetzner: %w", err)
	}

	if len(controlPlanes) == 0 {
		_ = d.deployRepo.UpdateStatus(deploymentID, "failed")
		return fmt.Errorf("no control plane server found in Hetzner")
	}

	controlPlane := controlPlanes[0]
	d.log(deploymentID, "info", fmt.Sprintf("Found: 1 control plane, %d data planes, %d app servers", len(dataPlanes), len(appServers)))

	// Stop and remove existing containers on all servers
	d.log(deploymentID, "info", "Stopping existing containers...")
	allServers := append(append(controlPlanes, dataPlanes...), appServers...)
	for _, server := range allServers {
		sshClient, err := ssh.New(server.PublicIP, "root", d.privateKeyPath)
		if err != nil {
			d.log(deploymentID, "warn", fmt.Sprintf("Failed to connect to %s: %v", server.Name, err))
			continue
		}
		// Stop containers and remove volumes
		_, _ = sshClient.Execute("cd /opt/harbor && docker compose down -v 2>/dev/null || true")
		sshClient.Close()
	}

	// Deploy control plane (pass data planes for k6 target configuration)
	d.log(deploymentID, "info", "Deploying control plane services")
	if err := d.DeployControlPlaneWithServers(deploymentID, controlPlane, dataPlanes, appServers); err != nil {
		_ = d.deployRepo.UpdateStatus(deploymentID, "failed")
		return fmt.Errorf("failed to deploy control plane: %w", err)
	}

	// Wait for control plane to be ready
	d.log(deploymentID, "info", "Waiting for control plane to be ready...")
	if err := d.waitForControlPlane(deploymentID, controlPlane); err != nil {
		_ = d.deployRepo.UpdateStatus(deploymentID, "failed")
		return fmt.Errorf("control plane failed to become ready: %w", err)
	}
	d.log(deploymentID, "info", "✓ Control plane is ready")

	// Deploy data planes in parallel
	d.log(deploymentID, "info", fmt.Sprintf("Deploying %d data planes in parallel", len(dataPlanes)))
	var wg sync.WaitGroup
	errChan := make(chan error, len(dataPlanes)+len(appServers))

	for _, server := range dataPlanes {
		wg.Add(1)
		go func(srv *models.Server) {
			defer wg.Done()
			d.log(deploymentID, "info", fmt.Sprintf("Deploying data plane on %s", srv.Name))
			if err := d.DeployDataPlane(deploymentID, srv, controlPlane.PrivateIP); err != nil {
				errChan <- fmt.Errorf("failed to deploy data plane on %s: %w", srv.Name, err)
				return
			}
			d.log(deploymentID, "info", fmt.Sprintf("✓ Data plane deployed on %s", srv.Name))
		}(server)
	}

	// Deploy app servers in parallel
	d.log(deploymentID, "info", fmt.Sprintf("Deploying %d app servers in parallel", len(appServers)))
	for _, server := range appServers {
		wg.Add(1)
		go func(srv *models.Server) {
			defer wg.Done()
			d.log(deploymentID, "info", fmt.Sprintf("Deploying app on %s", srv.Name))
			if err := d.DeployAppServer(deploymentID, srv); err != nil {
				errChan <- fmt.Errorf("failed to deploy app on %s: %w", srv.Name, err)
				return
			}
			d.log(deploymentID, "info", fmt.Sprintf("✓ App deployed on %s", srv.Name))
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
		_ = d.deployRepo.UpdateStatus(deploymentID, "failed")
		return errors[0]
	}

	// Wait for data plane services to be ready
	d.log(deploymentID, "info", "Waiting for data plane services to be ready...")
	if err := d.waitForDataPlanes(deploymentID, dataPlanes); err != nil {
		_ = d.deployRepo.UpdateStatus(deploymentID, "failed")
		return fmt.Errorf("data planes failed to become ready: %w", err)
	}
	d.log(deploymentID, "info", "✓ All data planes are ready")

	// Configure APISIX
	d.log(deploymentID, "info", "Configuring APISIX")
	if err := d.ConfigureAPISIX(deploymentID, controlPlane, appServers); err != nil {
		_ = d.deployRepo.UpdateStatus(deploymentID, "failed")
		return fmt.Errorf("failed to configure APISIX: %w", err)
	}

	d.log(deploymentID, "info", "Service redeployment completed successfully")
	_ = d.deployRepo.UpdateStatus(deploymentID, "completed")
	return nil
}

// Deploy deploys all services
func (d *Deployer) Deploy(ctx context.Context, deploymentID int64) error {
	d.log(deploymentID, "info", "Starting service deployment")

	// Get all servers
	servers, err := d.serverRepo.GetAll()
	if err != nil {
		return fmt.Errorf("failed to get servers: %w", err)
	}

	// Wait for servers to be ready (SSH accessible)
	d.log(deploymentID, "info", "Waiting for all servers to be SSH accessible...")
	if err := d.waitForServersReady(deploymentID, servers); err != nil {
		return fmt.Errorf("servers failed to become ready: %w", err)
	}
	d.log(deploymentID, "info", "✓ All servers are SSH accessible")

	// Install Docker on all servers in parallel
	d.log(deploymentID, "info", "Installing Docker on all servers")
	if err := d.InstallDocker(deploymentID, servers); err != nil {
		return fmt.Errorf("failed to install Docker: %w", err)
	}

	// Get server groups
	controlPlanes, _ := d.serverRepo.GetByRole(models.RoleControlPlane)
	dataPlanes, _ := d.serverRepo.GetByRole(models.RoleDataPlane)
	appServers, _ := d.serverRepo.GetByRole(models.RoleApp)

	if len(controlPlanes) == 0 {
		return fmt.Errorf("no control plane server found")
	}

	controlPlane := controlPlanes[0]

	// Deploy control plane
	d.log(deploymentID, "info", "Deploying control plane services")
	if err := d.DeployControlPlane(deploymentID, controlPlane); err != nil {
		return fmt.Errorf("failed to deploy control plane: %w", err)
	}

	// Wait for control plane to be ready
	d.log(deploymentID, "info", "Waiting for control plane to be ready...")
	if err := d.waitForControlPlane(deploymentID, controlPlane); err != nil {
		return fmt.Errorf("control plane failed to become ready: %w", err)
	}
	d.log(deploymentID, "info", "✓ Control plane is ready")

	// Deploy data planes in parallel
	d.log(deploymentID, "info", fmt.Sprintf("Deploying %d data planes in parallel", len(dataPlanes)))
	var wg sync.WaitGroup
	errChan := make(chan error, len(dataPlanes)+len(appServers))

	for _, server := range dataPlanes {
		wg.Add(1)
		go func(srv *models.Server) {
			defer wg.Done()
			d.log(deploymentID, "info", fmt.Sprintf("Deploying data plane on %s", srv.Name))
			if err := d.DeployDataPlane(deploymentID, srv, controlPlane.PrivateIP); err != nil {
				errChan <- fmt.Errorf("failed to deploy data plane on %s: %w", srv.Name, err)
				return
			}
			d.log(deploymentID, "info", fmt.Sprintf("✓ Data plane deployed on %s", srv.Name))
		}(server)
	}

	// Deploy app servers in parallel
	d.log(deploymentID, "info", fmt.Sprintf("Deploying %d app servers in parallel", len(appServers)))
	for _, server := range appServers {
		wg.Add(1)
		go func(srv *models.Server) {
			defer wg.Done()
			d.log(deploymentID, "info", fmt.Sprintf("Deploying app on %s", srv.Name))
			if err := d.DeployAppServer(deploymentID, srv); err != nil {
				errChan <- fmt.Errorf("failed to deploy app on %s: %w", srv.Name, err)
				return
			}
			d.log(deploymentID, "info", fmt.Sprintf("✓ App deployed on %s", srv.Name))
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
	d.log(deploymentID, "info", "Waiting for data plane services to be ready...")
	if err := d.waitForDataPlanes(deploymentID, dataPlanes); err != nil {
		return fmt.Errorf("data planes failed to become ready: %w", err)
	}
	d.log(deploymentID, "info", "✓ All data planes are ready")

	// Configure APISIX
	d.log(deploymentID, "info", "Configuring APISIX")
	if err := d.ConfigureAPISIX(deploymentID, controlPlane, appServers); err != nil {
		return fmt.Errorf("failed to configure APISIX: %w", err)
	}

	d.log(deploymentID, "info", "Service deployment completed successfully")
	return nil
}

// installDocker installs Docker on all servers in parallel
// InstallDocker installs Docker on the specified servers
func (d *Deployer) InstallDocker(deploymentID int64, servers []*models.Server) error {
	var wg sync.WaitGroup
	errChan := make(chan error, len(servers))

	for _, server := range servers {
		wg.Add(1)
		go func(srv *models.Server) {
			defer wg.Done()

			d.log(deploymentID, "info", fmt.Sprintf("Waiting for SSH on %s (%s)", srv.Name, srv.PublicIP))

			sshClient, err := ssh.WaitForConnection(srv.PublicIP, "root", d.privateKeyPath, 5*time.Minute)
			if err != nil {
				errChan <- fmt.Errorf("failed to connect to %s via SSH: %w", srv.Name, err)
				return
			}
			defer sshClient.Close()

			d.log(deploymentID, "info", fmt.Sprintf("✓ SSH ready on %s", srv.Name))

			// Update server status to running
			if err := d.serverRepo.UpdateStatus(srv.ID, "running"); err != nil {
				d.log(deploymentID, "warn", fmt.Sprintf("Failed to update status for %s: %v", srv.Name, err))
			}

			d.log(deploymentID, "info", fmt.Sprintf("Installing Docker on %s", srv.Name))
			if err := d.dockerClient.Install(sshClient); err != nil {
				errChan <- fmt.Errorf("failed to install Docker on %s: %w", srv.Name, err)
				return
			}

			d.log(deploymentID, "info", fmt.Sprintf("✓ Docker installed on %s", srv.Name))
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

	d.log(deploymentID, "info", "✓ Docker installed on all servers")
	return nil
}

// DeployControlPlane deploys services to the control plane server (queries DB for servers)
func (d *Deployer) DeployControlPlane(deploymentID int64, server *models.Server) error {
	// Get servers from database
	dataPlanes, _ := d.serverRepo.GetByRole(models.RoleDataPlane)
	appServers, _ := d.serverRepo.GetByRole(models.RoleApp)
	return d.DeployControlPlaneWithServers(deploymentID, server, dataPlanes, appServers)
}

// DeployControlPlaneWithServers deploys services to the control plane server with provided server lists
func (d *Deployer) DeployControlPlaneWithServers(deploymentID int64, server *models.Server, dataPlanes, appServers []*models.Server) error {
	sshClient, err := ssh.New(server.PublicIP, "root", d.privateKeyPath)
	if err != nil {
		return fmt.Errorf("failed to connect via SSH: %w", err)
	}
	defer sshClient.Close()

	// Create deployment directory
	if _, err := sshClient.Execute("mkdir -p /opt/harbor"); err != nil {
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

	// Set k6 defaults if not configured
	k6PreallocatedVUs := d.config.K6.PreallocatedVUs
	if k6PreallocatedVUs == 0 {
		k6PreallocatedVUs = 10
	}
	k6MaxVUs := d.config.K6.MaxVUs
	if k6MaxVUs == 0 {
		k6MaxVUs = 100
	}
	k6Rate := d.config.K6.Rate
	if k6Rate == 0 {
		k6Rate = 10
	}
	k6Duration := d.config.K6.Duration
	if k6Duration == "" {
		k6Duration = "30s"
	}
	k6TargetPath := d.config.K6.TargetPath
	if k6TargetPath == "" {
		k6TargetPath = "/"
	}
	k6ConnectionTimeout := d.config.K6.ConnectionTimeout
	if k6ConnectionTimeout == "" {
		k6ConnectionTimeout = "10s"
	}
	k6RequestTimeout := d.config.K6.RequestTimeout
	if k6RequestTimeout == "" {
		k6RequestTimeout = "30s"
	}
	k6GracefulStop := d.config.K6.GracefulStop
	if k6GracefulStop == "" {
		k6GracefulStop = "30s"
	}

	// Prepare template data
	data := TemplateData{
		PrometheusPort:      d.config.Monitoring.PrometheusPort,
		CAdvisorPort:        d.config.Monitoring.CAdvisorPort,
		NodeExporterPort:    d.config.Monitoring.NodeExporterPort,
		APIKey:              d.config.APISIX.APIKey,
		AutoscalerEnabled:   d.config.Autoscaler.Enabled,
		HetznerToken:        os.Getenv("HETZNER_API_TOKEN"),
		K6Enabled:           d.config.K6.Enabled,
		K6PreallocatedVUs:   k6PreallocatedVUs,
		K6MaxVUs:            k6MaxVUs,
		K6Rate:              k6Rate,
		K6Duration:          k6Duration,
		K6TargetPath:        k6TargetPath,
		K6ConnectionTimeout: k6ConnectionTimeout,
		K6RequestTimeout:    k6RequestTimeout,
		K6GracefulStop:      k6GracefulStop,
		K6LBTargets:         k6LBTargets,
	}

	// Render and deploy docker-compose
	composeContent, err := RenderTemplate(ControlPlaneTemplate, data)
	if err != nil {
		return fmt.Errorf("failed to render control plane template: %w", err)
	}

	if err := sshClient.WriteFile("/opt/harbor/docker-compose.yml", composeContent); err != nil {
		return fmt.Errorf("failed to write docker-compose.yml: %w", err)
	}

	// Render and deploy APISIX config
	apisixConfig, err := RenderTemplate(APISIXControlPlaneConfigTemplate, data)
	if err != nil {
		return fmt.Errorf("failed to render APISIX config: %w", err)
	}

	if err := sshClient.WriteFile("/opt/harbor/apisix-control.yaml", apisixConfig); err != nil {
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
	}

	prometheusConfig, err := RenderTemplate(PrometheusConfigTemplate, prometheusData)
	if err != nil {
		return fmt.Errorf("failed to render Prometheus config: %w", err)
	}

	if err := sshClient.WriteFile("/opt/harbor/prometheus.yml", prometheusConfig); err != nil {
		return fmt.Errorf("failed to write Prometheus config: %w", err)
	}

	// Copy Grafana configuration files
	if _, err := os.Stat("grafana"); err == nil {
		d.log(deploymentID, "info", "Copying Grafana configuration to control plane...")

		// Create Grafana directories
		if _, err := sshClient.Execute("mkdir -p /opt/harbor/grafana/config"); err != nil {
			return fmt.Errorf("failed to create grafana config directory: %w", err)
		}

		// Copy grafana.ini
		if err := sshClient.CopyFile("grafana/config/grafana.ini", "/opt/harbor/grafana/config/grafana.ini"); err != nil {
			return fmt.Errorf("failed to copy grafana.ini: %w", err)
		}

		// Copy provisioning directory
		if err := sshClient.CopyDir("grafana/provisioning", "/opt/harbor/grafana/provisioning"); err != nil {
			return fmt.Errorf("failed to copy grafana provisioning directory: %w", err)
		}

		// Copy dashboards directory
		if err := sshClient.CopyDir("grafana/dashboards", "/opt/harbor/grafana/dashboards"); err != nil {
			return fmt.Errorf("failed to copy grafana dashboards directory: %w", err)
		}

		d.log(deploymentID, "info", "✓ Grafana configuration copied to control plane")
	}

	// Copy harbor config for autoscaler
	if d.config.Autoscaler.Enabled {
		configPath := "harbor.yaml"
		if _, err := os.Stat(configPath); err == nil {
			d.log(deploymentID, "info", "Copying harbor config for autoscaler...")
			if err := sshClient.CopyFile(configPath, "/opt/harbor/config.yaml"); err != nil {
				return fmt.Errorf("failed to copy config file: %w", err)
			}
			d.log(deploymentID, "info", "✓ Config copied to control plane")
		}
	}

	// Copy k6 load test script
	if d.config.K6.Enabled {
		k6ScriptPath := "k6/loadtest.js"
		if _, err := os.Stat(k6ScriptPath); err == nil {
			d.log(deploymentID, "info", "Copying k6 load test script to control plane...")
			// Create k6 directory
			if _, err := sshClient.Execute("mkdir -p /opt/harbor/k6"); err != nil {
				return fmt.Errorf("failed to create k6 directory: %w", err)
			}
			if err := sshClient.CopyFile(k6ScriptPath, "/opt/harbor/k6/loadtest.js"); err != nil {
				return fmt.Errorf("failed to copy k6 script: %w", err)
			}
			d.log(deploymentID, "info", "✓ k6 script copied to control plane")
		} else {
			d.log(deploymentID, "warn", "k6 enabled but k6/loadtest.js not found")
		}
	}

	// Copy APISIX plugins directory
	if _, err := os.Stat("apisix/plugins"); err == nil {
		d.log(deploymentID, "info", "Copying APISIX plugins to control plane...")
		if err := sshClient.CopyDir("apisix/plugins", "/opt/harbor/apisix/plugins"); err != nil {
			return fmt.Errorf("failed to copy plugins directory: %w", err)
		}
		d.log(deploymentID, "info", "✓ Plugins copied to control plane")
	} else {
		d.log(deploymentID, "warn", "No apisix/plugins directory found, skipping plugin copy")
	}

	// Start services
	if err := d.dockerClient.ComposeUp(sshClient, "/opt/harbor"); err != nil {
		return fmt.Errorf("failed to start services: %w", err)
	}

	return nil
}

// DeployDataPlane deploys services to a data plane server
func (d *Deployer) DeployDataPlane(deploymentID int64, server *models.Server, controlPlaneIP string) error {
	sshClient, err := ssh.New(server.PublicIP, "root", d.privateKeyPath)
	if err != nil {
		return fmt.Errorf("failed to connect via SSH: %w", err)
	}
	defer sshClient.Close()

	// Create deployment directory
	if _, err := sshClient.Execute("mkdir -p /opt/harbor"); err != nil {
		return fmt.Errorf("failed to create deployment directory: %w", err)
	}

	// Prepare template data
	data := TemplateData{
		CAdvisorPort:     d.config.Monitoring.CAdvisorPort,
		NodeExporterPort: d.config.Monitoring.NodeExporterPort,
		ControlPlaneIP:   controlPlaneIP,
	}

	// Render and deploy docker-compose
	composeContent, err := RenderTemplate(DataPlaneTemplate, data)
	if err != nil {
		return fmt.Errorf("failed to render data plane template: %w", err)
	}

	if err := sshClient.WriteFile("/opt/harbor/docker-compose.yml", composeContent); err != nil {
		return fmt.Errorf("failed to write docker-compose.yml: %w", err)
	}

	// Render and deploy APISIX config
	apisixConfig, err := RenderTemplate(APISIXDataPlaneConfigTemplate, data)
	if err != nil {
		return fmt.Errorf("failed to render APISIX config: %w", err)
	}

	if err := sshClient.WriteFile("/opt/harbor/apisix-data.yaml", apisixConfig); err != nil {
		return fmt.Errorf("failed to write APISIX config: %w", err)
	}

	// Copy APISIX plugins directory
	if _, err := os.Stat("apisix/plugins"); err == nil {
		d.log(deploymentID, "info", fmt.Sprintf("Copying APISIX plugins to %s...", server.Name))
		if err := sshClient.CopyDir("apisix/plugins", "/opt/harbor/apisix/plugins"); err != nil {
			return fmt.Errorf("failed to copy plugins directory: %w", err)
		}
		d.log(deploymentID, "info", fmt.Sprintf("✓ Plugins copied to %s", server.Name))
	}

	// Start services
	if err := d.dockerClient.ComposeUp(sshClient, "/opt/harbor"); err != nil {
		return fmt.Errorf("failed to start services: %w", err)
	}

	return nil
}

// DeployAppServer deploys services to an app server
func (d *Deployer) DeployAppServer(deploymentID int64, server *models.Server) error {
	sshClient, err := ssh.New(server.PublicIP, "root", d.privateKeyPath)
	if err != nil {
		return fmt.Errorf("failed to connect via SSH: %w", err)
	}
	defer sshClient.Close()

	// Create deployment directory
	if _, err := sshClient.Execute("mkdir -p /opt/harbor"); err != nil {
		return fmt.Errorf("failed to create deployment directory: %w", err)
	}

	// Prepare template data
	data := TemplateData{
		CAdvisorPort:     d.config.Monitoring.CAdvisorPort,
		NodeExporterPort: d.config.Monitoring.NodeExporterPort,
		AppImage:         d.config.Container.Image,
		ServerID:         server.Name,
	}

	// Render and deploy docker-compose
	composeContent, err := RenderTemplate(AppServerTemplate, data)
	if err != nil {
		return fmt.Errorf("failed to render app server template: %w", err)
	}

	if err := sshClient.WriteFile("/opt/harbor/docker-compose.yml", composeContent); err != nil {
		return fmt.Errorf("failed to write docker-compose.yml: %w", err)
	}

	// Render and deploy nginx config
	nginxContent, err := RenderTemplate(NginxConfigTemplate, data)
	if err != nil {
		return fmt.Errorf("failed to render nginx config: %w", err)
	}

	if err := sshClient.WriteFile("/opt/harbor/nginx.conf", nginxContent); err != nil {
		return fmt.Errorf("failed to write nginx.conf: %w", err)
	}

	// Start services
	if err := d.dockerClient.ComposeUp(sshClient, "/opt/harbor"); err != nil {
		return fmt.Errorf("failed to start services: %w", err)
	}

	return nil
}

// ConfigureAPISIX configures APISIX via the Admin API
func (d *Deployer) ConfigureAPISIX(deploymentID int64, controlPlane *models.Server, appServers []*models.Server) error {
	// Connect to control plane via SSH to run curl commands locally
	sshClient, err := ssh.New(controlPlane.PublicIP, "root", d.privateKeyPath)
	if err != nil {
		return fmt.Errorf("failed to connect to control plane via SSH: %w", err)
	}
	defer sshClient.Close()

	// Wait for APISIX to be ready
	d.log(deploymentID, "info", "Waiting for APISIX Admin API to be ready...")
	adminURL := "http://127.0.0.1:9180"
	apiKey := d.config.APISIX.APIKey

	for i := range 30 {
		cmd := fmt.Sprintf("curl -s -o /dev/null -w '%%{http_code}' %s/apisix/admin/routes -H 'X-API-KEY: %s'", adminURL, apiKey)
		output, err := sshClient.Execute(cmd)
		if err == nil && output == "200" {
			d.log(deploymentID, "info", "✓ APISIX Admin API is ready")
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
		d.log(deploymentID, "info", fmt.Sprintf("Creating upstream: %s", upstream.Name))

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
		d.log(deploymentID, "info", fmt.Sprintf("Creating global rule: %s", rule.ID))
		if err := client.CreateGlobalRule(rule); err != nil {
			return fmt.Errorf("failed to create global rule %s: %w", rule.ID, err)
		}
	}

	// Create routes
	for _, route := range d.config.APISIX.Routes {
		d.log(deploymentID, "info", fmt.Sprintf("Creating route: %s", route.Name))
		if err := client.CreateRoute(route); err != nil {
			return fmt.Errorf("failed to create route %s: %w", route.Name, err)
		}
	}

	// Create SSL certificates (if configured)
	if d.config.APISIX.SSL.CertPath != "" {
		d.log(deploymentID, "info", "Configuring SSL certificates")

		cert, err := os.ReadFile(d.config.APISIX.SSL.CertPath)
		if err != nil {
			d.log(deploymentID, "warn", fmt.Sprintf("Failed to read SSL cert: %v", err))
		} else {
			key, err := os.ReadFile(d.config.APISIX.SSL.KeyPath)
			if err != nil {
				d.log(deploymentID, "warn", fmt.Sprintf("Failed to read SSL key: %v", err))
			} else {
				var clientCA string
				if d.config.APISIX.SSL.ClientCAPath != "" {
					caBytes, err := os.ReadFile(d.config.APISIX.SSL.ClientCAPath)
					if err != nil {
						d.log(deploymentID, "warn", fmt.Sprintf("Failed to read client CA: %v", err))
					} else {
						clientCA = string(caBytes)
					}
				}

				if err := client.CreateSSL(d.config.APISIX.SSL, string(cert), string(key), clientCA); err != nil {
					d.log(deploymentID, "warn", fmt.Sprintf("Failed to configure SSL: %v", err))
				}
			}
		}
	}

	d.log(deploymentID, "info", "APISIX configuration completed")
	return nil
}

// UpdateAPISIXUpstreams updates only the upstream nodes (used for scaling operations)
func (d *Deployer) UpdateAPISIXUpstreams(deploymentID int64, controlPlane *models.Server, appServers []*models.Server) error {
	// Connect to control plane via SSH
	sshClient, err := ssh.New(controlPlane.PublicIP, "root", d.privateKeyPath)
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
		d.log(deploymentID, "info", fmt.Sprintf("Updating upstream: %s", upstream.Name))

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

	d.log(deploymentID, "info", "✓ Upstreams updated")
	return nil
}

// UpdatePrometheusConfig updates Prometheus scrape targets (used for scaling operations)
func (d *Deployer) UpdatePrometheusConfig(deploymentID int64, controlPlane *models.Server, dataPlanes []*models.Server, appServers []*models.Server) error {
	// Connect to control plane via SSH
	sshClient, err := ssh.New(controlPlane.PublicIP, "root", d.privateKeyPath)
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
	}

	prometheusConfig, err := RenderTemplate(PrometheusConfigTemplate, prometheusData)
	if err != nil {
		return fmt.Errorf("failed to render Prometheus config: %w", err)
	}

	// Write new config to server
	if err := sshClient.WriteFile("/opt/harbor/prometheus.yml", prometheusConfig); err != nil {
		return fmt.Errorf("failed to write Prometheus config: %w", err)
	}

	// Reload Prometheus configuration
	d.log(deploymentID, "info", "Reloading Prometheus configuration")
	reloadCmd := "curl -X POST http://localhost:9090/-/reload"
	if _, err := sshClient.Execute(reloadCmd); err != nil {
		return fmt.Errorf("failed to reload Prometheus: %w", err)
	}

	d.log(deploymentID, "info", "✓ Prometheus configuration updated")
	return nil
}

// RestartK6 restarts the k6 load testing container with the latest configuration from harbor.yaml
func (d *Deployer) RestartK6(ctx context.Context) error {
	// Get control plane server
	controlPlane, err := d.serverRepo.GetByRole(models.RoleControlPlane)
	if err != nil {
		return fmt.Errorf("failed to get control plane: %w", err)
	}
	if len(controlPlane) == 0 {
		return fmt.Errorf("no control plane server found")
	}

	// Get all data planes from Hetzner (for accurate targets)
	token := os.Getenv("HETZNER_API_TOKEN")
	if token == "" {
		return fmt.Errorf("HETZNER_API_TOKEN environment variable not set")
	}

	_, dataPlanes, _, err := d.getServersFromHetzner(ctx, token)
	if err != nil {
		return fmt.Errorf("failed to get servers from Hetzner: %w", err)
	}

	if len(dataPlanes) == 0 {
		return fmt.Errorf("no data plane servers found")
	}

	// Connect to control plane via SSH
	sshClient, err := ssh.New(controlPlane[0].PublicIP, "root", d.privateKeyPath)
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

	// Set defaults for k6 config
	rate := d.config.K6.Rate
	if rate == 0 {
		rate = 10
	}

	duration := d.config.K6.Duration
	if duration == "" {
		duration = "30s"
	}

	preallocatedVUs := d.config.K6.PreallocatedVUs
	if preallocatedVUs == 0 {
		preallocatedVUs = 10
	}

	maxVUs := d.config.K6.MaxVUs
	if maxVUs == 0 {
		maxVUs = 100
	}

	targetPath := d.config.K6.TargetPath
	if targetPath == "" {
		targetPath = "/"
	}

	connectionTimeout := d.config.K6.ConnectionTimeout
	if connectionTimeout == "" {
		connectionTimeout = "10s"
	}

	requestTimeout := d.config.K6.RequestTimeout
	if requestTimeout == "" {
		requestTimeout = "30s"
	}

	gracefulStop := d.config.K6.GracefulStop
	if gracefulStop == "" {
		gracefulStop = "30s"
	}

	// Run k6 with updated configuration
	fmt.Println("[info] Starting k6 with updated configuration...")
	fmt.Printf("[info]   Rate: %d req/s | VUs: %d-%d | Duration: %s | Path: %s\n",
		rate, preallocatedVUs, maxVUs, duration, targetPath)

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

	output, err := sshClient.Execute(runCmd)
	if err != nil {
		return fmt.Errorf("failed to start k6 container: %w (output: %s)", err, output)
	}

	return nil
}

// StopK6 stops and removes the k6 load testing container
func (d *Deployer) StopK6(ctx context.Context) error {
	// Get control plane server
	controlPlane, err := d.serverRepo.GetByRole(models.RoleControlPlane)
	if err != nil {
		return fmt.Errorf("failed to get control plane: %w", err)
	}
	if len(controlPlane) == 0 {
		return fmt.Errorf("no control plane server found")
	}

	// Connect to control plane via SSH
	sshClient, err := ssh.New(controlPlane[0].PublicIP, "root", d.privateKeyPath)
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
func (d *Deployer) waitForControlPlane(deploymentID int64, server *models.Server) error {
	sshClient, err := ssh.New(server.PublicIP, "root", d.privateKeyPath)
	if err != nil {
		return fmt.Errorf("failed to connect via SSH: %w", err)
	}
	defer sshClient.Close()

	// Wait for etcd to be ready
	d.log(deploymentID, "info", "Checking etcd...")
	if err := d.waitForService(sshClient, "curl -sf http://127.0.0.1:2379/health", 60, 2*time.Second); err != nil {
		return fmt.Errorf("etcd failed to become ready: %w", err)
	}
	d.log(deploymentID, "info", "✓ etcd is ready")

	// Wait for APISIX control plane to be ready
	d.log(deploymentID, "info", "Checking APISIX control plane...")
	apiKey := d.config.APISIX.APIKey
	checkCmd := fmt.Sprintf("curl -sf -H 'X-API-KEY: %s' http://127.0.0.1:9180/apisix/admin/routes", apiKey)
	if err := d.waitForService(sshClient, checkCmd, 60, 2*time.Second); err != nil {
		return fmt.Errorf("APISIX control plane failed to become ready: %w", err)
	}
	d.log(deploymentID, "info", "✓ APISIX control plane is ready")

	// Wait for Prometheus to be ready
	d.log(deploymentID, "info", "Checking Prometheus...")
	if err := d.waitForService(sshClient, "curl -sf http://127.0.0.1:9090/-/ready", 30, 2*time.Second); err != nil {
		return fmt.Errorf("prometheus failed to become ready: %w", err)
	}
	d.log(deploymentID, "info", "✓ Prometheus is ready")

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
func (d *Deployer) waitForServersReady(deploymentID int64, servers []*models.Server) error {
	var wg sync.WaitGroup
	errChan := make(chan error, len(servers))

	for _, server := range servers {
		wg.Add(1)
		go func(srv *models.Server) {
			defer wg.Done()
			d.log(deploymentID, "info", fmt.Sprintf("Waiting for SSH on %s...", srv.Name))

			// Try to connect with timeout
			client, err := ssh.WaitForConnection(srv.PublicIP, "root", d.privateKeyPath, 10*time.Minute)
			if err != nil {
				errChan <- fmt.Errorf("server %s not ready: %w", srv.Name, err)
				return
			}
			client.Close()

			// Update server status to running
			if err := d.serverRepo.UpdateStatus(srv.ID, "running"); err != nil {
				d.log(deploymentID, "warn", fmt.Sprintf("Failed to update status for %s: %v", srv.Name, err))
			}

			d.log(deploymentID, "info", fmt.Sprintf("✓ %s is SSH accessible", srv.Name))
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

// waitForDataPlanes waits for all data plane APISIX instances to be ready
func (d *Deployer) waitForDataPlanes(deploymentID int64, dataPlanes []*models.Server) error {
	var wg sync.WaitGroup
	errChan := make(chan error, len(dataPlanes))

	for _, server := range dataPlanes {
		wg.Add(1)
		go func(srv *models.Server) {
			defer wg.Done()
			d.log(deploymentID, "info", fmt.Sprintf("Checking APISIX data plane on %s...", srv.Name))

			sshClient, err := ssh.New(srv.PublicIP, "root", d.privateKeyPath)
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

			d.log(deploymentID, "info", fmt.Sprintf("✓ APISIX data plane on %s is ready", srv.Name))
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

// log adds a log entry to the deployment
// DeployToServer deploys services to a single server (used by autoscaler)
func (d *Deployer) DeployToServer(serverIP string, roleLabel string, controlPlaneIP string) error {
	// Use a fake deployment ID for logging
	deploymentID := int64(0)

	// Connect via SSH
	sshClient, err := ssh.New(serverIP, "root", d.privateKeyPath)
	if err != nil {
		return fmt.Errorf("failed to connect via SSH: %w", err)
	}
	defer sshClient.Close()

	// Install Docker
	d.log(deploymentID, "info", fmt.Sprintf("Installing Docker on %s...", serverIP))
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
		d.log(deploymentID, "info", fmt.Sprintf("Deploying data plane services on %s...", serverIP))
		if err := d.DeployDataPlane(deploymentID, server, controlPlaneIP); err != nil {
			return fmt.Errorf("failed to deploy data plane: %w", err)
		}
	case "app":
		d.log(deploymentID, "info", fmt.Sprintf("Deploying app services on %s...", serverIP))
		if err := d.DeployAppServer(deploymentID, server); err != nil {
			return fmt.Errorf("failed to deploy app server: %w", err)
		}
	default:
		return fmt.Errorf("unsupported role: %s", roleLabel)
	}

	d.log(deploymentID, "info", fmt.Sprintf("✓ Services deployed on %s", serverIP))
	return nil
}

func (d *Deployer) log(deploymentID int64, level, message string) {
	// Only log to database if deployment ID is valid
	if deploymentID > 0 {
		_ = d.deployRepo.AddLog(deploymentID, level, message)
	}
	fmt.Printf("[%s] %s\n", level, message)
}

// getServersFromHetzner queries Hetzner API for servers by role labels
func (d *Deployer) getServersFromHetzner(ctx context.Context, token string) (controlPlanes, dataPlanes, appServers []*models.Server, err error) {
	client := hcloud.NewClient(hcloud.WithToken(token))

	// Get control plane servers
	controlHetzner, err := client.Server.AllWithOpts(ctx, hcloud.ServerListOpts{
		ListOpts: hcloud.ListOpts{
			LabelSelector: "role=control",
		},
	})
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to get control planes: %w", err)
	}

	for _, srv := range controlHetzner {
		var privateIP string
		if len(srv.PrivateNet) > 0 {
			privateIP = srv.PrivateNet[0].IP.String()
		}
		controlPlanes = append(controlPlanes, &models.Server{
			Name:      srv.Name,
			PublicIP:  srv.PublicNet.IPv4.IP.String(),
			PrivateIP: privateIP,
		})
	}

	// Get data plane (load balancer) servers
	lbHetzner, err := client.Server.AllWithOpts(ctx, hcloud.ServerListOpts{
		ListOpts: hcloud.ListOpts{
			LabelSelector: "role=lb",
		},
	})
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to get data planes: %w", err)
	}

	for _, srv := range lbHetzner {
		var privateIP string
		if len(srv.PrivateNet) > 0 {
			privateIP = srv.PrivateNet[0].IP.String()
		}
		dataPlanes = append(dataPlanes, &models.Server{
			Name:      srv.Name,
			PublicIP:  srv.PublicNet.IPv4.IP.String(),
			PrivateIP: privateIP,
		})
	}

	// Get app servers
	appHetzner, err := client.Server.AllWithOpts(ctx, hcloud.ServerListOpts{
		ListOpts: hcloud.ListOpts{
			LabelSelector: "role=app",
		},
	})
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to get app servers: %w", err)
	}

	for _, srv := range appHetzner {
		var privateIP string
		if len(srv.PrivateNet) > 0 {
			privateIP = srv.PrivateNet[0].IP.String()
		}
		appServers = append(appServers, &models.Server{
			Name:      srv.Name,
			PublicIP:  srv.PublicNet.IPv4.IP.String(),
			PrivateIP: privateIP,
		})
	}

	return controlPlanes, dataPlanes, appServers, nil
}
