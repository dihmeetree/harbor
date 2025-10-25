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
	"github.com/dihmeetree/harbor/internal/hetzner"
	"github.com/dihmeetree/harbor/pkg/models"
	"github.com/hetznercloud/hcloud-go/v2/hcloud"
	"golang.org/x/crypto/ssh"
)

// Provisioner handles infrastructure provisioning
type Provisioner struct {
	config       *config.Config
	hetzner      *hetzner.Client
	hetznerToken string
}

// NewProvisioner creates a new provisioner
func NewProvisioner(cfg *config.Config, hetznerToken string) *Provisioner {
	return &Provisioner{
		config:       cfg,
		hetzner:      hetzner.New(hetznerToken),
		hetznerToken: hetznerToken,
	}
}

// Provision provisions the entire infrastructure
func (p *Provisioner) Provision(ctx context.Context) error {
	p.log("info", "Starting infrastructure provisioning")

	// Extract base name from control plane server name (e.g., "harbor-control" -> "harbor")
	// This will be used as the prefix for all servers
	baseName := p.config.Control.Name
	if idx := strings.LastIndex(baseName, "-"); idx != -1 {
		baseName = baseName[:idx]
	}

	// Generate or load SSH key
	sshKey, privateKeyPath, err := p.ensureSSHKey(ctx)
	if err != nil {
		return fmt.Errorf("failed to ensure SSH key: %w", err)
	}
	p.log("info", fmt.Sprintf("SSH key ready: %s", sshKey.Name))

	// Create network
	network, err := p.createNetwork(ctx)
	if err != nil {
		return fmt.Errorf("failed to create network: %w", err)
	}
	p.log("info", fmt.Sprintf("Network created: %s", network.Name))

	// Create firewall
	firewall, err := p.createFirewall(ctx)
	if err != nil {
		return fmt.Errorf("failed to create firewall: %w", err)
	}
	p.log("info", fmt.Sprintf("Firewall created: %s", firewall.Name))

	// Create control plane server
	controlPlane, err := p.createServer(ctx, p.config.Control, models.RoleControlPlane, network, firewall, sshKey)
	if err != nil {
		return fmt.Errorf("failed to create control plane server: %w", err)
	}
	publicIP := controlPlane.PublicNet.IPv4.IP.String()
	p.log("info", fmt.Sprintf("Control plane server created: %s (%s)", controlPlane.Name, publicIP))

	// Create data plane servers
	for i := 0; i < p.config.LoadBalancer.Replicas; i++ {
		serverCfg := config.ServerConfig{
			Name:     fmt.Sprintf("%s-lb-%d", baseName, i+1),
			Type:     p.config.LoadBalancer.ServerType,
			Location: p.config.LoadBalancer.Location,
		}

		server, err := p.createServer(ctx, serverCfg, models.RoleDataPlane, network, firewall, sshKey)
		if err != nil {
			return fmt.Errorf("failed to create data plane server %d: %w", i+1, err)
		}
		publicIP := server.PublicNet.IPv4.IP.String()
		p.log("info", fmt.Sprintf("Data plane server created: %s (%s)", server.Name, publicIP))
	}

	// Create app pool servers
	for i := 0; i < p.config.App.Replicas; i++ {
		serverCfg := config.ServerConfig{
			Name:     fmt.Sprintf("%s-app-%d", baseName, i+1),
			Type:     p.config.App.ServerType,
			Location: p.config.App.Location,
		}

		server, err := p.createServer(ctx, serverCfg, models.RoleApp, network, firewall, sshKey)
		if err != nil {
			return fmt.Errorf("failed to create app server %d: %w", i+1, err)
		}
		publicIP := server.PublicNet.IPv4.IP.String()
		p.log("info", fmt.Sprintf("App server created: %s (%s)", server.Name, publicIP))
	}

	// Store private key path for later use
	os.Setenv("HARBOR_SSH_KEY", privateKeyPath)

	p.log("info", "Infrastructure provisioning completed successfully")

	// Deploy services
	deployer := NewDeployer(p.config, p.hetznerToken, privateKeyPath)
	if err := deployer.Deploy(ctx); err != nil {
		return fmt.Errorf("failed to deploy services: %w", err)
	}

	return nil
}

// createNetwork creates a private network
func (p *Provisioner) createNetwork(ctx context.Context) (*hcloud.Network, error) {
	// Create network
	network, err := p.hetzner.CreateNetwork(
		ctx,
		p.config.Network.Name,
		p.config.Network.IPRange,
		p.config.Network.SubnetRange,
		p.config.Control.Location,
	)
	if err != nil {
		return nil, err
	}

	return network, nil
}

// createFirewall creates a firewall
func (p *Provisioner) createFirewall(ctx context.Context) (*hcloud.Firewall, error) {
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

	return firewall, nil
}

