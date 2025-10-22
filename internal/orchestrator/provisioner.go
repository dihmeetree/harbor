package orchestrator

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/dihmeetree/harbor/internal/config"
	"github.com/dihmeetree/harbor/internal/database"
	"github.com/dihmeetree/harbor/internal/hetzner"
	"github.com/dihmeetree/harbor/pkg/models"
	"github.com/hetznercloud/hcloud-go/v2/hcloud"
	"golang.org/x/crypto/ssh"
)

// Provisioner handles infrastructure provisioning
type Provisioner struct {
	config     *config.Config
	hetzner    *hetzner.Client
	db         *database.DB
	netRepo    *database.NetworkRepository
	serverRepo *database.ServerRepository
	fwRepo     *database.FirewallRepository
	sshKeyRepo *database.SSHKeyRepository
	deployRepo *database.DeploymentRepository
}

// NewProvisioner creates a new provisioner
func NewProvisioner(cfg *config.Config, hetznerToken string, db *database.DB) *Provisioner {
	return &Provisioner{
		config:     cfg,
		hetzner:    hetzner.New(hetznerToken),
		db:         db,
		netRepo:    database.NewNetworkRepository(db),
		serverRepo: database.NewServerRepository(db),
		fwRepo:     database.NewFirewallRepository(db),
		sshKeyRepo: database.NewSSHKeyRepository(db),
		deployRepo: database.NewDeploymentRepository(db),
	}
}

// Provision provisions the entire infrastructure
func (p *Provisioner) Provision(ctx context.Context) error {
	// Create deployment record
	deployment := &models.Deployment{
		ConfigHash: "TODO",
		Status:     "started",
		StartedAt:  time.Now(),
	}
	if err := p.deployRepo.Create(deployment); err != nil {
		return fmt.Errorf("failed to create deployment record: %w", err)
	}

	p.log(deployment.ID, "info", "Starting infrastructure provisioning")

	// Extract base name from control plane server name (e.g., "harbor-control" -> "harbor")
	// This will be used as the prefix for all servers
	baseName := p.config.Server.Name
	if idx := strings.LastIndex(baseName, "-"); idx != -1 {
		baseName = baseName[:idx]
	}

	// Generate or load SSH key
	sshKey, privateKeyPath, err := p.ensureSSHKey(ctx)
	if err != nil {
		_ = p.deployRepo.UpdateStatus(deployment.ID, "failed")
		return fmt.Errorf("failed to ensure SSH key: %w", err)
	}
	p.log(deployment.ID, "info", fmt.Sprintf("SSH key ready: %s", sshKey.Name))

	// Create network
	network, err := p.createNetwork(ctx, deployment.ID)
	if err != nil {
		_ = p.deployRepo.UpdateStatus(deployment.ID, "failed")
		return fmt.Errorf("failed to create network: %w", err)
	}
	p.log(deployment.ID, "info", fmt.Sprintf("Network created: %s", network.Name))

	// Create firewall
	firewall, err := p.createFirewall(ctx, deployment.ID)
	if err != nil {
		_ = p.deployRepo.UpdateStatus(deployment.ID, "failed")
		return fmt.Errorf("failed to create firewall: %w", err)
	}
	p.log(deployment.ID, "info", fmt.Sprintf("Firewall created: %s", firewall.Name))

	// Create control plane server
	controlPlane, err := p.createServer(ctx, deployment.ID, p.config.Server, models.RoleControlPlane, network, firewall, sshKey)
	if err != nil {
		_ = p.deployRepo.UpdateStatus(deployment.ID, "failed")
		return fmt.Errorf("failed to create control plane server: %w", err)
	}
	publicIP := controlPlane.PublicNet.IPv4.IP.String()
	p.log(deployment.ID, "info", fmt.Sprintf("Control plane server created: %s (%s)", controlPlane.Name, publicIP))

	// Create data plane servers
	if p.config.LoadBalancer.Enabled {
		for i := 0; i < p.config.LoadBalancer.Replicas; i++ {
			serverCfg := config.ServerConfig{
				Name:     fmt.Sprintf("%s-lb-%d", baseName, i+1),
				Type:     p.config.LoadBalancer.ServerType,
				Location: p.config.LoadBalancer.Location,
				Image:    p.config.LoadBalancer.Image,
			}

			server, err := p.createServer(ctx, deployment.ID, serverCfg, models.RoleDataPlane, network, firewall, sshKey)
			if err != nil {
				_ = p.deployRepo.UpdateStatus(deployment.ID, "failed")
				return fmt.Errorf("failed to create data plane server %d: %w", i+1, err)
			}
			publicIP := server.PublicNet.IPv4.IP.String()
			p.log(deployment.ID, "info", fmt.Sprintf("Data plane server created: %s (%s)", server.Name, publicIP))
		}
	}

	// Create app pool servers
	if p.config.App.Enabled {
		for i := 0; i < p.config.App.Replicas; i++ {
			serverCfg := config.ServerConfig{
				Name:     fmt.Sprintf("%s-app-%d", baseName, i+1),
				Type:     p.config.App.ServerType,
				Location: p.config.App.Location,
				Image:    p.config.App.Image,
			}

			server, err := p.createServer(ctx, deployment.ID, serverCfg, models.RoleApp, network, firewall, sshKey)
			if err != nil {
				_ = p.deployRepo.UpdateStatus(deployment.ID, "failed")
				return fmt.Errorf("failed to create app server %d: %w", i+1, err)
			}
			publicIP := server.PublicNet.IPv4.IP.String()
			p.log(deployment.ID, "info", fmt.Sprintf("App server created: %s (%s)", server.Name, publicIP))
		}
	}

	// Store private key path for later use
	os.Setenv("HARBOR_SSH_KEY", privateKeyPath)

	p.log(deployment.ID, "info", "Infrastructure provisioning completed successfully")

	// Deploy services
	deployer := NewDeployer(p.config, p.db, privateKeyPath)
	if err := deployer.Deploy(ctx, deployment.ID); err != nil {
		_ = p.deployRepo.UpdateStatus(deployment.ID, "failed")
		return fmt.Errorf("failed to deploy services: %w", err)
	}

	_ = p.deployRepo.UpdateStatus(deployment.ID, "completed")
	p.log(deployment.ID, "info", "Deployment completed successfully")

	return nil
}

