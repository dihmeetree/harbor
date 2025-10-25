package cli

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/dihmeetree/harbor/internal/config"
)

// CommandContext holds common initialization for commands
type CommandContext struct {
	Config *config.Config
	Token  string
}

// InitCommandContext loads config and validates token
func InitCommandContext(cfgFile string) (*CommandContext, error) {
	// Load config
	cfg, err := config.Load(cfgFile)
	if err != nil {
		return nil, fmt.Errorf("failed to load config: %w", err)
	}

	// Get Hetzner API token
	token := os.Getenv("HETZNER_API_TOKEN")
	if token == "" {
		return nil, fmt.Errorf("HETZNER_API_TOKEN environment variable is required")
	}

	return &CommandContext{
		Config: cfg,
		Token:  token,
	}, nil
}

// ResolveSshKeyPath handles all SSH key path resolution logic
func ResolveSshKeyPath(cfg *config.Config) (string, error) {
	// Check HARBOR_SSH_KEY env var first
	privateKeyPath := os.Getenv("HARBOR_SSH_KEY")
	if privateKeyPath != "" {
		if _, err := os.Stat(privateKeyPath); err != nil {
			return "", fmt.Errorf("SSH key not found at %s: %w", privateKeyPath, err)
		}
		return privateKeyPath, nil
	}

	// Check SSH_PRIVATE_KEY_PATH env var
	privateKeyPath = os.Getenv("SSH_PRIVATE_KEY_PATH")
	if privateKeyPath != "" {
		if _, err := os.Stat(privateKeyPath); err != nil {
			return "", fmt.Errorf("SSH key not found at %s: %w", privateKeyPath, err)
		}
		return privateKeyPath, nil
	}

	// Try default location in project directory
	keyName := fmt.Sprintf("%s-key", cfg.Control.Name)
	privateKeyPath = filepath.Join(".harbor", "ssh", keyName)
	if _, err := os.Stat(privateKeyPath); err == nil {
		return privateKeyPath, nil
	}

	// Try home directory
	homeDir, err := os.UserHomeDir()
	if err == nil {
		privateKeyPath = filepath.Join(homeDir, ".harbor", "ssh", keyName)
		if _, err := os.Stat(privateKeyPath); err == nil {
			return privateKeyPath, nil
		}
	}

	// Not found anywhere
	return "", fmt.Errorf("SSH key not found. Expected at: .harbor/ssh/%s\nSet HARBOR_SSH_KEY environment variable or run 'harbor deploy' first", keyName)
}

// RequireEnvVar validates that an environment variable is set and returns its value
func RequireEnvVar(name string) (string, error) {
	value := os.Getenv(name)
	if value == "" {
		return "", fmt.Errorf("%s environment variable is required", name)
	}
	return value, nil
}
