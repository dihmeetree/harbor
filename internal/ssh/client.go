package ssh

import (
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// Client wraps an SSH client connection
type Client struct {
	client *ssh.Client
	host   string
}

// getKnownHostsPath returns the path to the known_hosts file, creating it if it doesn't exist
func getKnownHostsPath() (string, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("failed to get home directory: %w", err)
	}

	sshDir := filepath.Join(homeDir, ".harbor", "ssh")
	if err := os.MkdirAll(sshDir, 0700); err != nil {
		return "", fmt.Errorf("failed to create SSH directory: %w", err)
	}

	knownHostsPath := filepath.Join(sshDir, "known_hosts")

	// Create known_hosts file if it doesn't exist
	if _, err := os.Stat(knownHostsPath); os.IsNotExist(err) {
		file, err := os.OpenFile(knownHostsPath, os.O_CREATE|os.O_WRONLY, 0600)
		if err != nil {
			return "", fmt.Errorf("failed to create known_hosts file: %w", err)
		}
		file.Close()
	}

	return knownHostsPath, nil
}

// addHostKeyToKnownHosts adds a host key to the known_hosts file
func addHostKeyToKnownHosts(host string, remote net.Addr, key ssh.PublicKey) error {
	knownHostsPath, err := getKnownHostsPath()
	if err != nil {
		return err
	}

	// Open file in append mode
	file, err := os.OpenFile(knownHostsPath, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return fmt.Errorf("failed to open known_hosts file: %w", err)
	}
	defer file.Close()

	// Write the host key in OpenSSH known_hosts format
	hostPort := knownhosts.Normalize(remote.String())
	line := knownhosts.Line([]string{hostPort}, key)
	if _, err := file.WriteString(line + "\n"); err != nil {
		return fmt.Errorf("failed to write to known_hosts file: %w", err)
	}

	return nil
}

// createHostKeyCallback creates a host key callback that uses known_hosts and prompts for unknown keys
func createHostKeyCallback() (ssh.HostKeyCallback, error) {
	knownHostsPath, err := getKnownHostsPath()
	if err != nil {
		return nil, err
	}

	// Create callback that checks known_hosts
	hostKeyCallback, err := knownhosts.New(knownHostsPath)
	if err != nil {
		return nil, fmt.Errorf("failed to create known_hosts callback: %w", err)
	}

	// Wrap the callback to automatically add unknown hosts (trust on first use)
	return ssh.HostKeyCallback(func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		err := hostKeyCallback(hostname, remote, key)
		if err != nil {
			// Check if this is an "unknown host" error
			if keyErr, ok := err.(*knownhosts.KeyError); ok && len(keyErr.Want) == 0 {
				// Host is unknown, add it to known_hosts (TOFU - Trust On First Use)
				if addErr := addHostKeyToKnownHosts(hostname, remote, key); addErr != nil {
					return fmt.Errorf("failed to add host key to known_hosts: %w", addErr)
				}
				// Accept this connection
				return nil
			}
			// Host key mismatch or other error - reject connection
			return fmt.Errorf("host key verification failed: %w", err)
		}
		return nil
	}), nil
}

// New creates a new SSH client connection
func New(host, user, privateKeyPath string) (*Client, error) {
	// Read private key
	key, err := os.ReadFile(privateKeyPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read private key: %w", err)
	}

	// Parse private key
	signer, err := ssh.ParsePrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("failed to parse private key: %w", err)
	}

	// Create host key callback with known_hosts verification
	hostKeyCallback, err := createHostKeyCallback()
	if err != nil {
		return nil, fmt.Errorf("failed to create host key callback: %w", err)
	}

	// Create SSH config
	config := &ssh.ClientConfig{
		User: user,
		Auth: []ssh.AuthMethod{
			ssh.PublicKeys(signer),
		},
		HostKeyCallback: hostKeyCallback,
		Timeout:         30 * time.Second,
	}

	// Connect to SSH server
	client, err := ssh.Dial("tcp", host+":22", config)
	if err != nil {
		return nil, fmt.Errorf("failed to dial SSH: %w", err)
	}

	return &Client{
		client: client,
		host:   host,
	}, nil
}

// Close closes the SSH connection and releases associated resources.
func (c *Client) Close() error {
	if c.client != nil {
		return c.client.Close()
	}
	return nil
}

// Execute runs a command on the remote server and returns its combined stdout/stderr output.
// If the command fails, the error message includes the output for debugging purposes.
func (c *Client) Execute(command string) (string, error) {
	session, err := c.client.NewSession()
	if err != nil {
		return "", fmt.Errorf("failed to create session: %w", err)
	}
	defer session.Close()

	output, err := session.CombinedOutput(command)
	if err != nil {
		// Include the actual output in the error message for debugging
		if len(output) > 0 {
			return string(output), fmt.Errorf("command failed: %w\nOutput: %s", err, string(output))
		}
		return string(output), fmt.Errorf("command failed: %w", err)
	}

	return string(output), nil
}