// createNetwork creates a private network
func (p *Provisioner) createNetwork(ctx context.Context, deploymentID int64) (*hcloud.Network, error) {
	// Check if network already exists
	existing, err := p.netRepo.GetByHetznerID(0)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		network, err := p.hetzner.GetNetwork(ctx, existing.HetznerID)
		if err != nil {
			return nil, err
		}
		return network, nil
	}

	// Create network
	network, err := p.hetzner.CreateNetwork(
		ctx,
		p.config.Network.Name,
		p.config.Network.IPRange,
		p.config.Network.SubnetRange,
		p.config.Server.Location,
	)
	if err != nil {
		return nil, err
	}

	// Save to database
	dbNetwork := &models.Network{
		HetznerID:   network.ID,
		Name:        network.Name,
		IPRange:     network.IPRange.String(),
		SubnetRange: network.Subnets[0].IPRange.String(),
		CreatedAt:   time.Now(),
	}
	if err := p.netRepo.Create(dbNetwork); err != nil {
		return nil, err
	}

	return network, nil
}

// createFirewall creates a firewall
func (p *Provisioner) createFirewall(ctx context.Context, _deploymentID int64) (*hcloud.Firewall, error) {
	// Convert config rules to Hetzner rules
	var rules []hcloud.FirewallRule
	for _, rule := range p.config.Firewall.Rules {
		// Replace 'current' with actual current IP
		sourceIPs := make([]net.IPNet, 0)
		for _, ip := range rule.SourceIPs {
			if ip == "current" {
				currentIP, err := getCurrentIP()
				if err != nil {
					return nil, fmt.Errorf("failed to get current IP: %w", err)
				}
				_, ipNet, _ := net.ParseCIDR(currentIP + "/32")
				sourceIPs = append(sourceIPs, *ipNet)
			} else {
				_, ipNet, err := net.ParseCIDR(ip)
				if err != nil {
					return nil, fmt.Errorf("invalid IP range %s: %w", ip, err)
				}
				sourceIPs = append(sourceIPs, *ipNet)
			}
		}

		hcloudRule := hcloud.FirewallRule{
			Direction:   hcloud.FirewallRuleDirection(rule.Direction),
			SourceIPs:   sourceIPs,
			Protocol:    hcloud.FirewallRuleProtocol(rule.Protocol),
			Description: &rule.Description,
		}

		if rule.Port != "" {
			hcloudRule.Port = &rule.Port
		}

		rules = append(rules, hcloudRule)
	}

	firewall, err := p.hetzner.CreateFirewall(ctx, p.config.Firewall.Name, rules)
	if err != nil {
		return nil, err
	}

	// Save to database
	dbFirewall := &models.Firewall{
		HetznerID: firewall.ID,
		Name:      firewall.Name,
		CreatedAt: time.Now(),
		Rules:     make([]models.FirewallRule, 0),
	}
	for _, rule := range p.config.Firewall.Rules {
		dbFirewall.Rules = append(dbFirewall.Rules, models.FirewallRule{
			Direction:   rule.Direction,
			Port:        rule.Port,
			Protocol:    rule.Protocol,
			SourceIPs:   rule.SourceIPs,
			Description: rule.Description,
		})
	}
	if err := p.fwRepo.Create(dbFirewall); err != nil {
		return nil, err
	}

	return firewall, nil
}

