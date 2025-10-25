package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoad(t *testing.T) {
	tests := []struct {
		name      string
		yaml      string
		wantError bool
		errorMsg  string
	}{
		{
			name: "valid minimal config",
			yaml: `
provider: hetzner
snapshot_id: 123456
control:
  name: "test-control"
  type: "cx11"
  location: "nbg1"
network:
  name: "test-network"
  ip_range: "10.0.0.0/16"
  subnet_range: "10.0.1.0/24"
firewall:
  name: "test-firewall"
  rules: []
container:
  name: "nginx"
  image: "nginx:latest"
loadbalancer:
  enabled: false
app:
  enabled: false
apisix:
  admin_port: 9180
  api_key: "test-key"
monitoring:
  prometheus:
    port: 9090
  cadvisor:
    port: 8080
  node_exporter:
    port: 9100
autoscaler:
  enabled: false
k6:
  enabled: false
`,
			wantError: false,
		},
		{
			name: "invalid provider",
			yaml: `
provider: aws
control:
  name: "test-control"
`,
			wantError: true,
			errorMsg:  "invalid provider",
		},
		{
			name: "missing control name",
			yaml: `
provider: hetzner
control:
  type: "cx11"
  location: "nbg1"
network:
  name: "test-network"
firewall:
  name: "test-firewall"
`,
			wantError: true,
			errorMsg:  "control plane name is required",
		},
		{
			name: "missing network name",
			yaml: `
provider: hetzner
control:
  name: "test-control"
  type: "cx11"
  location: "nbg1"
firewall:
  name: "test-firewall"
`,
			wantError: true,
			errorMsg:  "network name is required",
		},
		{
			name: "missing firewall name",
			yaml: `
provider: hetzner
control:
  name: "test-control"
  type: "cx11"
  location: "nbg1"
network:
  name: "test-network"
`,
			wantError: true,
			errorMsg:  "firewall name is required",
		},
		{
			name: "invalid loadbalancer replicas",
			yaml: `
provider: hetzner
snapshot_id: 123456
control:
  name: "test-control"
  type: "cx11"
  location: "nbg1"
network:
  name: "test-network"
  ip_range: "10.0.0.0/16"
  subnet_range: "10.0.1.0/24"
firewall:
  name: "test-firewall"
  rules: []
loadbalancer:
  enabled: true
  replicas: 0
app:
  enabled: false
`,
			wantError: true,
			errorMsg:  "load balancer pool replicas must be at least 1",
		},
		{
			name: "invalid app replicas",
			yaml: `
provider: hetzner
snapshot_id: 123456
control:
  name: "test-control"
  type: "cx11"
  location: "nbg1"
network:
  name: "test-network"
  ip_range: "10.0.0.0/16"
  subnet_range: "10.0.1.0/24"
firewall:
  name: "test-firewall"
  rules: []
loadbalancer:
  enabled: false
app:
  enabled: true
  replicas: 0
`,
			wantError: true,
			errorMsg:  "app pool replicas must be at least 1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create temporary config file
			tmpDir := t.TempDir()
			configPath := filepath.Join(tmpDir, "config.yaml")
			if err := os.WriteFile(configPath, []byte(tt.yaml), 0644); err != nil {
				t.Fatalf("failed to write test config: %v", err)
			}

			// Load config
			cfg, err := Load(configPath)

			// Check error expectation
			if tt.wantError {
				if err == nil {
					t.Errorf("Load() expected error but got none")
					return
				}
				if tt.errorMsg != "" && !contains(err.Error(), tt.errorMsg) {
					t.Errorf("Load() error = %v, want error containing %q", err, tt.errorMsg)
				}
				return
			}

			if err != nil {
				t.Errorf("Load() unexpected error = %v", err)
				return
			}

			if cfg == nil {
				t.Error("Load() returned nil config")
			}
		})
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name      string
		config    Config
		wantError bool
		errorMsg  string
	}{
		{
			name: "valid digitalocean config",
			config: Config{
				Provider: "digitalocean",
				Control:  ServerConfig{Name: "test-control"},
				Network:  NetworkConfig{Name: "test-network"},
				Firewall: FirewallConfig{Name: "test-firewall"},
			},
			wantError: false,
		},
		{
			name: "valid hetzner config",
			config: Config{
				Provider: "hetzner",
				Control:  ServerConfig{Name: "test-control"},
				Network:  NetworkConfig{Name: "test-network"},
				Firewall: FirewallConfig{Name: "test-firewall"},
			},
			wantError: false,
		},
		{
			name: "invalid provider",
			config: Config{
				Provider: "aws",
			},
			wantError: true,
			errorMsg:  "invalid provider",
		},
		{
			name: "loadbalancer enabled with valid replicas",
			config: Config{
				Provider:     "hetzner",
				Control:      ServerConfig{Name: "test-control"},
				Network:      NetworkConfig{Name: "test-network"},
				Firewall:     FirewallConfig{Name: "test-firewall"},
				LoadBalancer: PoolConfig{Enabled: true, Replicas: 2},
			},
			wantError: false,
		},
		{
			name: "app enabled with valid replicas",
			config: Config{
				Provider: "hetzner",
				Control:  ServerConfig{Name: "test-control"},
				Network:  NetworkConfig{Name: "test-network"},
				Firewall: FirewallConfig{Name: "test-firewall"},
				App:      PoolConfig{Enabled: true, Replicas: 3},
			},
			wantError: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.config.Validate()

			if tt.wantError {
				if err == nil {
					t.Errorf("Validate() expected error but got none")
					return
				}
				if tt.errorMsg != "" && !contains(err.Error(), tt.errorMsg) {
					t.Errorf("Validate() error = %v, want error containing %q", err, tt.errorMsg)
				}
				return
			}

			if err != nil {
				t.Errorf("Validate() unexpected error = %v", err)
			}
		})
	}
}

