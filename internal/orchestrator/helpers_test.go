package orchestrator

import (
	"net"
	"testing"

	"github.com/dihmeetree/harbor/internal/config"
	"github.com/dihmeetree/harbor/pkg/models"
	"github.com/hetznercloud/hcloud-go/v2/hcloud"
)

func TestExtractPrivateIP(t *testing.T) {
	tests := []struct {
		name   string
		server *hcloud.Server
		want   string
	}{
		{
			name: "server with private IP",
			server: &hcloud.Server{
				PrivateNet: []hcloud.ServerPrivateNet{
					{
						IP: net.ParseIP("10.0.1.5"),
					},
				},
			},
			want: "10.0.1.5",
		},
		{
			name:   "server without private IP",
			server: &hcloud.Server{},
			want:   "",
		},
		{
			name: "server with multiple private IPs",
			server: &hcloud.Server{
				PrivateNet: []hcloud.ServerPrivateNet{
					{
						IP: net.ParseIP("10.0.1.5"),
					},
					{
						IP: net.ParseIP("10.0.2.10"),
					},
				},
			},
			want: "10.0.1.5", // Should return first one
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ExtractPrivateIP(tt.server)
			if got != tt.want {
				t.Errorf("ExtractPrivateIP() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestHcloudToModels(t *testing.T) {
	tests := []struct {
		name   string
		server *hcloud.Server
		want   *models.Server
	}{
		{
			name: "server with all fields",
			server: &hcloud.Server{
				Name: "test-server",
				PublicNet: hcloud.ServerPublicNet{
					IPv4: hcloud.ServerPublicNetIPv4{
						IP: net.ParseIP("1.2.3.4"),
					},
				},
				PrivateNet: []hcloud.ServerPrivateNet{
					{
						IP: net.ParseIP("10.0.1.5"),
					},
				},
			},
			want: &models.Server{
				Name:      "test-server",
				PublicIP:  "1.2.3.4",
				PrivateIP: "10.0.1.5",
			},
		},
		{
			name: "server without private IP",
			server: &hcloud.Server{
				Name: "test-server-2",
				PublicNet: hcloud.ServerPublicNet{
					IPv4: hcloud.ServerPublicNetIPv4{
						IP: net.ParseIP("5.6.7.8"),
					},
				},
			},
			want: &models.Server{
				Name:      "test-server-2",
				PublicIP:  "5.6.7.8",
				PrivateIP: "",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := HcloudToModels(tt.server)
			if got.Name != tt.want.Name {
				t.Errorf("HcloudToModels().Name = %v, want %v", got.Name, tt.want.Name)
			}
			if got.PublicIP != tt.want.PublicIP {
				t.Errorf("HcloudToModels().PublicIP = %v, want %v", got.PublicIP, tt.want.PublicIP)
			}
			if got.PrivateIP != tt.want.PrivateIP {
				t.Errorf("HcloudToModels().PrivateIP = %v, want %v", got.PrivateIP, tt.want.PrivateIP)
			}
		})
	}
}

func TestHcloudListToModels(t *testing.T) {
	servers := []*hcloud.Server{
		{
			Name: "server-1",
			PublicNet: hcloud.ServerPublicNet{
				IPv4: hcloud.ServerPublicNetIPv4{
					IP: net.ParseIP("1.2.3.4"),
				},
			},
			PrivateNet: []hcloud.ServerPrivateNet{
				{
					IP: net.ParseIP("10.0.1.1"),
				},
			},
		},
		{
			Name: "server-2",
			PublicNet: hcloud.ServerPublicNet{
				IPv4: hcloud.ServerPublicNetIPv4{
					IP: net.ParseIP("5.6.7.8"),
				},
			},
			PrivateNet: []hcloud.ServerPrivateNet{
				{
					IP: net.ParseIP("10.0.1.2"),
				},
			},
		},
	}

	result := HcloudListToModels(servers)

	if len(result) != 2 {
		t.Errorf("HcloudListToModels() returned %d servers, want 2", len(result))
	}

	if result[0].Name != "server-1" {
		t.Errorf("HcloudListToModels()[0].Name = %v, want server-1", result[0].Name)
	}
	if result[1].Name != "server-2" {
		t.Errorf("HcloudListToModels()[1].Name = %v, want server-2", result[1].Name)
	}
}

func TestRoleToLabel(t *testing.T) {
	tests := []struct {
		name string
		role models.ServerRole
		want string
	}{
		{
			name: "data plane role",
			role: models.RoleDataPlane,
			want: "lb",
		},
		{
			name: "app role",
			role: models.RoleApp,
			want: "app",
		},
		{
			name: "control plane role",
			role: models.RoleControlPlane,
			want: "control",
		},
		{
			name: "unknown role",
			role: models.ServerRole("unknown"),
			want: "unknown",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := RoleToLabel(tt.role)
			if got != tt.want {
				t.Errorf("RoleToLabel() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestApplyK6Defaults(t *testing.T) {
	tests := []struct {
		name  string
		input config.K6Config
		want  config.K6Config
	}{
		{
			name:  "empty config gets all defaults",
			input: config.K6Config{},
			want: config.K6Config{
				PreallocatedVUs:   10,
				MaxVUs:            100,
				Rate:              10,
				Duration:          "30s",
				TargetPath:        "/",
				ConnectionTimeout: "10s",
				RequestTimeout:    "30s",
				GracefulStop:      "30s",
			},
		},
		{
			name: "partial config keeps custom values",
			input: config.K6Config{
				Rate:     50,
				Duration: "5m",
			},
			want: config.K6Config{
				PreallocatedVUs:   10,    // default
				MaxVUs:            100,   // default
				Rate:              50,    // custom
				Duration:          "5m",  // custom
				TargetPath:        "/",   // default
				ConnectionTimeout: "10s", // default
				RequestTimeout:    "30s", // default
				GracefulStop:      "30s", // default
			},
		},
		{
			name: "full custom config unchanged",
			input: config.K6Config{
				PreallocatedVUs:   20,
				MaxVUs:            200,
				Rate:              100,
				Duration:          "10m",
				TargetPath:        "/api",
				ConnectionTimeout: "5s",
				RequestTimeout:    "15s",
				GracefulStop:      "15s",
			},
			want: config.K6Config{
				PreallocatedVUs:   20,
				MaxVUs:            200,
				Rate:              100,
				Duration:          "10m",
				TargetPath:        "/api",
				ConnectionTimeout: "5s",
				RequestTimeout:    "15s",
				GracefulStop:      "15s",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := tt.input
			ApplyK6Defaults(&cfg)

			if cfg.PreallocatedVUs != tt.want.PreallocatedVUs {
				t.Errorf("PreallocatedVUs = %v, want %v", cfg.PreallocatedVUs, tt.want.PreallocatedVUs)
			}
			if cfg.MaxVUs != tt.want.MaxVUs {
				t.Errorf("MaxVUs = %v, want %v", cfg.MaxVUs, tt.want.MaxVUs)
			}
			if cfg.Rate != tt.want.Rate {
				t.Errorf("Rate = %v, want %v", cfg.Rate, tt.want.Rate)
			}
			if cfg.Duration != tt.want.Duration {
				t.Errorf("Duration = %v, want %v", cfg.Duration, tt.want.Duration)
			}
			if cfg.TargetPath != tt.want.TargetPath {
				t.Errorf("TargetPath = %v, want %v", cfg.TargetPath, tt.want.TargetPath)
			}
			if cfg.ConnectionTimeout != tt.want.ConnectionTimeout {
				t.Errorf("ConnectionTimeout = %v, want %v", cfg.ConnectionTimeout, tt.want.ConnectionTimeout)
			}
			if cfg.RequestTimeout != tt.want.RequestTimeout {
				t.Errorf("RequestTimeout = %v, want %v", cfg.RequestTimeout, tt.want.RequestTimeout)
			}
			if cfg.GracefulStop != tt.want.GracefulStop {
				t.Errorf("GracefulStop = %v, want %v", cfg.GracefulStop, tt.want.GracefulStop)
			}
		})
	}
}