// createServer creates a server
func (p *Provisioner) createServer(ctx context.Context, deploymentID int64, cfg config.ServerConfig, role models.ServerRole, network *hcloud.Network, firewall *hcloud.Firewall, sshKey *hcloud.SSHKey) (*hcloud.Server, error) {
	// Get server type
	serverType, err := p.hetzner.GetServerType(ctx, cfg.Type)
	if err != nil {
		return nil, err
	}

	// Get image
	image, err := p.hetzner.GetImage(ctx, cfg.Image)
	if err != nil {
		return nil, err
	}

	// Get location
	location, err := p.hetzner.GetLocation(ctx, cfg.Location)
	if err != nil {
		return nil, err
	}

	// Determine role label for autoscaler
	var roleLabel string
	switch role {
	case models.RoleDataPlane:
		roleLabel = "lb"
	case models.RoleApp:
		roleLabel = "app"
	case models.RoleControlPlane:
		roleLabel = "control"
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

	server, err := p.hetzner.CreateServer(ctx, opts)
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
	dbNetwork, err := p.netRepo.GetByHetznerID(network.ID)
	if err != nil {
		return nil, fmt.Errorf("failed to get network from database: %w", err)
	}
	if dbNetwork == nil {
		return nil, fmt.Errorf("network not found in database")
	}

	// Save to database
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
	if err := p.serverRepo.Create(dbServer); err != nil {
		return nil, err
	}

	return server, nil
}

// ensureSSHKey ensures an SSH key exists
func (p *Provisioner) ensureSSHKey(ctx context.Context) (*hcloud.SSHKey, string, error) {
	keyName := fmt.Sprintf("%s-key", p.config.Server.Name)

	// Check if SSH_PRIVATE_KEY_PATH is provided
	existingKeyPath := os.Getenv("SSH_PRIVATE_KEY_PATH")
	if existingKeyPath != "" {
		// Check if key already exists in Hetzner
		key, err := p.hetzner.GetSSHKeyByName(ctx, keyName)
		if err == nil && key != nil {
			return key, existingKeyPath, nil
		}

		// Read existing public key
		publicKeyPath := existingKeyPath + ".pub"
		publicKeyBytes, err := os.ReadFile(publicKeyPath)
		if err != nil {
			return nil, "", fmt.Errorf("failed to read public key at %s: %w", publicKeyPath, err)
		}

		// Create SSH key in Hetzner using existing public key
		hetznerKey, err := p.hetzner.CreateSSHKey(ctx, keyName, string(publicKeyBytes))
		if err != nil {
			return nil, "", fmt.Errorf("failed to create SSH key in Hetzner: %w", err)
		}

		// Store in database
		dbKey := &models.SSHKey{
			HetznerID: hetznerKey.ID,
			Name:      hetznerKey.Name,
			PublicKey: hetznerKey.PublicKey,
			CreatedAt: time.Now(),
		}
		if err := p.sshKeyRepo.Create(dbKey); err != nil {
			return nil, "", fmt.Errorf("failed to store SSH key in database: %w", err)
		}

		return hetznerKey, existingKeyPath, nil
	}

	// No SSH_PRIVATE_KEY_PATH provided, generate new keys in project directory
	sshDir := ".harbor/ssh"
	if err := os.MkdirAll(sshDir, 0700); err != nil {
		return nil, "", fmt.Errorf("failed to create SSH directory: %w", err)
	}

	privateKeyPath := fmt.Sprintf("%s/%s", sshDir, keyName)
	publicKeyPath := fmt.Sprintf("%s/%s.pub", sshDir, keyName)

	// Check if keys already exist locally
	if _, err := os.Stat(privateKeyPath); err == nil {
		// Keys exist locally, read public key
		publicKeyBytes, err := os.ReadFile(publicKeyPath)
		if err != nil {
			return nil, "", fmt.Errorf("failed to read existing public key: %w", err)
		}

		// Check if key exists in Hetzner
		key, err := p.hetzner.GetSSHKeyByName(ctx, keyName)
		if err == nil && key != nil {
			return key, privateKeyPath, nil
		}

		// Create in Hetzner
		hetznerKey, err := p.hetzner.CreateSSHKey(ctx, keyName, string(publicKeyBytes))
		if err != nil {
			return nil, "", fmt.Errorf("failed to create SSH key in Hetzner: %w", err)
		}

		// Store in database
		dbKey := &models.SSHKey{
			HetznerID: hetznerKey.ID,
			Name:      hetznerKey.Name,
			PublicKey: hetznerKey.PublicKey,
			CreatedAt: time.Now(),
		}
		if err := p.sshKeyRepo.Create(dbKey); err != nil {
			return nil, "", fmt.Errorf("failed to store SSH key in database: %w", err)
		}

		return hetznerKey, privateKeyPath, nil
	}

	// Generate new SSH key pair
	privateKey, err := rsa.GenerateKey(rand.Reader, 4096)
	if err != nil {
		return nil, "", fmt.Errorf("failed to generate private key: %w", err)
	}

	// Encode private key
	privateKeyPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(privateKey),
	})

	// Generate public key
	publicKey, err := ssh.NewPublicKey(&privateKey.PublicKey)
	if err != nil {
		return nil, "", fmt.Errorf("failed to generate public key: %w", err)
	}
	publicKeyBytes := ssh.MarshalAuthorizedKey(publicKey)

	// Save private key
	if err := os.WriteFile(privateKeyPath, privateKeyPEM, 0600); err != nil {
		return nil, "", fmt.Errorf("failed to write private key: %w", err)
	}

	// Save public key
	if err := os.WriteFile(publicKeyPath, publicKeyBytes, 0644); err != nil {
		return nil, "", fmt.Errorf("failed to write public key: %w", err)
	}

	// Create SSH key in Hetzner
	hetznerKey, err := p.hetzner.CreateSSHKey(ctx, keyName, string(publicKeyBytes))
	if err != nil {
		return nil, "", fmt.Errorf("failed to create SSH key in Hetzner: %w", err)
	}

	// Store in database
	dbKey := &models.SSHKey{
		HetznerID: hetznerKey.ID,
		Name:      hetznerKey.Name,
		PublicKey: hetznerKey.PublicKey,
		CreatedAt: time.Now(),
	}
	if err := p.sshKeyRepo.Create(dbKey); err != nil {
		return nil, "", fmt.Errorf("failed to store SSH key in database: %w", err)
	}

	return hetznerKey, privateKeyPath, nil
}

