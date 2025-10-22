package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/hetznercloud/hcloud-go/v2/hcloud"
	"github.com/spf13/cobra"
	"github.com/dihmeetree/harbor/internal/config"
	"github.com/dihmeetree/harbor/internal/database"
	"github.com/dihmeetree/harbor/internal/hetzner"
	"github.com/dihmeetree/harbor/internal/orchestrator"
	"github.com/dihmeetree/harbor/pkg/models"
)

var (
	cfgFile string
	dbPath  string
	rootCmd *cobra.Command
)

func init() {
	// Set default paths
	homeDir, _ := os.UserHomeDir()
	defaultDBPath := filepath.Join(homeDir, ".harbor", "state.db")

	rootCmd = &cobra.Command{
		Use:   "harbor",
		Short: "Harbor - Infrastructure provisioning and management CLI",
		Long: `Harbor is a CLI tool for provisioning and managing APISIX-based
infrastructure on Hetzner Cloud. It automates the deployment of API gateways,
load balancers, and observability stacks.`,
	}

	rootCmd.PersistentFlags().StringVarP(&cfgFile, "config", "c", "harbor.yaml", "config file path")
	rootCmd.PersistentFlags().StringVarP(&dbPath, "db", "d", defaultDBPath, "database file path")

	// Add commands
	rootCmd.AddCommand(initCmd())
	rootCmd.AddCommand(validateCmd())
	rootCmd.AddCommand(deployCmd())
	rootCmd.AddCommand(redeployCmd())
	rootCmd.AddCommand(statusCmd())
	rootCmd.AddCommand(scaleCmd())
	rootCmd.AddCommand(destroyCmd())
	rootCmd.AddCommand(versionCmd())
}

func main() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func initCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "init",
		Short: "Initialize a new Harbor configuration",
		RunE: func(cmd *cobra.Command, args []string) error {
			// Check if config already exists
			if _, err := os.Stat(cfgFile); err == nil {
				return fmt.Errorf("config file already exists: %s", cfgFile)
			}

			// Copy example config
			exampleConfig := "configs/harbor.example.yaml"
			input, err := os.ReadFile(exampleConfig)
			if err != nil {
				return fmt.Errorf("failed to read example config: %w", err)
			}

			if err := os.WriteFile(cfgFile, input, 0644); err != nil {
				return fmt.Errorf("failed to write config: %w", err)
			}

			fmt.Printf("✓ Created configuration file: %s\n", cfgFile)
			fmt.Println("\nNext steps:")
			fmt.Println("1. Edit the config file with your settings")
			fmt.Println("2. Set environment variables:")
			fmt.Println("   export HETZNER_API_TOKEN=\"your-token\"")
			fmt.Println("3. Run: harbor deploy")
			return nil
		},
	}
}

func validateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "validate",
		Short: "Validate the configuration file",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load(cfgFile)
			if err != nil {
				return fmt.Errorf("validation failed: %w", err)
			}

			fmt.Printf("✓ Configuration is valid\n")
			fmt.Printf("  Provider: %s\n", cfg.Provider)
			fmt.Printf("  Control plane: %s (%s)\n", cfg.Server.Name, cfg.Server.Type)
			if cfg.LoadBalancer.Enabled {
				fmt.Printf("  Data planes: %d x %s\n", cfg.LoadBalancer.Replicas, cfg.LoadBalancer.ServerType)
			}
			if cfg.App.Enabled {
				fmt.Printf("  App servers: %d x %s\n", cfg.App.Replicas, cfg.App.ServerType)
			}
			return nil
		},
	}
}

func deployCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "deploy",
		Short: "Deploy infrastructure",
		Long:  "Provisions all infrastructure components defined in the configuration",
		RunE: func(cmd *cobra.Command, args []string) error {
			// Load config
			cfg, err := config.Load(cfgFile)
			if err != nil {
				return fmt.Errorf("failed to load config: %w", err)
			}

			// Get Hetzner API token
			token := os.Getenv("HETZNER_API_TOKEN")
			if token == "" {
				return fmt.Errorf("HETZNER_API_TOKEN environment variable is required")
			}

			// Ensure database directory exists
			dbDir := filepath.Dir(dbPath)
			if err := os.MkdirAll(dbDir, 0755); err != nil {
				return fmt.Errorf("failed to create database directory: %w", err)
			}

			// Open database
			db, err := database.New(dbPath)
			if err != nil {
				return fmt.Errorf("failed to open database: %w", err)
			}
			defer db.Close()

			// Create provisioner
			provisioner := orchestrator.NewProvisioner(cfg, token, db)

			// Provision infrastructure
			ctx := context.Background()
			if err := provisioner.Provision(ctx); err != nil {
				return fmt.Errorf("provisioning failed: %w", err)
			}

			fmt.Println("\n✓ Deployment completed successfully!")
			return nil
		},
	}
}

func redeployCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "redeploy",
		Short: "Redeploy services to existing infrastructure",
		Long:  "Redeploys Docker containers and APISIX configuration to existing servers without recreating infrastructure",
		RunE: func(cmd *cobra.Command, args []string) error {
			// Load config
			cfg, err := config.Load(cfgFile)
			if err != nil {
				return fmt.Errorf("failed to load config: %w", err)
			}

			// Open database
			db, err := database.New(dbPath)
			if err != nil {
				return fmt.Errorf("failed to open database: %w", err)
			}
			defer db.Close()

			// Check if infrastructure exists
			serverRepo := database.NewServerRepository(db)
			servers, err := serverRepo.GetAll()
			if err != nil {
				return fmt.Errorf("failed to get servers: %w", err)
			}

			if len(servers) == 0 {
				return fmt.Errorf("no infrastructure found - run 'harbor deploy' first")
			}

			// Get SSH key path
			privateKeyPath := os.Getenv("HARBOR_SSH_KEY")
			if privateKeyPath == "" {
				// Try default location in project directory
				privateKeyPath = filepath.Join(".harbor", "ssh", cfg.Server.Name+"-key")
				if _, err := os.Stat(privateKeyPath); err != nil {
					return fmt.Errorf("SSH key not found. Expected at: %s\nSet HARBOR_SSH_KEY environment variable or run 'harbor deploy' first", privateKeyPath)
				}
			}

			deployer := orchestrator.NewDeployer(cfg, db, privateKeyPath)

			// Deploy services only
			ctx := context.Background()
			if err := deployer.DeployServicesOnly(ctx); err != nil {
				return fmt.Errorf("redeployment failed: %w", err)
			}

			fmt.Println("\n✓ Services redeployed successfully!")
			return nil
		},
	}
}

func statusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show infrastructure status",
		RunE: func(cmd *cobra.Command, args []string) error {
			// Get Hetzner API token
			token := os.Getenv("HETZNER_API_TOKEN")
			if token == "" {
				return fmt.Errorf("HETZNER_API_TOKEN environment variable is required")
			}

			// Create Hetzner client
			ctx := context.Background()
			hetznerClient := hetzner.New(token)

			// Get all servers with "managed=harbor" label
			allServers, err := hetznerClient.GetServersByLabel(ctx, "managed", "harbor")
			if err != nil {
				return fmt.Errorf("failed to get servers from Hetzner: %w", err)
			}

			if len(allServers) == 0 {
				fmt.Println("[info] No infrastructure deployed")
				return nil
			}

			fmt.Println("Infrastructure Status:")
			fmt.Println()

			// Group by role label
			roleGroups := make(map[string][]*hcloud.Server)
			for _, server := range allServers {
				if role, ok := server.Labels["role"]; ok {
					roleGroups[role] = append(roleGroups[role], server)
				}
			}

			// Print control plane
			if controlPlanes, ok := roleGroups["control"]; ok {
				fmt.Println("Control Plane:")
				for _, s := range controlPlanes {
					privateIP := ""
					if len(s.PrivateNet) > 0 {
						privateIP = s.PrivateNet[0].IP.String()
					}
					autoscaled := s.Labels["autoscale"] == "true"

					fmt.Printf("  - %s\n", s.Name)
					fmt.Printf("    Public IP:  %s\n", s.PublicNet.IPv4.IP.String())
					fmt.Printf("    Private IP: %s\n", privateIP)
					fmt.Printf("    Status:     %s\n", s.Status)
					if autoscaled {
						fmt.Printf("    Autoscaled: yes\n")
					}
				}
				fmt.Println()
			}

			// Print data planes (load balancers)
			if dataPlanes, ok := roleGroups["lb"]; ok {
				fmt.Printf("Load Balancers (%d):\n", len(dataPlanes))
				for _, s := range dataPlanes {
					privateIP := ""
					if len(s.PrivateNet) > 0 {
						privateIP = s.PrivateNet[0].IP.String()
					}
					autoscaled := s.Labels["autoscale"] == "true"

					fmt.Printf("  - %s\n", s.Name)
					fmt.Printf("    Public IP:  %s\n", s.PublicNet.IPv4.IP.String())
					fmt.Printf("    Private IP: %s\n", privateIP)
					fmt.Printf("    Status:     %s\n", s.Status)
					if autoscaled {
						fmt.Printf("    Autoscaled: yes\n")
					}
				}
				fmt.Println()
			}

			// Print app servers
			if appServers, ok := roleGroups["app"]; ok {
				fmt.Printf("App Servers (%d):\n", len(appServers))
				for _, s := range appServers {
					privateIP := ""
					if len(s.PrivateNet) > 0 {
						privateIP = s.PrivateNet[0].IP.String()
					}
					autoscaled := s.Labels["autoscale"] == "true"

					fmt.Printf("  - %s\n", s.Name)
					fmt.Printf("    Public IP:  %s\n", s.PublicNet.IPv4.IP.String())
					fmt.Printf("    Private IP: %s\n", privateIP)
					fmt.Printf("    Status:     %s\n", s.Status)
					if autoscaled {
						fmt.Printf("    Autoscaled: yes\n")
					}
				}
			}

			return nil
		},
	}
}

func destroyCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "destroy",
		Short: "Destroy all infrastructure",
		Long:  "Destroys all provisioned infrastructure (servers, networks, firewalls)",
		RunE: func(cmd *cobra.Command, args []string) error {
			// Require confirmation
			force, _ := cmd.Flags().GetBool("force")
			if !force {
				fmt.Print("This will destroy ALL infrastructure. Are you sure? (yes/no): ")
				var response string
				_, _ = fmt.Scanln(&response)
				if response != "yes" {
					fmt.Println("Aborted")
					return nil
				}
			}

			// Load config
			cfg, err := config.Load(cfgFile)
			if err != nil {
				return fmt.Errorf("failed to load config: %w", err)
			}

			// Get Hetzner API token
			token := os.Getenv("HETZNER_API_TOKEN")
			if token == "" {
				return fmt.Errorf("HETZNER_API_TOKEN environment variable is required")
			}

			// Open database
			db, err := database.New(dbPath)
			if err != nil {
				return fmt.Errorf("failed to open database: %w", err)
			}
			defer db.Close()

			// Create provisioner
			provisioner := orchestrator.NewProvisioner(cfg, token, db)

			// Destroy infrastructure
			ctx := context.Background()
			if err := provisioner.Destroy(ctx); err != nil {
				return fmt.Errorf("destruction failed: %w", err)
			}

			fmt.Println("✓ Infrastructure destroyed successfully")
			return nil
		},
	}

	cmd.Flags().Bool("force", false, "Skip confirmation prompt")
	return cmd
}

func scaleCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "scale",
		Short: "Manually scale load balancers or app servers",
		Long:  "Manually scale the number of load balancer or app servers up or down",
	}

	cmd.AddCommand(scaleLoadBalancerCmd())
	cmd.AddCommand(scaleAppCmd())
	return cmd
}

func scaleLoadBalancerCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "lb [replicas]",
		Short: "Scale load balancer servers",
		Long:  "Scale the number of load balancer (data plane) servers to the specified count",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var replicas int
			if _, err := fmt.Sscanf(args[0], "%d", &replicas); err != nil {
				return fmt.Errorf("invalid replica count: %s", args[0])
			}

			if replicas < 0 {
				return fmt.Errorf("replica count must be non-negative")
			}

			return scaleServers("lb", "load balancer", replicas)
		},
	}
}

func scaleAppCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "app [replicas]",
		Short: "Scale app servers",
		Long:  "Scale the number of app servers to the specified count",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var replicas int
			if _, err := fmt.Sscanf(args[0], "%d", &replicas); err != nil {
				return fmt.Errorf("invalid replica count: %s", args[0])
			}

			if replicas < 0 {
				return fmt.Errorf("replica count must be non-negative")
			}

			return scaleServers("app", "app", replicas)
		},
	}
}

func scaleServers(role, poolName string, targetReplicas int) error {
	// Load config
	cfg, err := config.Load(cfgFile)
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	// Get Hetzner API token
	token := os.Getenv("HETZNER_API_TOKEN")
	if token == "" {
		return fmt.Errorf("HETZNER_API_TOKEN environment variable is required")
	}

	// Open database
	db, err := database.New(dbPath)
	if err != nil {
		return fmt.Errorf("failed to open database: %w", err)
	}
	defer db.Close()

	// Get SSH key path
	sshKeyPath := os.Getenv("SSH_PRIVATE_KEY_PATH")
	if sshKeyPath == "" {
		// Use the actual server name from config for the key
		keyName := fmt.Sprintf("%s-key", cfg.Server.Name)
		// Check current directory first
		sshKeyPath = filepath.Join(".harbor", "ssh", keyName)
		if _, err := os.Stat(sshKeyPath); os.IsNotExist(err) {
			// Fall back to home directory
			homeDir, _ := os.UserHomeDir()
			sshKeyPath = filepath.Join(homeDir, ".harbor", "ssh", keyName)
		}
	}

	// Get control plane IP for SSH connection
	serverRepo := database.NewServerRepository(db)
	controlPlane, err := serverRepo.GetByRole(models.RoleControlPlane)
	if err != nil || len(controlPlane) == 0 {
		return fmt.Errorf("failed to get control plane server: %w", err)
	}

	// Create manual scaler instance with control plane public IP for SSH
	ctx := context.Background()
	scaler, err := orchestrator.NewManualScaler(cfg, token, controlPlane[0].PublicIP, cfg.APISIX.APIKey, sshKeyPath, db)
	if err != nil {
		return fmt.Errorf("failed to create scaler: %w", err)
	}
	defer scaler.Close()

	// Get current server count
	fmt.Printf("[info] Querying Hetzner for current %s servers...\n", poolName)
	currentCount, err := scaler.GetServerCount(ctx, role)
	if err != nil {
		return fmt.Errorf("failed to get current server count: %w", err)
	}

	fmt.Printf("[info] Current %s servers: %d\n", poolName, currentCount)
	fmt.Printf("[info] Target %s servers: %d\n", poolName, targetReplicas)

	if currentCount == targetReplicas {
		fmt.Printf("[info] ✓ Already at target replica count\n")
		return nil
	}

	if targetReplicas > currentCount {
		// Scale up
		toAdd := targetReplicas - currentCount
		fmt.Printf("[info] Scaling up: adding %d %s server(s)\n", toAdd, poolName)
		if err := scaler.ScaleUp(ctx, role, poolName, toAdd); err != nil {
			return fmt.Errorf("failed to scale up: %w", err)
		}
	} else {
		// Scale down
		toRemove := currentCount - targetReplicas
		fmt.Printf("[info] Scaling down: removing %d %s server(s)\n", toRemove, poolName)
		if err := scaler.ScaleDown(ctx, role, poolName, toRemove); err != nil {
			return fmt.Errorf("failed to scale down: %w", err)
		}
	}

	return nil
}

func versionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Show version information",
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Println("Harbor v0.1.0")
			fmt.Println("Infrastructure provisioning and management CLI")
		},
	}
}
