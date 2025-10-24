package ssh

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

// Client wraps an SSH client connection
type Client struct {
	client *ssh.Client
	host   string
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

	// Create SSH config
	config := &ssh.ClientConfig{
		User: user,
		Auth: []ssh.AuthMethod{
			ssh.PublicKeys(signer),
		},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), // In production, verify host keys
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

// Close closes the SSH connection
func (c *Client) Close() error {
	if c.client != nil {
		return c.client.Close()
	}
	return nil
}

// Execute executes a command on the remote server
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