// ExecuteWithCallback executes a command and streams output via callback
func (c *Client) ExecuteWithCallback(command string, callback func(line string)) error {
	session, err := c.client.NewSession()
	if err != nil {
		return fmt.Errorf("failed to create session: %w", err)
	}
	defer session.Close()

	stdout, err := session.StdoutPipe()
	if err != nil {
		return fmt.Errorf("failed to get stdout pipe: %w", err)
	}

	stderr, err := session.StderrPipe()
	if err != nil {
		return fmt.Errorf("failed to get stderr pipe: %w", err)
	}

	if err := session.Start(command); err != nil {
		return fmt.Errorf("failed to start command: %w", err)
	}

	// Stream stdout
	go func() {
		buf := make([]byte, 1024)
		for {
			n, err := stdout.Read(buf)
			if n > 0 {
				callback(string(buf[:n]))
			}
			if err != nil {
				break
			}
		}
	}()

	// Stream stderr
	go func() {
		buf := make([]byte, 1024)
		for {
			n, err := stderr.Read(buf)
			if n > 0 {
				callback(string(buf[:n]))
			}
			if err != nil {
				break
			}
		}
	}()

	if err := session.Wait(); err != nil {
		return fmt.Errorf("command failed: %w", err)
	}

	return nil
}

// CopyFile copies a file to the remote server using SFTP
func (c *Client) CopyFile(localPath, remotePath string) error {
	// Open SFTP session
	sftpClient, err := sftp.NewClient(c.client)
	if err != nil {
		return fmt.Errorf("failed to create SFTP client: %w", err)
	}
	defer sftpClient.Close()

	// Open local file
	localFile, err := os.Open(localPath)
	if err != nil {
		return fmt.Errorf("failed to open local file: %w", err)
	}
	defer localFile.Close()

	// Create remote file
	remoteFile, err := sftpClient.Create(remotePath)
	if err != nil {
		return fmt.Errorf("failed to create remote file: %w", err)
	}
	defer remoteFile.Close()

	// Copy file contents
	if _, err := io.Copy(remoteFile, localFile); err != nil {
		return fmt.Errorf("failed to copy file contents: %w", err)
	}

	return nil
}

// WriteFile writes content to a file on the remote server
func (c *Client) WriteFile(remotePath, content string) error {
	session, err := c.client.NewSession()
	if err != nil {
		return fmt.Errorf("failed to create session: %w", err)
	}
	defer session.Close()

	stdin, err := session.StdinPipe()
	if err != nil {
		return fmt.Errorf("failed to get stdin pipe: %w", err)
	}

	if err := session.Start(fmt.Sprintf("cat > %s", remotePath)); err != nil {
		return fmt.Errorf("failed to start cat: %w", err)
	}

	_, _ = io.WriteString(stdin, content)
	stdin.Close()

	if err := session.Wait(); err != nil {
		return fmt.Errorf("write failed: %w", err)
	}

	return nil
}

// FileExists checks if a file exists on the remote server
func (c *Client) FileExists(remotePath string) (bool, error) {
	_, err := c.Execute(fmt.Sprintf("test -f %s", remotePath))
	if err != nil {
		if exitErr, ok := err.(*ssh.ExitError); ok {
			if exitErr.ExitStatus() == 1 {
				return false, nil
			}
		}
		return false, err
	}
	return true, nil
}

// CopyDir copies a directory and its contents to the remote server using SFTP
func (c *Client) CopyDir(localPath, remotePath string) error {
	// Open SFTP session
	sftpClient, err := sftp.NewClient(c.client)
	if err != nil {
		return fmt.Errorf("failed to create SFTP client: %w", err)
	}
	defer sftpClient.Close()

	// Create remote directory
	if err := sftpClient.MkdirAll(remotePath); err != nil {
		return fmt.Errorf("failed to create remote directory: %w", err)
	}

	// Walk local directory
	return filepath.Walk(localPath, func(localFilePath string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		// Calculate relative path
		relPath, err := filepath.Rel(localPath, localFilePath)
		if err != nil {
			return fmt.Errorf("failed to get relative path: %w", err)
		}

		// Skip the root directory itself
		if relPath == "." {
			return nil
		}

		// Calculate remote path
		remoteFilePath := filepath.Join(remotePath, relPath)

		if info.IsDir() {
			// Create remote directory
			if err := sftpClient.MkdirAll(remoteFilePath); err != nil {
				return fmt.Errorf("failed to create remote directory %s: %w", remoteFilePath, err)
			}
		} else {
			// Copy file
			localFile, err := os.Open(localFilePath)
			if err != nil {
				return fmt.Errorf("failed to open local file %s: %w", localFilePath, err)
			}
			defer localFile.Close()

			remoteFile, err := sftpClient.Create(remoteFilePath)
			if err != nil {
				return fmt.Errorf("failed to create remote file %s: %w", remoteFilePath, err)
			}
			defer remoteFile.Close()

			if _, err := io.Copy(remoteFile, localFile); err != nil {
				return fmt.Errorf("failed to copy file %s: %w", localFilePath, err)
			}
		}

		return nil
	})
}

// WaitForConnection waits for SSH to become available
func WaitForConnection(host, user, privateKeyPath string, timeout time.Duration) (*Client, error) {
	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		client, err := New(host, user, privateKeyPath)
		if err == nil {
			return client, nil
		}

		time.Sleep(5 * time.Second)
	}

	return nil, fmt.Errorf("timeout waiting for SSH connection to %s", host)
}
