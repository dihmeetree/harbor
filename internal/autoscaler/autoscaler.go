package autoscaler

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/dihmeetree/harbor/internal/apisix"
	"github.com/dihmeetree/harbor/internal/config"
	"github.com/dihmeetree/harbor/internal/orchestrator"
	"github.com/dihmeetree/harbor/internal/ssh"
	"github.com/dihmeetree/harbor/pkg/models"
	"github.com/hetznercloud/hcloud-go/v2/hcloud"
)

// Autoscaler monitors metrics and scales infrastructure
type Autoscaler struct {
	config        *config.Config
	prometheusURL string
	hetznerToken  string
	hetznerClient *hcloud.Client
	apisixClient  *apisix.Client
	deployer      *orchestrator.Deployer
	lastScaleTime map[string]time.Time
	scaleLock     sync.Mutex
	stopChan      chan struct{}
}

// MetricResult represents a Prometheus query result
type PrometheusResponse struct {
	Status string `json:"status"`
	Data   struct {
		ResultType string `json:"resultType"`
		Result     []struct {
			Metric map[string]string `json:"metric"`
			Value  []interface{}     `json:"value"`
		} `json:"result"`
	} `json:"data"`
}

// NewAutoscaler creates a new autoscaler.
// Returns an error if the database cannot be opened or the home directory cannot be determined.
func NewAutoscaler(cfg *config.Config, prometheusURL string, hetznerToken string, apisixURL string, apisixKey string, sshKeyPath string) (*Autoscaler, error) {
	hetznerClient := hcloud.NewClient(hcloud.WithToken(hetznerToken))
	apisixClient := apisix.New(apisixURL, apisixKey)

	// Create deployer for service deployment
	deployer := orchestrator.NewDeployer(cfg, hetznerToken, sshKeyPath)

	return &Autoscaler{
		config:        cfg,
		prometheusURL: prometheusURL,
		hetznerToken:  hetznerToken,
		hetznerClient: hetznerClient,
		apisixClient:  apisixClient,
		deployer:      deployer,
		lastScaleTime: make(map[string]time.Time),
		stopChan:      make(chan struct{}),
	}, nil
}

// Start starts the autoscaler loop
func (a *Autoscaler) Start(ctx context.Context) error {
	if !a.config.Autoscaler.Enabled {
		return nil
	}

	ticker := time.NewTicker(time.Duration(a.config.Autoscaler.CheckInterval) * time.Second)
	defer ticker.Stop()

	a.log("info", "Autoscaler started")

	for {
		select {
		case <-ctx.Done():
			a.log("info", "Autoscaler stopped")
			return nil
		case <-a.stopChan:
			a.log("info", "Autoscaler stopped")
			return nil
		case <-ticker.C:
			if err := a.checkAndScale(ctx); err != nil {
				a.log("error", fmt.Sprintf("Scaling check failed: %v", err))
			}
		}
	}
}

// Stop stops the autoscaler
func (a *Autoscaler) Stop() {
	close(a.stopChan)
}

// checkAndScale checks metrics and scales if needed
func (a *Autoscaler) checkAndScale(ctx context.Context) error {
	a.scaleLock.Lock()
	defer a.scaleLock.Unlock()

	a.log("info", "Running autoscaler check cycle...")

	// Check load balancer pool
	if a.config.LoadBalancer.Enabled {
		if err := a.checkPool(ctx, "lb", "loadbalancer"); err != nil {
			a.log("warn", fmt.Sprintf("Failed to check LB pool: %v", err))
		}
	}

	// Check app pool
	if a.config.App.Enabled {
		if err := a.checkPool(ctx, "app", "app"); err != nil {
			a.log("warn", fmt.Sprintf("Failed to check app pool: %v", err))
		}
	}

	a.log("info", "Autoscaler check cycle completed")

	return nil
}