// log adds a log entry to the deployment
func (p *Provisioner) log(deploymentID int64, level, message string) {
	_ = p.deployRepo.AddLog(deploymentID, level, message)
	fmt.Printf("[%s] %s\n", level, message)
}

// getCurrentIP gets the current public IP address
func getCurrentIP() (string, error) {
	resp, err := http.Get("https://ipv4.icanhazip.com")
	if err != nil {
		return "", fmt.Errorf("failed to get current IP: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read response: %w", err)
	}

	ip := strings.TrimSpace(string(body))
	if ip == "" {
		return "", fmt.Errorf("empty IP response")
	}

	return ip, nil
}

// waitForServersDeletion polls Hetzner API to ensure servers are fully deleted
func (p *Provisioner) waitForServersDeletion(ctx context.Context, servers []*models.Server) error {
	maxAttempts := 60 // 2 minutes with 2-second intervals
	interval := 2 * time.Second

	// Track which servers are still pending deletion
	pendingServers := make(map[int64]string)
	for _, server := range servers {
		pendingServers[server.HetznerID] = server.Name
	}

	for i := 0; i < maxAttempts; i++ {
		// Check each pending server
		for hetznerID, name := range pendingServers {
			// Try to get server from Hetzner - if it returns error, server is deleted
			_, err := p.hetzner.GetServer(ctx, hetznerID)
			if err != nil {
				// Server is deleted
				delete(pendingServers, hetznerID)
				p.log(0, "info", fmt.Sprintf("  ✓ %s deleted", name))
			}
		}

		if len(pendingServers) == 0 {
			return nil
		}

		time.Sleep(interval)
	}

	// Show which servers failed to delete
	remainingNames := make([]string, 0, len(pendingServers))
	for _, name := range pendingServers {
		remainingNames = append(remainingNames, name)
	}
	return fmt.Errorf("timeout waiting for servers to be deleted: %v", remainingNames)
}

