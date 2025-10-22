package apisix

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/dihmeetree/harbor/internal/config"
	"github.com/dihmeetree/harbor/internal/ssh"
)

// Client wraps the APISIX Admin API client
type Client struct {
	baseURL   string
	apiKey    string
	client    *http.Client
	sshClient *ssh.Client
}

// New creates a new APISIX Admin API client
func New(baseURL, apiKey string) *Client {
	return &Client{
		baseURL: baseURL,
		apiKey:  apiKey,
		client: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// NewWithSSH creates a new APISIX Admin API client that executes via SSH
func NewWithSSH(baseURL, apiKey string, sshClient *ssh.Client) *Client {
	return &Client{
		baseURL:   baseURL,
		apiKey:    apiKey,
		sshClient: sshClient,
	}
}

// CreateUpstream creates an APISIX upstream
func (c *Client) CreateUpstream(upstream config.APISIXUpstream, nodes map[string]int) error {
	payload := map[string]interface{}{
		"name":  upstream.Name,
		"type":  "roundrobin",
		"nodes": nodes,
	}

	if upstream.EnableHealthCheck {
		payload["checks"] = map[string]interface{}{
			"active": map[string]interface{}{
				"type":      "http",
				"http_path": upstream.HealthCheckPath,
				"timeout":   5,
				"healthy": map[string]interface{}{
					"interval":  upstream.HealthyInterval,
					"successes": 1,
				},
				"unhealthy": map[string]interface{}{
					"interval":      upstream.UnhealthyInterval,
					"http_failures": 2,
				},
			},
		}
	}

	if upstream.KeepaliveSize > 0 {
		payload["keepalive_pool"] = map[string]interface{}{
			"size":         upstream.KeepaliveSize,
			"idle_timeout": upstream.KeepaliveIdleTimeout,
			"requests":     upstream.KeepaliveRequests,
		}
	}

	url := fmt.Sprintf("%s/apisix/admin/upstreams/%s", c.baseURL, upstream.ID)
	return c.request("PUT", url, payload, nil)
}

// CreateRoute creates an APISIX route
func (c *Client) CreateRoute(route config.APISIXRoute) error {
	payload := map[string]interface{}{
		"name":        route.Name,
		"methods":     route.Methods,
		"uri":         route.URI,
		"upstream_id": route.UpstreamID,
	}

	// Add host if specified (for host-based routing)
	if route.Host != "" {
		payload["host"] = route.Host
	}

	// Add plugins if specified (don't send empty plugins)
	if route.Plugins != nil && len(route.Plugins) > 0 {
		payload["plugins"] = route.Plugins
	}

	url := fmt.Sprintf("%s/apisix/admin/routes/%s", c.baseURL, route.ID)
	return c.request("PUT", url, payload, nil)
}

// CreateGlobalRule creates an APISIX global rule
func (c *Client) CreateGlobalRule(rule config.APISIXGlobalRule) error {
	payload := map[string]interface{}{
		"plugins": rule.Plugins,
	}

	url := fmt.Sprintf("%s/apisix/admin/global_rules/%s", c.baseURL, rule.ID)
	return c.request("PUT", url, payload, nil)
}

// CreateSSL creates an APISIX SSL certificate
func (c *Client) CreateSSL(sslConfig config.APISIXSSLConfig, cert, key, clientCA string) error {
	payload := map[string]interface{}{
		"cert": cert,
		"key":  key,
		"snis": sslConfig.SNIs,
	}

	if clientCA != "" {
		payload["client"] = map[string]interface{}{
			"ca":    clientCA,
			"depth": sslConfig.ClientCADepth,
		}
	}

	if len(sslConfig.SSLProtocols) > 0 {
		payload["ssl_protocols"] = sslConfig.SSLProtocols
	}

	url := fmt.Sprintf("%s/apisix/admin/ssls/%s", c.baseURL, sslConfig.ID)
	return c.request("PUT", url, payload, nil)
}

// GetUpstream retrieves an upstream
func (c *Client) GetUpstream(id string) (map[string]interface{}, error) {
	url := fmt.Sprintf("%s/apisix/admin/upstreams/%s", c.baseURL, id)
	var result map[string]interface{}
	if err := c.request("GET", url, nil, &result); err != nil {
		return nil, err
	}
	return result, nil
}

// UpdateUpstream updates an upstream
func (c *Client) UpdateUpstream(id string, upstream map[string]interface{}) error {
	url := fmt.Sprintf("%s/apisix/admin/upstreams/%s", c.baseURL, id)
	return c.request("PATCH", url, upstream, nil)
}

// DeleteUpstream deletes an upstream
func (c *Client) DeleteUpstream(id string) error {
	url := fmt.Sprintf("%s/apisix/admin/upstreams/%s", c.baseURL, id)
	return c.request("DELETE", url, nil, nil)
}

// DeleteRoute deletes a route
func (c *Client) DeleteRoute(id string) error {
	url := fmt.Sprintf("%s/apisix/admin/routes/%s", c.baseURL, id)
	return c.request("DELETE", url, nil, nil)
}

// DeleteGlobalRule deletes a global rule
func (c *Client) DeleteGlobalRule(id string) error {
	url := fmt.Sprintf("%s/apisix/admin/global_rules/%s", c.baseURL, id)
	return c.request("DELETE", url, nil, nil)
}

// DeleteSSL deletes an SSL certificate
func (c *Client) DeleteSSL(id string) error {
	url := fmt.Sprintf("%s/apisix/admin/ssls/%s", c.baseURL, id)
	return c.request("DELETE", url, nil, nil)
}

// HealthCheck checks if APISIX Admin API is accessible
func (c *Client) HealthCheck() error {
	url := fmt.Sprintf("%s/apisix/admin/routes", c.baseURL)
	return c.request("GET", url, nil, nil)
}

// request makes an HTTP request to the APISIX Admin API
func (c *Client) request(method, url string, payload interface{}, result interface{}) error {
	// If SSH client is available, execute via SSH
	if c.sshClient != nil {
		return c.requestViaSSH(method, url, payload, result)
	}

	// Otherwise use direct HTTP client
	var body io.Reader
	if payload != nil {
		jsonData, err := json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("failed to marshal payload: %w", err)
		}
		body = bytes.NewBuffer(jsonData)
	}

	req, err := http.NewRequest(method, url, body)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-KEY", c.apiKey)

	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to send request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("API request failed with status %d: %s", resp.StatusCode, string(bodyBytes))
	}

	if result != nil && resp.StatusCode != http.StatusNoContent {
		if err := json.NewDecoder(resp.Body).Decode(result); err != nil {
			return fmt.Errorf("failed to decode response: %w", err)
		}
	}

	return nil
}