// checkPool checks a specific server pool and scales if needed
func (a *Autoscaler) checkPool(ctx context.Context, roleLabel string, poolName string) error {
	// Check cooldown period
	if lastScale, exists := a.lastScaleTime[poolName]; exists {
		cooldownRemaining := time.Duration(a.config.Autoscaler.Cooldown)*time.Second - time.Since(lastScale)
		if cooldownRemaining > 0 {
			a.log("info", fmt.Sprintf("[%s] In cooldown period, %d seconds remaining", poolName, int(cooldownRemaining.Seconds())))
			return nil
		}
	}

	// Get servers from Hetzner by label
	servers, err := a.hetznerClient.Server.AllWithOpts(ctx, hcloud.ServerListOpts{
		ListOpts: hcloud.ListOpts{
			LabelSelector: fmt.Sprintf("role=%s", roleLabel),
		},
	})
	if err != nil {
		return fmt.Errorf("failed to get servers from Hetzner: %w", err)
	}

	currentReplicas := len(servers)
	a.log("info", fmt.Sprintf("[%s] Found %d servers with role=%s", poolName, currentReplicas, roleLabel))
	if currentReplicas == 0 {
		a.log("warn", fmt.Sprintf("[%s] No servers found, skipping metrics check", poolName))
		return nil
	}

	// Get server IPs for metrics
	var serverIPs []string
	for _, srv := range servers {
		// Use private IP if available, otherwise public IP
		privateIP := orchestrator.ExtractPrivateIP(srv)
		if privateIP != "" {
			serverIPs = append(serverIPs, privateIP)
		} else {
			serverIPs = append(serverIPs, srv.PublicNet.IPv4.IP.String())
		}
	}

	// Get average CPU usage
	avgCPU, err := a.getAverageCPU(serverIPs)
	if err != nil {
		return fmt.Errorf("failed to get CPU metrics: %w", err)
	}

	// Get average memory usage
	avgMem, err := a.getAverageMemory(serverIPs)
	if err != nil {
		return fmt.Errorf("failed to get memory metrics: %w", err)
	}

	a.log("info", fmt.Sprintf("[%s] Metrics - Replicas: %d | CPU: %.2f%% (threshold: %.0f%%/%.0f%%) | Memory: %.2f%% (threshold: %.0f%%/%.0f%%)",
		poolName, currentReplicas, avgCPU, a.config.Autoscaler.CPUThresholdDown, a.config.Autoscaler.CPUThresholdUp,
		avgMem, a.config.Autoscaler.MemThresholdDown, a.config.Autoscaler.MemThresholdUp))

	// Determine if scaling is needed
	shouldScaleUp := (avgCPU > a.config.Autoscaler.CPUThresholdUp ||
		avgMem > a.config.Autoscaler.MemThresholdUp) &&
		currentReplicas < a.config.Autoscaler.MaxReplicas

	shouldScaleDown := (avgCPU < a.config.Autoscaler.CPUThresholdDown &&
		avgMem < a.config.Autoscaler.MemThresholdDown) &&
		currentReplicas > a.config.Autoscaler.MinReplicas

	if shouldScaleUp {
		a.log("info", fmt.Sprintf("[%s] Scaling UP (CPU: %.2f%%, Memory: %.2f%%)",
			poolName, avgCPU, avgMem))
		if err := a.scaleUp(ctx, roleLabel, poolName); err != nil {
			return fmt.Errorf("failed to scale up: %w", err)
		}
		a.lastScaleTime[poolName] = time.Now()
	} else if shouldScaleDown {
		a.log("info", fmt.Sprintf("[%s] Scaling DOWN (CPU: %.2f%%, Memory: %.2f%%)",
			poolName, avgCPU, avgMem))
		if err := a.scaleDown(ctx, roleLabel, poolName); err != nil {
			return fmt.Errorf("failed to scale down: %w", err)
		}
		a.lastScaleTime[poolName] = time.Now()
	}

	return nil
}

// getAverageCPU gets average CPU usage from Prometheus
func (a *Autoscaler) getAverageCPU(ips []string) (float64, error) {
	if len(ips) == 0 {
		return 0, nil
	}

	query := fmt.Sprintf(`avg(100 - (avg by(instance) (irate(node_cpu_seconds_total{mode="idle",instance=~"%s:9100"}[5m])) * 100))`,
		strings.Join(ips, ":9100|"))

	return a.queryPrometheus(query)
}

