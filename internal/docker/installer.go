package docker

import (
	"fmt"
	"strings"

	"github.com/dihmeetree/harbor/internal/ssh"
)

// Installer handles Docker installation on remote servers
type Installer struct{}

// New creates a new Docker installer
func New() *Installer {
	return &Installer{}
}

// Install verifies Docker is available and installs docker-compose on Flatcar Linux
func (i *Installer) Install(sshClient *ssh.Client) error {

	// Verify Docker is available (silently)
	_, err := sshClient.Execute("docker --version")
	if err != nil {
		return fmt.Errorf("docker not available: %w", err)
	}

	// Check if docker-compose is already installed
	_, err = sshClient.Execute("/opt/bin/docker-compose version")
	if err == nil {
		// Already installed - don't log anything
		return nil
	}

	// Create /opt/bin directory if it doesn't exist (need sudo for /opt)
	if _, err := sshClient.Execute("sudo mkdir -p /opt/bin"); err != nil {
		return fmt.Errorf("failed to create /opt/bin directory: %w", err)
	}

	// Download docker-compose binary (silently, need sudo to write to /opt/bin)
	const composeVersion = "v2.40.2"
	const composeURL = "https://github.com/docker/compose/releases/download/" + composeVersion + "/docker-compose-linux-x86_64"

	downloadCmd := fmt.Sprintf("sudo curl -sL %s -o /opt/bin/docker-compose", composeURL)
	if _, err := sshClient.Execute(downloadCmd); err != nil {
		return fmt.Errorf("failed to download docker-compose: %w", err)
	}

	// Make it executable
	chmodCmd := "sudo chmod +x /opt/bin/docker-compose"
	if _, err := sshClient.Execute(chmodCmd); err != nil {
		return fmt.Errorf("failed to make docker-compose executable: %w", err)
	}

	// Verify installation
	_, err = sshClient.Execute("/opt/bin/docker-compose version")
	if err != nil {
		return fmt.Errorf("docker-compose installation failed - unable to run docker-compose: %w", err)
	}

	return nil
}

// ComposeUp runs docker-compose up on a remote server
func (i *Installer) ComposeUp(sshClient *ssh.Client, composeDir string) error {
	cmd := fmt.Sprintf("cd %s && PATH=/opt/bin:$PATH docker-compose up -d", composeDir)

	if _, err := sshClient.Execute(cmd); err != nil {
		return fmt.Errorf("failed to run docker compose up: %w", err)
	}

	return nil
}

// ComposeDown runs docker-compose down on a remote server
func (i *Installer) ComposeDown(sshClient *ssh.Client, composeDir string) error {
	cmd := fmt.Sprintf("cd %s && PATH=/opt/bin:$PATH docker-compose down", composeDir)

	if _, err := sshClient.Execute(cmd); err != nil {
		return fmt.Errorf("failed to run docker compose down: %w", err)
	}

	return nil
}

// CreateNetwork creates a Docker network on a remote server
func (i *Installer) CreateNetwork(sshClient *ssh.Client, networkName string) error {
	// Check if network already exists
	checkCmd := fmt.Sprintf("docker network ls --format '{{.Name}}' | grep -w %s", networkName)
	output, _ := sshClient.Execute(checkCmd)
	if strings.TrimSpace(output) == networkName {
		return nil // Network already exists
	}

	// Create network
	cmd := fmt.Sprintf("docker network create %s", networkName)
	if _, err := sshClient.Execute(cmd); err != nil {
		return fmt.Errorf("failed to create network %s: %w", networkName, err)
	}

	return nil
}