func TestMonitoringConfigStructure(t *testing.T) {
	yaml := `
monitoring:
  prometheus:
    port: 9090
  cadvisor:
    port: 8080
  node_exporter:
    port: 9100
`
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.yaml")

	// Create minimal valid config
	fullYaml := `
provider: hetzner
snapshot_id: 123456
control:
  name: "test-control"
  type: "cx11"
  location: "nbg1"
network:
  name: "test-network"
  ip_range: "10.0.0.0/16"
  subnet_range: "10.0.1.0/24"
firewall:
  name: "test-firewall"
  rules: []
container:
  name: "nginx"
  image: "nginx:latest"
loadbalancer:
  enabled: false
app:
  enabled: false
apisix:
  admin_port: 9180
  api_key: "test-key"
` + yaml + `
autoscaler:
  enabled: false
k6:
  enabled: false
`

	if err := os.WriteFile(configPath, []byte(fullYaml), 0644); err != nil {
		t.Fatalf("failed to write test config: %v", err)
	}

	cfg, err := Load(configPath)
	if err != nil {
		t.Fatalf("Load() failed: %v", err)
	}

	// Verify new structure
	if cfg.Monitoring.Prometheus.Port != 9090 {
		t.Errorf("Prometheus.Port = %d, want 9090", cfg.Monitoring.Prometheus.Port)
	}
	if cfg.Monitoring.CAdvisor.Port != 8080 {
		t.Errorf("CAdvisor.Port = %d, want 8080", cfg.Monitoring.CAdvisor.Port)
	}
	if cfg.Monitoring.NodeExporter.Port != 9100 {
		t.Errorf("NodeExporter.Port = %d, want 9100", cfg.Monitoring.NodeExporter.Port)
	}
}

// Helper function
func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(substr) == 0 ||
		(len(s) > 0 && len(substr) > 0 && findSubstring(s, substr)))
}

func findSubstring(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
