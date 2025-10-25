package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Config represents the main configuration structure
type Config struct {
	Provider     string           `yaml:"provider"`
	SnapshotID   int64            `yaml:"snapshot_id"` // Hetzner snapshot ID for Flatcar Linux (applies to all servers)
	Control      ServerConfig     `yaml:"control"`
	Network      NetworkConfig    `yaml:"network"`
	Firewall     FirewallConfig   `yaml:"firewall"`
	LoadBalancer PoolConfig       `yaml:"loadbalancer"`
	App          PoolConfig       `yaml:"app"`
	APISIX       APISIXConfig     `yaml:"apisix"`
	Monitoring   MonitoringConfig `yaml:"monitoring"`
	Autoscaler   AutoscalerConfig `yaml:"autoscaler"`
	K6           K6Config         `yaml:"k6"`
}

// ServerConfig represents server configuration
type ServerConfig struct {
	Name     string `yaml:"name"`
	Type     string `yaml:"type"`
	Location string `yaml:"location"`
}

// NetworkConfig represents network configuration
type NetworkConfig struct {
	Name        string `yaml:"name"`
	IPRange     string `yaml:"ip_range"`
	SubnetRange string `yaml:"subnet_range"`
}

// FirewallConfig represents firewall configuration
type FirewallConfig struct {
	Name  string         `yaml:"name"`
	Rules []FirewallRule `yaml:"rules"`
}

// FirewallRule represents a firewall rule
type FirewallRule struct {
	Direction   string   `yaml:"direction"`
	Port        string   `yaml:"port"`
	Protocol    string   `yaml:"protocol"`
	SourceIPs   []string `yaml:"source_ips"`
	Description string   `yaml:"description"`
}

// PoolConfig represents a server pool configuration
type PoolConfig struct {
	Enabled     bool   `yaml:"enabled"`
	Replicas    int    `yaml:"replicas"`
	ServerType  string `yaml:"server_type"`
	Location    string `yaml:"location"`
	ServiceName string `yaml:"service_name"`
	ComposeFile string `yaml:"compose_file"` // Path to docker-compose.yml file for this pool
}

// APISIXConfig represents APISIX configuration
type APISIXConfig struct {
	AdminPort   int                `yaml:"admin_port"`
	APIKey      string             `yaml:"api_key"`
	Upstreams   []APISIXUpstream   `yaml:"upstreams"`
	GlobalRules []APISIXGlobalRule `yaml:"global_rules"`
	Routes      []APISIXRoute      `yaml:"routes"`
	SSL         APISIXSSLConfig    `yaml:"ssl"`
}

// APISIXUpstream represents an APISIX upstream configuration
type APISIXUpstream struct {
	ID                   string                 `yaml:"id"`
	Name                 string                 `yaml:"name"`
	Nodes                map[string]interface{} `yaml:"nodes"`
	EnableHealthCheck    bool                   `yaml:"enable_health_check"`
	HealthCheckPath      string                 `yaml:"health_check_path"`
	HealthyInterval      int                    `yaml:"healthy_interval"`
	UnhealthyInterval    int                    `yaml:"unhealthy_interval"`
	KeepaliveSize        int                    `yaml:"keepalive_size"`
	KeepaliveIdleTimeout int                    `yaml:"keepalive_idle_timeout"`
	KeepaliveRequests    int                    `yaml:"keepalive_requests"`
}

// APISIXGlobalRule represents an APISIX global rule
type APISIXGlobalRule struct {
	ID      string                 `yaml:"id"`
	Plugins map[string]interface{} `yaml:"plugins"`
}

// APISIXRoute represents an APISIX route
type APISIXRoute struct {
	ID         string                 `yaml:"id"`
	Name       string                 `yaml:"name"`
	Methods    []string               `yaml:"methods"`
	Host       string                 `yaml:"host,omitempty"`
	URI        string                 `yaml:"uri"`
	UpstreamID string                 `yaml:"upstream_id"`
	Plugins    map[string]interface{} `yaml:"plugins"`
}