// requestViaSSH executes an HTTP request via SSH using curl
func (c *Client) requestViaSSH(method, url string, payload interface{}, result interface{}) error {
	// Build curl command
	var cmd strings.Builder
	cmd.WriteString("curl -s -w '\\n%{http_code}'")

	// Add method
	if method != "GET" {
		cmd.WriteString(fmt.Sprintf(" -X %s", method))
	}

	// Add headers
	cmd.WriteString(" -H 'Content-Type: application/json'")
	cmd.WriteString(fmt.Sprintf(" -H 'X-API-KEY: %s'", c.apiKey))

	// Add payload if present
	if payload != nil {
		jsonData, err := json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("failed to marshal payload: %w", err)
		}
		// Escape single quotes in JSON
		escapedJSON := strings.ReplaceAll(string(jsonData), "'", "'\\''")
		cmd.WriteString(fmt.Sprintf(" -d '%s'", escapedJSON))
	}

	// Add URL
	cmd.WriteString(fmt.Sprintf(" '%s'", url))

	// Execute via SSH
	output, err := c.sshClient.Execute(cmd.String())
	if err != nil {
		return fmt.Errorf("failed to execute curl via SSH: %w", err)
	}

	// Parse output - last line is status code, rest is body
	lines := strings.Split(strings.TrimSpace(output), "\n")
	if len(lines) < 1 {
		return fmt.Errorf("empty response from curl")
	}

	statusCode := lines[len(lines)-1]
	var responseBody string
	if len(lines) > 1 {
		responseBody = strings.Join(lines[:len(lines)-1], "\n")
	}

	// Check status code
	if statusCode < "200" || statusCode >= "300" {
		return fmt.Errorf("API request failed with status %s: %s", statusCode, responseBody)
	}

	// Decode result if needed
	if result != nil && responseBody != "" && statusCode != "204" {
		if err := json.Unmarshal([]byte(responseBody), result); err != nil {
			return fmt.Errorf("failed to decode response: %w", err)
		}
	}

	return nil
}