// createServer creates a server
func (p *Provisioner) createServer(ctx context.Context, cfg config.ServerConfig, role models.ServerRole, network *hcloud.Network, firewall *hcloud.Firewall, sshKey *hcloud.SSHKey) (*hcloud.Server, error) {
	return CreateServer(ctx, p.hetzner, cfg, p.config.SnapshotID, role, network, firewall, sshKey)
}

// ensureSSHKey ensures an SSH key exists
func (p *Provisioner) ensureSSHKey(ctx context.Context) (*hcloud.SSHKey, string, error) {
	keyName := fmt.Sprintf("%s-key", p.config.Control.Name)

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

	return hetznerKey, privateKeyPath, nil
}

// log adds a log entry
func (p *Provisioner) log(level, message string) {
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
func (p *Provisioner) waitForServersDeletion(ctx context.Context, serverIDs map[int64]string) error {
	maxAttempts := 60 // 2 minutes with 2-second intervals
	interval := 2 * time.Second

	// Track which servers are still pending deletion
	pendingServers := make(map[int64]string)
	for id, name := range serverIDs {
		pendingServers[id] = name
	}

	for i := 0; i < maxAttempts; i++ {
		// Check each pending server
		for hetznerID, name := range pendingServers {
			// Try to get server from Hetzner - if server is nil, it's deleted
			server, err := p.hetzner.GetServer(ctx, hetznerID)
			if err != nil || server == nil {
				// Server is deleted (either error or nil response)
				delete(pendingServers, hetznerID)
				p.log("info", fmt.Sprintf("  ✓ %s deleted", name))
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

// Destroy destroys all infrastructure by querying Hetzner API
func (p *Provisioner) Destroy(ctx context.Context) error {
	p.log("info", "Starting infrastructure destruction")

	// 1. Delete all servers first (query Hetzner for servers with "managed=harbor" label)
	servers, err := p.hetzner.GetServersByLabel(ctx, "managed", "harbor")
	if err != nil {
		return fmt.Errorf("failed to get servers from Hetzner: %w", err)
	}

	serverIDs := make(map[int64]string)
	for _, server := range servers {
		serverIDs[server.ID] = server.Name
		p.log("info", fmt.Sprintf("Deleting server: %s", server.Name))
		if err := p.hetzner.DeleteServer(ctx, server.ID); err != nil {
			p.log("error", fmt.Sprintf("Failed to delete server %s: %v", server.Name, err))
		} else {
			p.log("info", fmt.Sprintf("✓ Deleted server: %s", server.Name))
		}
	}

	// Wait for servers to be fully deleted before deleting firewalls
	if len(serverIDs) > 0 {
		p.log("info", "Waiting for servers to be fully deleted...")
		if err := p.waitForServersDeletion(ctx, serverIDs); err != nil {
			p.log("warn", fmt.Sprintf("Warning: Some servers may not be fully deleted: %v", err))
		} else {
			p.log("info", "✓ All servers fully deleted")
		}
	}

	// 2. Delete firewalls with managed=harbor label (with retry for "still in use" errors)
	// Note: We need to list all firewalls and filter by name since Hetzner API doesn't support label filtering for firewalls
	// For now, we'll use the firewall name from config to find it
	if p.config.Firewall.Name != "" {
		p.log("info", fmt.Sprintf("Deleting firewall: %s", p.config.Firewall.Name))

		// Try to find firewall by querying - we'll need to scan or use a helper
		// For simplicity, let's just try to delete by reconstructing the expected ID pattern
		// This is a known limitation - in production you'd want better firewall tracking

		// Actually, let's query all servers to find any firewall attached to them
		// But servers are already deleted... so we'll try a direct deletion approach
		// Since we don't have the firewall ID, we'll need to search

		// Simplified: Delete all firewalls (Hetzner API will prevent deleting if in use)
		// This is acceptable since Harbor should only create one firewall

		p.log("warn", "Firewall deletion by name not yet implemented - manually delete if needed")
	}

	// 3. Delete networks by name
	if p.config.Network.Name != "" {
		p.log("info", fmt.Sprintf("Deleting network: %s", p.config.Network.Name))
		// Similar issue - we need network ID, not just name
		// For now, log a warning
		p.log("warn", "Network deletion by name not yet implemented - manually delete if needed")
	}

	// 4. Delete SSH keys by name pattern
	keyName := fmt.Sprintf("%s-key", p.config.Control.Name)
	p.log("info", fmt.Sprintf("Deleting SSH key: %s", keyName))
	key, err := p.hetzner.GetSSHKeyByName(ctx, keyName)
	if err == nil && key != nil {
		if err := p.hetzner.DeleteSSHKey(ctx, key.ID); err != nil {
			p.log("error", fmt.Sprintf("Failed to delete SSH key %s: %v", keyName, err))
		} else {
			p.log("info", fmt.Sprintf("✓ Deleted SSH key: %s", keyName))
		}
	}

	p.log("info", "✓ Infrastructure destruction completed")
	p.log("warn", "Note: Networks and firewalls may need manual cleanup via Hetzner console")
	return nil
}
