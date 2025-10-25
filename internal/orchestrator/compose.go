package orchestrator

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// ComposeFile represents a simplified docker-compose.yml structure for parsing volumes
type ComposeFile struct {
	Version  string                    `yaml:"version"`
	Services map[string]ComposeService `yaml:"services"`
}

// ComposeService represents a service in docker-compose.yml
type ComposeService struct {
	Image       string   `yaml:"image"`
	Volumes     []string `yaml:"volumes"`
	Environment []string `yaml:"environment"`
}

// ParseComposeFile parses a docker-compose.yml file and returns the structure
func ParseComposeFile(path string) (*ComposeFile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read compose file: %w", err)
	}

	var compose ComposeFile
	if err := yaml.Unmarshal(data, &compose); err != nil {
		return nil, fmt.Errorf("failed to parse compose file: %w", err)
	}

	return &compose, nil
}

// ExtractLocalVolumePaths extracts local file/directory paths from volume mounts
// Returns a map of absolute local paths to their relative paths (to preserve structure on remote)
func ExtractLocalVolumePaths(compose *ComposeFile, composeDir string) (map[string]string, error) {
	volumeMap := make(map[string]string)

	for _, service := range compose.Services {
		for _, volume := range service.Volumes {
			// Parse volume mount (format: "local:remote[:options]")
			parts := strings.Split(volume, ":")
			if len(parts) < 2 {
				continue // Skip named volumes or invalid entries
			}

			localPath := parts[0]

			// Skip if it doesn't look like a local file path (e.g., named volumes)
			if !strings.HasPrefix(localPath, ".") && !strings.HasPrefix(localPath, "/") {
				continue
			}

			// Only process relative paths (absolute host paths are not portable)
			if strings.HasPrefix(localPath, ".") {
				// Convert to absolute path for reading
				absPath := filepath.Join(composeDir, localPath)
				// Store with the original relative path as the target
				volumeMap[absPath] = localPath
			}
		}
	}

	return volumeMap, nil
}

// CopyComposeFilesAndVolumes copies the docker-compose.yml and all local volume files to the remote server
// serverName is used to replace SERVER_ID_PLACEHOLDER in text files
func CopyComposeFilesAndVolumes(sshClient SSHClient, composeFilePath, remoteBaseDir, serverName string) error {
	// Get the directory containing the compose file
	composeDir := filepath.Dir(composeFilePath)
	absComposeDir, err := filepath.Abs(composeDir)
	if err != nil {
		return fmt.Errorf("failed to get absolute compose directory: %w", err)
	}

	// Parse the compose file
	compose, err := ParseComposeFile(composeFilePath)
	if err != nil {
		return fmt.Errorf("failed to parse compose file: %w", err)
	}

	// Extract volume paths
	volumeMap, err := ExtractLocalVolumePaths(compose, absComposeDir)
	if err != nil {
		return fmt.Errorf("failed to extract volume paths: %w", err)
	}

	// Create remote directory
	if _, err := sshClient.Execute(fmt.Sprintf("mkdir -p %s", remoteBaseDir)); err != nil {
		return fmt.Errorf("failed to create remote directory: %w", err)
	}

	// Read and process docker-compose.yml (replace SERVER_ID_PLACEHOLDER)
	composeData, err := os.ReadFile(composeFilePath)
	if err != nil {
		return fmt.Errorf("failed to read compose file: %w", err)
	}

	// Replace SERVER_ID_PLACEHOLDER with actual server name
	processedCompose := strings.ReplaceAll(string(composeData), "SERVER_ID_PLACEHOLDER", serverName)

	// Write processed docker-compose.yml to remote server
	remoteComposePath := filepath.Join(remoteBaseDir, "docker-compose.yml")
	if err := sshClient.WriteFile(remoteComposePath, processedCompose); err != nil {
		return fmt.Errorf("failed to write compose file: %w", err)
	}

	// Copy each volume file/directory
	for localPath, relativeTargetPath := range volumeMap {
		// Calculate full remote path (relative to remoteBaseDir)
		fullRemotePath := filepath.Join(remoteBaseDir, relativeTargetPath)

		// Check if it's a file or directory
		info, err := os.Stat(localPath)
		if err != nil {
			return fmt.Errorf("failed to stat local path %s: %w", localPath, err)
		}

		if info.IsDir() {
			// For directories, we need to process each file individually to replace placeholders
			err := filepath.Walk(localPath, func(filePath string, fileInfo os.FileInfo, err error) error {
				if err != nil {
					return err
				}

				// Calculate relative path within the directory
				relPath, err := filepath.Rel(localPath, filePath)
				if err != nil {
					return fmt.Errorf("failed to get relative path: %w", err)
				}

				// Calculate remote file path
				remoteFilePath := filepath.Join(fullRemotePath, relPath)

				if fileInfo.IsDir() {
					// Create remote directory
					if _, err := sshClient.Execute(fmt.Sprintf("mkdir -p %s", remoteFilePath)); err != nil {
						return fmt.Errorf("failed to create remote directory %s: %w", remoteFilePath, err)
					}
				} else {
					// Read file and replace placeholder
					fileData, err := os.ReadFile(filePath)
					if err != nil {
						return fmt.Errorf("failed to read file %s: %w", filePath, err)
					}

					// Replace SERVER_ID_PLACEHOLDER
					processedData := strings.ReplaceAll(string(fileData), "SERVER_ID_PLACEHOLDER", serverName)

					// Write to remote
					if err := sshClient.WriteFile(remoteFilePath, processedData); err != nil {
						return fmt.Errorf("failed to write file %s: %w", remoteFilePath, err)
					}
				}

				return nil
			})

			if err != nil {
				return fmt.Errorf("failed to process directory %s: %w", localPath, err)
			}
		} else {
			// Ensure remote directory exists
			remoteDir := filepath.Dir(fullRemotePath)
			if _, err := sshClient.Execute(fmt.Sprintf("mkdir -p %s", remoteDir)); err != nil {
				return fmt.Errorf("failed to create remote directory %s: %w", remoteDir, err)
			}

			// Read file and replace placeholder
			fileData, err := os.ReadFile(localPath)
			if err != nil {
				return fmt.Errorf("failed to read file %s: %w", localPath, err)
			}

			// Replace SERVER_ID_PLACEHOLDER
			processedData := strings.ReplaceAll(string(fileData), "SERVER_ID_PLACEHOLDER", serverName)

			// Write to remote
			if err := sshClient.WriteFile(fullRemotePath, processedData); err != nil {
				return fmt.Errorf("failed to write file %s: %w", fullRemotePath, err)
			}
		}
	}

	return nil
}

// SSHClient interface for testability
type SSHClient interface {
	Execute(command string) (string, error)
	CopyFile(localPath, remotePath string) error
	CopyDir(localPath, remotePath string) error
	WriteFile(remotePath, content string) error
}
