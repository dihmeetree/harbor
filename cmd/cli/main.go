package main

import (
	"context"
	"fmt"
	"os"

	"github.com/dihmeetree/harbor/internal/cli"
	"github.com/dihmeetree/harbor/internal/config"
	"github.com/dihmeetree/harbor/internal/hetzner"
	"github.com/dihmeetree/harbor/internal/orchestrator"
	"github.com/hetznercloud/hcloud-go/v2/hcloud"
	"github.com/spf13/cobra"
)

var (
	cfgFile     string
	composeFile string
	rootCmd     *cobra.Command
)

func init() {
	rootCmd = &cobra.Command{
		Use:   "harbor",
		Short: "Harbor - Infrastructure provisioning and management CLI",
		Long: `Harbor is a CLI tool for provisioning and managing APISIX-based
infrastructure on Hetzner Cloud. It automates the deployment of API gateways,
load balancers, and observability stacks.`,
	}

	rootCmd.PersistentFlags().StringVarP(&cfgFile, "config", "c", "harbor.yaml", "config file path")

	// Add commands
	rootCmd.AddCommand(initCmd())
	rootCmd.AddCommand(validateCmd())
	rootCmd.AddCommand(deployCmd())
	rootCmd.AddCommand(redeployCmd())
	rootCmd.AddCommand(restartCmd())
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
			fmt.Printf("  Control plane: %s (%s)\n", cfg.Control.Name, cfg.Control.Type)
			fmt.Printf("  Data planes: %d x %s\n", cfg.LoadBalancer.Replicas, cfg.LoadBalancer.ServerType)
			fmt.Printf("  App servers: %d x %s\n", cfg.App.Replicas, cfg.App.ServerType)
			return nil
		},
	}
}

func deployCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "deploy",
		Short: "Deploy infrastructure",
		Long:  "Provisions all infrastructure components defined in the configuration",
		RunE: func(cmd *cobra.Command, args []string) error {
			// Initialize command context
			cmdCtx, err := cli.InitCommandContext(cfgFile)
			if err != nil {
				return err
			}

			// Override compose file if flag is set
			if composeFile != "" {
				cmdCtx.Config.App.ComposeFile = composeFile
			}

			// Create provisioner
			provisioner := orchestrator.NewProvisioner(cmdCtx.Config, cmdCtx.Token)

			// Provision infrastructure
			ctx := context.Background()
			if err := provisioner.Provision(ctx); err != nil {
				return fmt.Errorf("provisioning failed: %w", err)
			}

			fmt.Println("[info] ✓ Deployment completed successfully!")
			return nil
		},
	}
	cmd.Flags().StringVar(&composeFile, "compose-file", "", "Path to docker-compose.yml for app servers (overrides config)")
	return cmd
}

func redeployCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "redeploy [service]",
		Short: "Redeploy app services using blue-green strategy",
		Long: `Redeploy services from docker-compose.yml to app servers using zero-downtime blue-green deployment.

Examples:
  harbor redeploy           # Redeploy all services
  harbor redeploy nginx     # Redeploy only the nginx service
  harbor redeploy api       # Redeploy only the api service

This command performs a rolling blue-green deployment across all app servers, ensuring zero downtime.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// Determine which service to redeploy
			var serviceName string
			if len(args) > 0 {
				serviceName = args[0]
			}

			// Require confirmation unless --yes flag is set
			yes, _ := cmd.Flags().GetBool("yes")
			if !yes {
				if serviceName != "" {
					fmt.Printf("This will redeploy the '%s' service using blue-green strategy.\n", serviceName)
					fmt.Println("App servers will be updated one at a time with zero downtime.")
				} else {
					fmt.Println("This will redeploy ALL app services using blue-green strategy.")
					fmt.Println("App servers will be updated one at a time with zero downtime.")
				}
				fmt.Print("Continue? (yes/no): ")
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

			// Override compose file if flag is set
			if composeFile != "" {
				cfg.App.ComposeFile = composeFile
			}

			// Get Hetzner API token
			token, err := cli.RequireEnvVar("HETZNER_API_TOKEN")
			if err != nil {
				return err
			}

			// Check if infrastructure exists by querying Hetzner
			ctx := context.Background()
			hetznerClient := hetzner.New(token)
			servers, err := hetznerClient.GetServersByLabel(ctx, "managed", "harbor")
			if err != nil {
				return fmt.Errorf("failed to query servers from Hetzner: %w", err)
			}

			if len(servers) == 0 {
				return fmt.Errorf("no infrastructure found - run 'harbor deploy' first")
			}

			// Get SSH key path
			privateKeyPath, err := cli.ResolveSshKeyPath(cfg)
			if err != nil {
				return err
			}

			deployer := orchestrator.NewDeployer(cfg, token, privateKeyPath)

			// Redeploy app servers with optional service filter
			if err := deployer.RedeployAppServers(ctx, serviceName); err != nil {
				return fmt.Errorf("redeployment failed: %w", err)
			}

			if serviceName != "" {
				fmt.Printf("[info] ✓ Service '%s' redeployed successfully!\n", serviceName)
			} else {
				fmt.Println("[info] ✓ All services redeployed successfully!")
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&composeFile, "compose-file", "", "Path to docker-compose.yml for app servers (overrides config)")
	cmd.Flags().BoolP("yes", "y", false, "Skip confirmation prompt")
	return cmd
}

// printServerGroup prints server information for a group of servers
func printServerGroup(servers []*hcloud.Server) {
	for _, s := range servers {
		privateIP := orchestrator.ExtractPrivateIP(s)
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

func statusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show infrastructure status",
		RunE: func(cmd *cobra.Command, args []string) error {
			// Get Hetzner API token
			token, err := cli.RequireEnvVar("HETZNER_API_TOKEN")
			if err != nil {
				return err
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
				printServerGroup(controlPlanes)
				fmt.Println()
			}

			// Print data planes (load balancers)
			if dataPlanes, ok := roleGroups["lb"]; ok {
				fmt.Printf("Load Balancers (%d):\n", len(dataPlanes))
				printServerGroup(dataPlanes)
				fmt.Println()
			}

			// Print app servers
			if appServers, ok := roleGroups["app"]; ok {
				fmt.Printf("App Servers (%d):\n", len(appServers))
				printServerGroup(appServers)
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

			// Initialize command context
			cmdCtx, err := cli.InitCommandContext(cfgFile)
			if err != nil {
				return err
			}

			// Create provisioner
			provisioner := orchestrator.NewProvisioner(cmdCtx.Config, cmdCtx.Token)

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
	token, err := cli.RequireEnvVar("HETZNER_API_TOKEN")
	if err != nil {
		return err
	}

	// Get SSH key path
	sshKeyPath, err := cli.ResolveSshKeyPath(cfg)
	if err != nil {
		return err
	}

	// Query Hetzner for control plane server
	ctx := context.Background()
	hetznerClient := hetzner.New(token)
	controlPlanes, err := hetznerClient.GetServersByLabel(ctx, "role", "control")
	if err != nil {
		return fmt.Errorf("failed to get control plane server from Hetzner: %w", err)
	}
	if len(controlPlanes) == 0 {
		return fmt.Errorf("no control plane server found - run 'harbor deploy' first")
	}

	controlPlaneIP := controlPlanes[0].PublicNet.IPv4.IP.String()

	// Create manual scaler instance with control plane public IP for SSH
	scaler, err := orchestrator.NewManualScaler(cfg, token, controlPlaneIP, cfg.APISIX.APIKey, sshKeyPath)
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

func restartCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "restart [service]",
		Short: "Restart a service on the control plane",
		Long: `Restart a service container on the control plane.

Available services:
  k6       - Restart k6 load testing with latest configuration
  grafana  - Restart Grafana monitoring dashboard

Examples:
  harbor restart k6
  harbor restart grafana`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			service := args[0]
			ctx := context.Background()

			// Validate service name
			validServices := map[string]bool{"k6": true, "grafana": true}
			if !validServices[service] {
				return fmt.Errorf("invalid service '%s'. Valid services: k6, grafana", service)
			}

			// Load config
			cfg, err := config.Load(cfgFile)
			if err != nil {
				return fmt.Errorf("failed to load config: %w", err)
			}

			// Service-specific validation
			if service == "k6" && !cfg.K6.Enabled {
				return fmt.Errorf("k6 is not enabled in configuration (k6.enabled: false)")
			}

			// Get API token
			token, err := cli.RequireEnvVar("HETZNER_API_TOKEN")
			if err != nil {
				return err
			}

			// Query Hetzner for control plane server
			hetznerClient := hetzner.New(token)
			controlPlanes, err := hetznerClient.GetServersByLabel(ctx, "role", "control")
			if err != nil {
				return fmt.Errorf("failed to get control plane server from Hetzner: %w", err)
			}

			if len(controlPlanes) == 0 {
				return fmt.Errorf("no control plane server found - please run 'harbor deploy' first")
			}

			controlPlane := controlPlanes[0]

			// Get SSH key path
			privateKeyPath, err := cli.ResolveSshKeyPath(cfg)
			if err != nil {
				return err
			}

			// Initialize deployer
			deployer := orchestrator.NewDeployer(cfg, token, privateKeyPath)

			fmt.Printf("[info] Restarting %s...\n", service)
			fmt.Printf("[info] Control plane: %s (%s)\n", controlPlane.Name, controlPlane.PublicNet.IPv4.IP.String())

			// Restart the appropriate service
			switch service {
			case "k6":
				if err := deployer.RestartK6(ctx); err != nil {
					return fmt.Errorf("failed to restart k6: %w", err)
				}
				fmt.Println("[info] ✓ k6 successfully restarted with latest configuration")

			case "grafana":
				if err := deployer.RestartGrafana(ctx); err != nil {
					return fmt.Errorf("failed to restart Grafana: %w", err)
				}
				fmt.Println("[info] ✓ Grafana successfully restarted")
			}

			return nil
		},
	}
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