// APISIXSSLConfig represents APISIX SSL configuration
type APISIXSSLConfig struct {
	ID            string   `yaml:"id"`
	CertPath      string   `yaml:"cert_path"`
	KeyPath       string   `yaml:"key_path"`
	ClientCAPath  string   `yaml:"client_ca_path"`
	ClientCADepth int      `yaml:"client_ca_depth"`
	SNIs          []string `yaml:"snis"`
	SSLProtocols  []string `yaml:"ssl_protocols"`
}

// MonitoringConfig represents monitoring configuration
type MonitoringConfig struct {
	Prometheus   ServiceConfig `yaml:"prometheus"`
	CAdvisor     ServiceConfig `yaml:"cadvisor"`
	NodeExporter ServiceConfig `yaml:"node_exporter"`
}

// ServiceConfig represents a service configuration with port
type ServiceConfig struct {
	Port int `yaml:"port"`
}

// AutoscalerConfig represents autoscaler configuration
type AutoscalerConfig struct {
	Enabled          bool    `yaml:"enabled"`
	CheckInterval    int     `yaml:"check_interval"`     // Seconds between checks
	Cooldown         int     `yaml:"cooldown"`           // Seconds to wait after scaling
	CPUThresholdUp   float64 `yaml:"cpu_threshold_up"`   // CPU % to scale up
	CPUThresholdDown float64 `yaml:"cpu_threshold_down"` // CPU % to scale down
	MemThresholdUp   float64 `yaml:"mem_threshold_up"`   // Memory % to scale up
	MemThresholdDown float64 `yaml:"mem_threshold_down"` // Memory % to scale down
	MinReplicas      int     `yaml:"min_replicas"`       // Minimum replicas per pool
	MaxReplicas      int     `yaml:"max_replicas"`       // Maximum replicas per pool
}

// K6Config represents k6 load testing configuration
type K6Config struct {
	Enabled           bool   `yaml:"enabled"`
	PreallocatedVUs   int    `yaml:"preallocated_vus"`   // Number of VUs to preallocate
	MaxVUs            int    `yaml:"max_vus"`            // Maximum number of VUs
	Rate              int    `yaml:"rate"`               // Requests per second
	Duration          string `yaml:"duration"`           // Test duration (e.g., "30s", "5m")
	TargetPath        string `yaml:"target_path"`        // Path to test (e.g., "/")
	ConnectionTimeout string `yaml:"connection_timeout"` // Connection timeout (default: "10s")
	RequestTimeout    string `yaml:"request_timeout"`    // Request timeout (default: "30s")
	GracefulStop      string `yaml:"graceful_stop"`      // Graceful stop duration (default: "30s")
}

// Load loads configuration from a YAML file
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	var config Config
	if err := yaml.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("failed to parse config file: %w", err)
	}

	if err := config.Validate(); err != nil {
		return nil, fmt.Errorf("config validation failed: %w", err)
	}

	return &config, nil
}

// Validate validates the configuration
func (c *Config) Validate() error {
	if c.Provider != "hetzner" && c.Provider != "digitalocean" {
		return fmt.Errorf("invalid provider: %s (must be 'hetzner' or 'digitalocean')", c.Provider)
	}

	if c.Control.Name == "" {
		return fmt.Errorf("control plane name is required")
	}

	if c.Network.Name == "" {
		return fmt.Errorf("network name is required")
	}

	if c.Firewall.Name == "" {
		return fmt.Errorf("firewall name is required")
	}

	if c.LoadBalancer.Enabled && c.LoadBalancer.Replicas < 1 {
		return fmt.Errorf("load balancer pool replicas must be at least 1")
	}

	if c.App.Enabled && c.App.Replicas < 1 {
		return fmt.Errorf("app pool replicas must be at least 1")
	}

	return nil
}
