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

// Install installs Docker on a remote server using the official installation script
func (i *Installer) Install(sshClient *ssh.Client) error {
	// Check if Docker is already installed
	_, err := sshClient.Execute("docker --version")
	if err == nil {
		return nil // Docker already installed
	}

	// Install Docker using the official convenience script
	installCmd := "curl -fsSL https://get.docker.com | sh"
	if _, err := sshClient.Execute(installCmd); err != nil {
		return fmt.Errorf("failed to install Docker: %w", err)
	}

	// Add current user to docker group (assuming root or user with sudo)
	if _, err := sshClient.Execute("usermod -aG docker $USER"); err != nil {
		// Non-fatal if this fails
		fmt.Printf("Warning: failed to add user to docker group: %v\n", err)
	}

	// Enable and start Docker service
	if _, err := sshClient.Execute("systemctl enable docker"); err != nil {
		return fmt.Errorf("failed to enable Docker service: %w", err)
	}

	if _, err := sshClient.Execute("systemctl start docker"); err != nil {
		return fmt.Errorf("failed to start Docker service: %w", err)
	}

	// Verify Docker is running
	output, err := sshClient.Execute("docker --version")
	if err != nil {
		return fmt.Errorf("docker installation verification failed: %w", err)
	}

	if !strings.Contains(output, "Docker version") {
		return fmt.Errorf("docker installation verification failed: unexpected output")
	}

	return nil
}

// IsInstalled checks if Docker is installed on a remote server
func (i *Installer) IsInstalled(sshClient *ssh.Client) (bool, error) {
	_, err := sshClient.Execute("docker --version")
	if err != nil {
		return false, nil
	}
	return true, nil
}

// DeployComposeFile deploys a docker-compose file to a remote server
func (i *Installer) DeployComposeFile(sshClient *ssh.Client, composeContent, remotePath string) error {
	// Write compose file
	if err := sshClient.WriteFile(remotePath, composeContent); err != nil {
		return fmt.Errorf("failed to write compose file: %w", err)
	}

	return nil
}

// ComposeUp runs docker-compose up on a remote server
func (i *Installer) ComposeUp(sshClient *ssh.Client, composeDir string) error {
	cmd := fmt.Sprintf("cd %s && docker compose up -d", composeDir)
	if _, err := sshClient.Execute(cmd); err != nil {
		return fmt.Errorf("failed to run docker compose up: %w", err)
	}

	return nil
}

// ComposeDown runs docker-compose down on a remote server
func (i *Installer) ComposeDown(sshClient *ssh.Client, composeDir string) error {
	cmd := fmt.Sprintf("cd %s && docker compose down", composeDir)
	if _, err := sshClient.Execute(cmd); err != nil {
		return fmt.Errorf("failed to run docker compose down: %w", err)
	}

	return nil
}

// GetRunningContainers gets a list of running containers on a remote server
func (i *Installer) GetRunningContainers(sshClient *ssh.Client) (string, error) {
	output, err := sshClient.Execute("docker ps --format '{{.Names}}'")
	if err != nil {
		return "", fmt.Errorf("failed to get running containers: %w", err)
	}

	return output, nil
}

// PullImage pulls a Docker image on a remote server
func (i *Installer) PullImage(sshClient *ssh.Client, image string) error {
	cmd := fmt.Sprintf("docker pull %s", image)
	if _, err := sshClient.Execute(cmd); err != nil {
		return fmt.Errorf("failed to pull image %s: %w", image, err)
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

// RunContainer runs a Docker container on a remote server
func (i *Installer) RunContainer(sshClient *ssh.Client, opts ContainerRunOpts) error {
	cmd := []string{"docker run -d"}

	// Add name
	if opts.Name != "" {
		cmd = append(cmd, fmt.Sprintf("--name %s", opts.Name))
	}

	// Add network
	if opts.Network != "" {
		cmd = append(cmd, fmt.Sprintf("--network %s", opts.Network))
	}

	// Add restart policy
	if opts.Restart != "" {
		cmd = append(cmd, fmt.Sprintf("--restart %s", opts.Restart))
	}

	// Add port mappings
	for _, port := range opts.Ports {
		cmd = append(cmd, fmt.Sprintf("-p %s", port))
	}

	// Add environment variables
	for key, value := range opts.Env {
		cmd = append(cmd, fmt.Sprintf("-e %s=%s", key, value))
	}

	// Add volumes
	for _, volume := range opts.Volumes {
		cmd = append(cmd, fmt.Sprintf("-v %s", volume))
	}

	// Add image
	cmd = append(cmd, opts.Image)

	// Add command
	if opts.Command != "" {
		cmd = append(cmd, opts.Command)
	}

	fullCmd := strings.Join(cmd, " ")
	if _, err := sshClient.Execute(fullCmd); err != nil {
		return fmt.Errorf("failed to run container: %w", err)
	}

	return nil
}

// ContainerRunOpts represents options for running a container
type ContainerRunOpts struct {
	Name    string
	Image   string
	Network string
	Restart string
	Ports   []string
	Env     map[string]string
	Volumes []string
	Command string
}

// StopContainer stops a Docker container
func (i *Installer) StopContainer(sshClient *ssh.Client, containerName string) error {
	cmd := fmt.Sprintf("docker stop %s", containerName)
	if _, err := sshClient.Execute(cmd); err != nil {
		return fmt.Errorf("failed to stop container %s: %w", containerName, err)
	}

	return nil
}

// RemoveContainer removes a Docker container
func (i *Installer) RemoveContainer(sshClient *ssh.Client, containerName string) error {
	cmd := fmt.Sprintf("docker rm -f %s", containerName)
	if _, err := sshClient.Execute(cmd); err != nil {
		return fmt.Errorf("failed to remove container %s: %w", containerName, err)
	}

	return nil
}