func (p *Provisioner) Destroy(ctx context.Context) error {
	p.log(0, "info", "Starting infrastructure destruction")

	// 1. Delete all servers first
	servers, err := p.serverRepo.GetAll()
	if err != nil {
		return fmt.Errorf("failed to get servers: %w", err)
	}

	for _, server := range servers {
		p.log(0, "info", fmt.Sprintf("Deleting server: %s", server.Name))
		if err := p.hetzner.DeleteServer(ctx, server.HetznerID); err != nil {
			p.log(0, "error", fmt.Sprintf("Failed to delete server %s: %v", server.Name, err))
		} else {
			p.log(0, "info", fmt.Sprintf("✓ Deleted server: %s", server.Name))
		}
		if err := p.serverRepo.Delete(server.HetznerID); err != nil {
			p.log(0, "error", fmt.Sprintf("Failed to delete server from DB: %v", err))
		}
	}

	// Wait for servers to be fully deleted before deleting firewalls
	if len(servers) > 0 {
		p.log(0, "info", "Waiting for servers to be fully deleted...")
		if err := p.waitForServersDeletion(ctx, servers); err != nil {
			p.log(0, "warn", fmt.Sprintf("Warning: Some servers may not be fully deleted: %v", err))
		} else {
			p.log(0, "info", "✓ All servers fully deleted")
		}
	}

	// 2. Delete firewalls
	firewalls, err := p.fwRepo.GetAll()
	if err != nil {
		p.log(0, "error", fmt.Sprintf("Failed to get firewalls: %v", err))
	} else {
		for _, firewall := range firewalls {
			p.log(0, "info", fmt.Sprintf("Deleting firewall: %s", firewall.Name))
			if err := p.hetzner.DeleteFirewall(ctx, firewall.HetznerID); err != nil {
				p.log(0, "error", fmt.Sprintf("Failed to delete firewall %s: %v", firewall.Name, err))
			} else {
				p.log(0, "info", fmt.Sprintf("✓ Deleted firewall: %s", firewall.Name))
			}
			if err := p.fwRepo.Delete(firewall.HetznerID); err != nil {
				p.log(0, "error", fmt.Sprintf("Failed to delete firewall from DB: %v", err))
			}
		}
	}

	// 3. Delete networks
	networks, err := p.netRepo.GetAll()
	if err != nil {
		p.log(0, "error", fmt.Sprintf("Failed to get networks: %v", err))
	} else {
		for _, network := range networks {
			p.log(0, "info", fmt.Sprintf("Deleting network: %s", network.Name))
			if err := p.hetzner.DeleteNetwork(ctx, network.HetznerID); err != nil {
				p.log(0, "error", fmt.Sprintf("Failed to delete network %s: %v", network.Name, err))
			} else {
				p.log(0, "info", fmt.Sprintf("✓ Deleted network: %s", network.Name))
			}
			if err := p.netRepo.Delete(network.HetznerID); err != nil {
				p.log(0, "error", fmt.Sprintf("Failed to delete network from DB: %v", err))
			}
		}
	}

	// 4. Delete SSH keys
	sshKeys, err := p.sshKeyRepo.GetAll()
	if err != nil {
		p.log(0, "error", fmt.Sprintf("Failed to get SSH keys: %v", err))
	} else {
		for _, key := range sshKeys {
			p.log(0, "info", fmt.Sprintf("Deleting SSH key: %s", key.Name))
			if err := p.hetzner.DeleteSSHKey(ctx, key.HetznerID); err != nil {
				p.log(0, "error", fmt.Sprintf("Failed to delete SSH key %s: %v", key.Name, err))
			} else {
				p.log(0, "info", fmt.Sprintf("✓ Deleted SSH key: %s", key.Name))
			}
			if err := p.sshKeyRepo.Delete(key.HetznerID); err != nil {
				p.log(0, "error", fmt.Sprintf("Failed to delete SSH key from DB: %v", err))
			}
		}
	}

	p.log(0, "info", "✓ Infrastructure destruction completed")
	return nil
}
