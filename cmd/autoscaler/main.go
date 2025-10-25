package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/dihmeetree/harbor/internal/autoscaler"
	"github.com/dihmeetree/harbor/internal/config"
)

func main() {
	// Load config
	configPath := os.Getenv("CONFIG_PATH")
	if configPath == "" {
		configPath = "/etc/harbor/config.yaml"
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load config: %v\n", err)
		os.Exit(1)
	}

	// Check if autoscaler is enabled
	if !cfg.Autoscaler.Enabled {
		fmt.Println("Autoscaler is disabled in config")
		os.Exit(0)
	}

	// Get Prometheus URL
	prometheusURL := os.Getenv("PROMETHEUS_URL")
	if prometheusURL == "" {
		prometheusURL = fmt.Sprintf("http://localhost:%d", cfg.Monitoring.PrometheusPort)
	}

	// Get APISIX URL
	apisixURL := os.Getenv("APISIX_URL")
	if apisixURL == "" {
		apisixURL = fmt.Sprintf("http://localhost:%d", cfg.APISIX.AdminPort)
	}

	// Get APISIX API key
	apisixKey := cfg.APISIX.APIKey

	// Get Hetzner token
	hetznerToken := os.Getenv("HETZNER_API_TOKEN")
	if hetznerToken == "" {
		fmt.Fprintf(os.Stderr, "HETZNER_API_TOKEN environment variable is required\n")
		os.Exit(1)
	}

	// Get SSH key path
	sshKeyPath := os.Getenv("SSH_KEY_PATH")
	if sshKeyPath == "" {
		sshKeyPath = "/root/.ssh/id_rsa" // Default path in container
	}

	// Create autoscaler
	as, err := autoscaler.NewAutoscaler(cfg, prometheusURL, hetznerToken, apisixURL, apisixKey, sshKeyPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to create autoscaler: %v\n", err)
		os.Exit(1)
	}

	// Setup signal handling
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	go func() {
		<-sigChan
		fmt.Println("\nShutting down autoscaler...")
		cancel()
		as.Stop()
	}()

	// Start autoscaler
	fmt.Println("Starting Harbor autoscaler...")
	if err := as.Start(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "Autoscaler error: %v\n", err)
		os.Exit(1)
	}
}
