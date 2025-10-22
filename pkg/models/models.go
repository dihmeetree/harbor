package models

import "time"

// ServerRole defines the role of a server in the infrastructure
type ServerRole string

const (
	RoleControlPlane ServerRole = "control_plane"
	RoleDataPlane    ServerRole = "data_plane"
	RoleApp          ServerRole = "app"
)

// Network represents a Hetzner private network
type Network struct {
	ID          int64
	HetznerID   int64
	Name        string
	IPRange     string
	SubnetRange string
	CreatedAt   time.Time
}

// Server represents a Hetzner cloud server
type Server struct {
	ID        int64
	HetznerID int64
	Name      string
	Type      string
	Role      ServerRole
	PublicIP  string
	PrivateIP string
	Location  string
	Image     string
	Status    string
	NetworkID int64
	CreatedAt time.Time
}

// Firewall represents a Hetzner firewall
type Firewall struct {
	ID        int64
	HetznerID int64
	Name      string
	CreatedAt time.Time
	Rules     []FirewallRule
}

// FirewallRule represents a firewall rule
type FirewallRule struct {
	ID          int64
	FirewallID  int64
	Direction   string
	Port        string
	Protocol    string
	SourceIPs   []string
	Description string
}

// SSHKey represents an SSH key
type SSHKey struct {
	ID        int64
	HetznerID int64
	Name      string
	PublicKey string
	CreatedAt time.Time
}

// Deployment represents a deployment operation
type Deployment struct {
	ID          int64
	ConfigHash  string
	Status      string
	StartedAt   time.Time
	CompletedAt *time.Time
}

// DeploymentLog represents a log entry for a deployment
type DeploymentLog struct {
	ID           int64
	DeploymentID int64
	Timestamp    time.Time
	Level        string
	Message      string
}