// getAverageMemory gets average memory usage from Prometheus
func (a *Autoscaler) getAverageMemory(ips []string) (float64, error) {
	if len(ips) == 0 {
		return 0, nil
	}

	query := fmt.Sprintf(`avg((1 - (node_memory_MemAvailable_bytes{instance=~"%s:9100"} / node_memory_MemTotal_bytes{instance=~"%s:9100"})) * 100)`,
		strings.Join(ips, ":9100|"), strings.Join(ips, ":9100|"))

	return a.queryPrometheus(query)
}

// queryPrometheus queries Prometheus and returns a single metric value
func (a *Autoscaler) queryPrometheus(query string) (float64, error) {
	reqURL := fmt.Sprintf("%s/api/v1/query?query=%s", a.prometheusURL, url.QueryEscape(query))

	resp, err := http.Get(reqURL)
	if err != nil {
		return 0, fmt.Errorf("failed to query Prometheus: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, fmt.Errorf("failed to read response: %w", err)
	}

	var promResp PrometheusResponse
	if err := json.Unmarshal(body, &promResp); err != nil {
		return 0, fmt.Errorf("failed to parse response: %w", err)
	}

	if promResp.Status != "success" || len(promResp.Data.Result) == 0 {
		return 0, nil // Return 0 if no data
	}

	// Extract value
	valueStr, ok := promResp.Data.Result[0].Value[1].(string)
	if !ok {
		return 0, fmt.Errorf("invalid metric value type")
	}

	var value float64
	if _, err := fmt.Sscanf(valueStr, "%f", &value); err != nil {
		return 0, fmt.Errorf("failed to parse metric value: %w", err)
	}

	return value, nil
}

// scaleUp adds a new server to the pool
func (a *Autoscaler) scaleUp(ctx context.Context, roleLabel string, poolName string) error {
	a.log("info", fmt.Sprintf("[%s] Scaling up...", poolName))

	// Get existing servers to determine next index
	servers, err := a.hetznerClient.Server.AllWithOpts(ctx, hcloud.ServerListOpts{
		ListOpts: hcloud.ListOpts{
			LabelSelector: fmt.Sprintf("role=%s", roleLabel),
		},
	})
	if err != nil {
		return fmt.Errorf("failed to get existing servers: %w", err)
	}

	// Determine server configuration based on role
	var serverType, location string
	var baseName string
	switch roleLabel {
	case "lb":
		serverType = a.config.LoadBalancer.ServerType
		location = a.config.LoadBalancer.Location
		baseName = strings.TrimSuffix(a.config.Control.Name, "-control")
		baseName = fmt.Sprintf("%s-lb-%d", baseName, len(servers)+1)
	case "app":
		serverType = a.config.App.ServerType
		location = a.config.App.Location
		baseName = strings.TrimSuffix(a.config.Control.Name, "-control")
		baseName = fmt.Sprintf("%s-app-%d", baseName, len(servers)+1)
	default:
		return fmt.Errorf("unsupported role: %s", roleLabel)
	}

	// Get network (first network found)
	networks, err := a.hetznerClient.Network.All(ctx)
	if err != nil || len(networks) == 0 {
		return fmt.Errorf("failed to get network: %w", err)
	}
	network := networks[0]

	// Get firewall (first firewall found)
	firewalls, err := a.hetznerClient.Firewall.All(ctx)
	if err != nil || len(firewalls) == 0 {
		return fmt.Errorf("failed to get firewall: %w", err)
	}
	firewall := firewalls[0]

	// Get SSH key (first SSH key found)
	sshKeys, err := a.hetznerClient.SSHKey.All(ctx)
	if err != nil || len(sshKeys) == 0 {
		return fmt.Errorf("failed to get SSH key: %w", err)
	}
	sshKey := sshKeys[0]

	// Get server type object
	sType, _, err := a.hetznerClient.ServerType.GetByName(ctx, serverType)
	if err != nil || sType == nil {
		return fmt.Errorf("failed to get server type %s: %w", serverType, err)
	}

	// Get Flatcar snapshot
	img, _, err := a.hetznerClient.Image.GetByID(ctx, a.config.SnapshotID)
	if err != nil || img == nil {
		return fmt.Errorf("failed to get snapshot %d: %w", a.config.SnapshotID, err)
	}

	// Get location object
	loc, _, err := a.hetznerClient.Location.GetByName(ctx, location)
	if err != nil || loc == nil {
		return fmt.Errorf("failed to get location %s: %w", location, err)
	}

	// Create server
	a.log("info", fmt.Sprintf("[%s] Creating server: %s", poolName, baseName))
	opts := hcloud.ServerCreateOpts{
		Name:       baseName,
		ServerType: sType,
		Image:      img,
		Location:   loc,
		SSHKeys:    []*hcloud.SSHKey{sshKey},
		Networks:   []*hcloud.Network{network},
		Firewalls:  []*hcloud.ServerCreateFirewall{{Firewall: *firewall}},
		Labels: map[string]string{
			"role":      roleLabel,
			"managed":   "harbor",
			"autoscale": "true",
		},
	}

	result, _, err := a.hetznerClient.Server.Create(ctx, opts)
	if err != nil {
		return fmt.Errorf("failed to create server: %w", err)
	}

	server := result.Server
	a.log("info", fmt.Sprintf("[%s] Server created: %s (ID: %d, IP: %s)", poolName, server.Name, server.ID, server.PublicNet.IPv4.IP.String()))

	// Wait for server to be running
	a.log("info", fmt.Sprintf("[%s] Waiting for server to start...", poolName))
	for i := 0; i < 60; i++ {
		srv, _, err := a.hetznerClient.Server.GetByID(ctx, server.ID)
		if err == nil && srv.Status == hcloud.ServerStatusRunning {
			server = srv
			a.log("info", fmt.Sprintf("[%s] Server is running", poolName))
			break
		}
		time.Sleep(5 * time.Second)
	}

	// Get private IP
	privateIP := orchestrator.ExtractPrivateIP(server)

	// Get control plane IP for data plane configuration
	var controlPlaneIP string
	if roleLabel == "lb" {
		controlPlanes, err := a.hetznerClient.Server.AllWithOpts(ctx, hcloud.ServerListOpts{
			ListOpts: hcloud.ListOpts{
				LabelSelector: "role=control",
			},
		})
		if err != nil || len(controlPlanes) == 0 {
			return fmt.Errorf("failed to get control plane: %w", err)
		}
		controlPlaneIP = orchestrator.ExtractPrivateIP(controlPlanes[0])
	}

	// Deploy services using the deployer
	a.log("info", fmt.Sprintf("[%s] Deploying services on %s...", poolName, baseName))
	if err := a.deployer.DeployToServer(server.PublicNet.IPv4.IP.String(), roleLabel, controlPlaneIP); err != nil {
		return fmt.Errorf("failed to deploy services: %w", err)
	}

	// Update APISIX upstream if this is an app server
	if roleLabel == "app" && privateIP != "" {
		a.log("info", fmt.Sprintf("[%s] Adding %s to APISIX upstream", poolName, privateIP))
		if err := a.addToAPISIXUpstream(ctx, privateIP); err != nil {
			a.log("warn", fmt.Sprintf("[%s] Failed to update APISIX upstream: %v", poolName, err))
		} else {
			a.log("info", fmt.Sprintf("[%s] Successfully added to APISIX upstream", poolName))
		}
	}

	// Update Prometheus configuration with new server targets
	a.log("info", fmt.Sprintf("[%s] Updating Prometheus scrape targets", poolName))
	if err := a.updatePrometheusConfig(ctx); err != nil {
		a.log("warn", fmt.Sprintf("[%s] Failed to update Prometheus config: %v", poolName, err))
	} else {
		a.log("info", fmt.Sprintf("[%s] Prometheus configuration updated", poolName))
	}

	// Update k6 targets if k6 is enabled and we scaled data planes
	if a.config.K6.Enabled && roleLabel == "lb" {
		a.log("info", fmt.Sprintf("[%s] Updating k6 load balancer targets", poolName))
		if err := a.updateK6Config(ctx); err != nil {
			a.log("warn", fmt.Sprintf("[%s] Failed to update k6 config: %v", poolName, err))
		} else {
			a.log("info", fmt.Sprintf("[%s] k6 configuration updated and restarted", poolName))
		}
	}

	a.log("info", fmt.Sprintf("[%s] Scale up completed for %s", poolName, baseName))
	return nil
}

// scaleDown removes a server from the pool
func (a *Autoscaler) scaleDown(ctx context.Context, roleLabel string, poolName string) error {
	a.log("info", fmt.Sprintf("[%s] Scaling down...", poolName))

	// Get servers with autoscale label (don't remove manually created servers)
	servers, err := a.hetznerClient.Server.AllWithOpts(ctx, hcloud.ServerListOpts{
		ListOpts: hcloud.ListOpts{
			LabelSelector: fmt.Sprintf("role=%s,autoscale=true", roleLabel),
		},
	})
	if err != nil {
		return fmt.Errorf("failed to get servers: %w", err)
	}

	if len(servers) == 0 {
		a.log("warn", fmt.Sprintf("[%s] No autoscaled servers found to remove", poolName))
		return nil
	}

	// Choose server to remove (newest one - last in list by ID)
	var serverToRemove *hcloud.Server
	for _, srv := range servers {
		if serverToRemove == nil || srv.ID > serverToRemove.ID {
			serverToRemove = srv
		}
	}

	a.log("info", fmt.Sprintf("[%s] Removing server: %s (ID: %d)", poolName, serverToRemove.Name, serverToRemove.ID))

	// Get private IP for APISIX upstream removal
	privateIP := orchestrator.ExtractPrivateIP(serverToRemove)

	// Remove from APISIX upstream if app server
	if roleLabel == "app" && privateIP != "" {
		a.log("info", fmt.Sprintf("[%s] Removing %s from APISIX upstream", poolName, privateIP))
		if err := a.removeFromAPISIXUpstream(ctx, privateIP); err != nil {
			a.log("warn", fmt.Sprintf("[%s] Failed to remove from APISIX upstream: %v", poolName, err))
		} else {
			a.log("info", fmt.Sprintf("[%s] Successfully removed from APISIX upstream", poolName))
		}
	}

	// Power off server
	a.log("info", fmt.Sprintf("[%s] Powering off server %s...", poolName, serverToRemove.Name))
	_, _, err = a.hetznerClient.Server.Poweroff(ctx, serverToRemove)
	if err != nil {
		a.log("warn", fmt.Sprintf("[%s] Failed to power off server: %v", poolName, err))
	}

	// Wait a moment for graceful shutdown
	time.Sleep(10 * time.Second)

	// Delete server
	a.log("info", fmt.Sprintf("[%s] Deleting server %s...", poolName, serverToRemove.Name))
	_, _, err = a.hetznerClient.Server.DeleteWithResult(ctx, serverToRemove)
	if err != nil {
		return fmt.Errorf("failed to delete server: %w", err)
	}

	// Update Prometheus configuration to remove server from targets
	a.log("info", fmt.Sprintf("[%s] Updating Prometheus scrape targets", poolName))
	if err := a.updatePrometheusConfig(ctx); err != nil {
		a.log("warn", fmt.Sprintf("[%s] Failed to update Prometheus config: %v", poolName, err))
	} else {
		a.log("info", fmt.Sprintf("[%s] Prometheus configuration updated", poolName))
	}

	// Update k6 targets if k6 is enabled and we scaled data planes
	if a.config.K6.Enabled && roleLabel == "lb" {
		a.log("info", fmt.Sprintf("[%s] Updating k6 load balancer targets", poolName))
		if err := a.updateK6Config(ctx); err != nil {
			a.log("warn", fmt.Sprintf("[%s] Failed to update k6 config: %v", poolName, err))
		} else {
			a.log("info", fmt.Sprintf("[%s] k6 configuration updated and restarted", poolName))
		}
	}

	a.log("info", fmt.Sprintf("[%s] Scale down completed - removed %s", poolName, serverToRemove.Name))
	return nil
}

// addToAPISIXUpstream adds a server to the APISIX upstream
func (a *Autoscaler) addToAPISIXUpstream(ctx context.Context, privateIP string) error {
	// Get current upstream configuration
	upstream, err := a.apisixClient.GetUpstream("web")
	if err != nil {
		return fmt.Errorf("failed to get upstream: %w", err)
	}

	// Extract nodes from response
	var nodes map[string]interface{}
	if value, ok := upstream["value"]; ok {
		if valueMap, ok := value.(map[string]interface{}); ok {
			if nodesVal, ok := valueMap["nodes"]; ok {
				if nodesMap, ok := nodesVal.(map[string]interface{}); ok {
					nodes = nodesMap
				}
			}
		}
	}

	if nodes == nil {
		nodes = make(map[string]interface{})
	}

	// Add new node
	nodeKey := fmt.Sprintf("%s:80", privateIP)
	nodes[nodeKey] = 1

	// Update upstream with just the nodes
	update := map[string]interface{}{
		"nodes": nodes,
	}

	if err := a.apisixClient.UpdateUpstream("web", update); err != nil {
		return fmt.Errorf("failed to update upstream: %w", err)
	}

	return nil
}

// removeFromAPISIXUpstream removes a server from the APISIX upstream
func (a *Autoscaler) removeFromAPISIXUpstream(ctx context.Context, privateIP string) error {
	// Get current upstream configuration
	upstream, err := a.apisixClient.GetUpstream("web")
	if err != nil {
		return fmt.Errorf("failed to get upstream: %w", err)
	}

	// Extract nodes from response
	var nodes map[string]interface{}
	if value, ok := upstream["value"]; ok {
		if valueMap, ok := value.(map[string]interface{}); ok {
			if nodesVal, ok := valueMap["nodes"]; ok {
				if nodesMap, ok := nodesVal.(map[string]interface{}); ok {
					nodes = nodesMap
				}
			}
		}
	}

	if nodes == nil {
		return fmt.Errorf("no nodes found in upstream")
	}

	// Remove node
	nodeKey := fmt.Sprintf("%s:80", privateIP)
	delete(nodes, nodeKey)

	// Update upstream with just the nodes
	update := map[string]interface{}{
		"nodes": nodes,
	}

	if err := a.apisixClient.UpdateUpstream("web", update); err != nil {
		return fmt.Errorf("failed to update upstream: %w", err)
	}

	return nil
}

// updatePrometheusConfig updates Prometheus scrape targets with current servers
func (a *Autoscaler) updatePrometheusConfig(ctx context.Context) error {
	// Get control plane
	controlPlanes, err := a.hetznerClient.Server.AllWithOpts(ctx, hcloud.ServerListOpts{
		ListOpts: hcloud.ListOpts{
			LabelSelector: "role=control",
		},
	})
	if err != nil || len(controlPlanes) == 0 {
		return fmt.Errorf("failed to get control plane: %w", err)
	}
	controlPlane := controlPlanes[0]

	// Get all data planes
	dataPlanes, err := a.hetznerClient.Server.AllWithOpts(ctx, hcloud.ServerListOpts{
		ListOpts: hcloud.ListOpts{
			LabelSelector: "role=lb",
		},
	})
	if err != nil {
		return fmt.Errorf("failed to get data planes: %w", err)
	}

	// Get all app servers
	appServers, err := a.hetznerClient.Server.AllWithOpts(ctx, hcloud.ServerListOpts{
		ListOpts: hcloud.ListOpts{
			LabelSelector: "role=app",
		},
	})
	if err != nil {
		return fmt.Errorf("failed to get app servers: %w", err)
	}

	// Convert to models.Server format for deployer
	controlPlaneModel := &models.Server{
		PublicIP:  controlPlane.PublicNet.IPv4.IP.String(),
		PrivateIP: orchestrator.ExtractPrivateIP(controlPlane),
	}

	var dataPlaneModels []*models.Server
	for _, dp := range dataPlanes {
		dataPlaneModels = append(dataPlaneModels, &models.Server{
			PrivateIP: orchestrator.ExtractPrivateIP(dp),
		})
	}

	var appServerModels []*models.Server
	for _, as := range appServers {
		appServerModels = append(appServerModels, &models.Server{
			PrivateIP: orchestrator.ExtractPrivateIP(as),
		})
	}

	// Call deployer's UpdatePrometheusConfig
	return a.deployer.UpdatePrometheusConfig(controlPlaneModel, dataPlaneModels, appServerModels)
}

// updateK6Config updates k6 load balancer targets by recreating the container
func (a *Autoscaler) updateK6Config(ctx context.Context) error {
	// Get control plane
	controlPlanes, err := a.hetznerClient.Server.AllWithOpts(ctx, hcloud.ServerListOpts{
		ListOpts: hcloud.ListOpts{
			LabelSelector: "role=control",
		},
	})
	if err != nil || len(controlPlanes) == 0 {
		return fmt.Errorf("failed to get control plane: %w", err)
	}
	controlPlane := controlPlanes[0]

	// Get all data planes
	dataPlanes, err := a.hetznerClient.Server.AllWithOpts(ctx, hcloud.ServerListOpts{
		ListOpts: hcloud.ListOpts{
			LabelSelector: "role=lb",
		},
	})
	if err != nil {
		return fmt.Errorf("failed to get data planes: %w", err)
	}

	// Build LB_TARGETS comma-separated list
	var targets []string
	for _, dp := range dataPlanes {
		privateIP := orchestrator.ExtractPrivateIP(dp)
		if privateIP != "" {
			targets = append(targets, fmt.Sprintf("http://%s", privateIP))
		}
	}
	lbTargets := strings.Join(targets, ",")

	// SSH to control plane to recreate k6 container
	publicIP := controlPlane.PublicNet.IPv4.IP.String()

	// Get the actual docker network name (docker compose prefixes it)
	networkCmd := "docker network ls --filter name=apisix --format '{{.Name}}' | head -1"
	networkName, err := a.executeSSHCommand(publicIP, networkCmd)
	if err != nil || networkName == "" {
		networkName = "harbor_apisix" // Default fallback
	}
	networkName = strings.TrimSpace(networkName)

	// Stop and remove existing k6 container
	// We can't use docker-compose because it has the old LB_TARGETS baked into the yml file
	// Instead, we'll remove the container and run it directly with docker run
	removeCmd := "docker rm -f k6 2>/dev/null || true"
	if _, err := a.executeSSHCommand(publicIP, removeCmd); err != nil {
		a.log("warn", fmt.Sprintf("Failed to remove k6 container (may not exist): %v", err))
	}

	// Apply defaults for k6 config
	orchestrator.ApplyK6Defaults(&a.config.K6)

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
		-v /var/lib/harbor/k6:/scripts:ro \
		grafana/k6:latest run /scripts/loadtest.js`,
		networkName,
		lbTargets,
		a.config.K6.Rate,
		a.config.K6.Duration,
		a.config.K6.PreallocatedVUs,
		a.config.K6.MaxVUs,
		a.config.K6.TargetPath,
		a.config.K6.ConnectionTimeout,
		a.config.K6.RequestTimeout,
		a.config.K6.GracefulStop)

	if output, err := a.executeSSHCommand(publicIP, runCmd); err != nil {
		return fmt.Errorf("failed to start k6 container: %w (output: %s)", err, output)
	}

	return nil
}

// executeSSHCommand executes a command via SSH on a server
func (a *Autoscaler) executeSSHCommand(host string, command string) (string, error) {
	// Get SSH key path
	sshKeyPath := os.Getenv("SSH_KEY_PATH")
	if sshKeyPath == "" {
		homeDir, _ := os.UserHomeDir()
		sshKeyPath = fmt.Sprintf("%s/.harbor/ssh/id_rsa", homeDir)
	}

	// Create SSH client
	sshClient, err := ssh.New(host, "root", sshKeyPath)
	if err != nil {
		return "", fmt.Errorf("failed to create SSH client: %w", err)
	}
	defer sshClient.Close()

	output, err := sshClient.Execute(command)
	return output, err
}

// log logs a message
func (a *Autoscaler) log(level, message string) {
	fmt.Printf("[autoscaler] [%s] %s\n", level, message)
}
