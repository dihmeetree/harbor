package database

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/dihmeetree/harbor/pkg/models"
)

// NetworkRepository handles network database operations
type NetworkRepository struct {
	db *DB
}

// NewNetworkRepository creates a new network repository
func NewNetworkRepository(db *DB) *NetworkRepository {
	return &NetworkRepository{db: db}
}

// Create creates a new network
func (r *NetworkRepository) Create(network *models.Network) error {
	result, err := r.db.conn.Exec(`
		INSERT INTO networks (hetzner_id, name, ip_range, subnet_range)
		VALUES (?, ?, ?, ?)
	`, network.HetznerID, network.Name, network.IPRange, network.SubnetRange)
	if err != nil {
		return fmt.Errorf("failed to create network: %w", err)
	}

	id, err := result.LastInsertId()
	if err != nil {
		return fmt.Errorf("failed to get network ID: %w", err)
	}

	network.ID = id
	return nil
}

// GetByHetznerID retrieves a network by its Hetzner ID
func (r *NetworkRepository) GetByHetznerID(hetznerID int64) (*models.Network, error) {
	var network models.Network
	err := r.db.conn.QueryRow(`
		SELECT id, hetzner_id, name, ip_range, subnet_range, created_at
		FROM networks
		WHERE hetzner_id = ?
	`, hetznerID).Scan(&network.ID, &network.HetznerID, &network.Name,
		&network.IPRange, &network.SubnetRange, &network.CreatedAt)

	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get network: %w", err)
	}

	return &network, nil
}

// GetAll retrieves all networks
func (r *NetworkRepository) GetAll() ([]*models.Network, error) {
	rows, err := r.db.conn.Query(`
		SELECT id, hetzner_id, name, ip_range, subnet_range, created_at
		FROM networks
	`)
	if err != nil {
		return nil, fmt.Errorf("failed to query networks: %w", err)
	}
	defer rows.Close()

	var networks []*models.Network
	for rows.Next() {
		var network models.Network
		if err := rows.Scan(&network.ID, &network.HetznerID, &network.Name,
			&network.IPRange, &network.SubnetRange, &network.CreatedAt); err != nil {
			return nil, fmt.Errorf("failed to scan network: %w", err)
		}
		networks = append(networks, &network)
	}

	return networks, nil
}

// Delete deletes a network by its Hetzner ID
func (r *NetworkRepository) Delete(hetznerID int64) error {
	_, err := r.db.conn.Exec("DELETE FROM networks WHERE hetzner_id = ?", hetznerID)
	if err != nil {
		return fmt.Errorf("failed to delete network: %w", err)
	}
	return nil
}

// ServerRepository handles server database operations
type ServerRepository struct {
	db *DB
}

// NewServerRepository creates a new server repository
func NewServerRepository(db *DB) *ServerRepository {
	return &ServerRepository{db: db}
}

// Create creates a new server
func (r *ServerRepository) Create(server *models.Server) error {
	result, err := r.db.conn.Exec(`
		INSERT INTO servers (hetzner_id, name, type, role, public_ip, private_ip, location, image, status, network_id)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, server.HetznerID, server.Name, server.Type, server.Role, server.PublicIP,
		server.PrivateIP, server.Location, server.Image, server.Status, server.NetworkID)
	if err != nil {
		return fmt.Errorf("failed to create server: %w", err)
	}

	id, err := result.LastInsertId()
	if err != nil {
		return fmt.Errorf("failed to get server ID: %w", err)
	}

	server.ID = id
	return nil
}

// Update updates a server
func (r *ServerRepository) Update(server *models.Server) error {
	_, err := r.db.conn.Exec(`
		UPDATE servers
		SET public_ip = ?, private_ip = ?, status = ?
		WHERE id = ?
	`, server.PublicIP, server.PrivateIP, server.Status, server.ID)
	if err != nil {
		return fmt.Errorf("failed to update server: %w", err)
	}
	return nil
}

// UpdateStatus updates only the status of a server
func (r *ServerRepository) UpdateStatus(id int64, status string) error {
	_, err := r.db.conn.Exec(`
		UPDATE servers
		SET status = ?
		WHERE id = ?
	`, status, id)
	if err != nil {
		return fmt.Errorf("failed to update server status: %w", err)
	}
	return nil
}

// GetByRole retrieves all servers with a specific role
func (r *ServerRepository) GetByRole(role models.ServerRole) ([]*models.Server, error) {
	rows, err := r.db.conn.Query(`
		SELECT id, hetzner_id, name, type, role, public_ip, private_ip, location, image, status, network_id, created_at
		FROM servers
		WHERE role = ?
		ORDER BY created_at ASC
	`, role)
	if err != nil {
		return nil, fmt.Errorf("failed to query servers: %w", err)
	}
	defer rows.Close()

	var servers []*models.Server
	for rows.Next() {
		var server models.Server
		err := rows.Scan(&server.ID, &server.HetznerID, &server.Name, &server.Type,
			&server.Role, &server.PublicIP, &server.PrivateIP, &server.Location,
			&server.Image, &server.Status, &server.NetworkID, &server.CreatedAt)
		if err != nil {
			return nil, fmt.Errorf("failed to scan server: %w", err)
		}
		servers = append(servers, &server)
	}

	return servers, nil
}

// GetAll retrieves all servers
func (r *ServerRepository) GetAll() ([]*models.Server, error) {
	rows, err := r.db.conn.Query(`
		SELECT id, hetzner_id, name, type, role, public_ip, private_ip, location, image, status, network_id, created_at
		FROM servers
		ORDER BY created_at ASC
	`)
	if err != nil {
		return nil, fmt.Errorf("failed to query servers: %w", err)
	}
	defer rows.Close()

	var servers []*models.Server
	for rows.Next() {
		var server models.Server
		err := rows.Scan(&server.ID, &server.HetznerID, &server.Name, &server.Type,
			&server.Role, &server.PublicIP, &server.PrivateIP, &server.Location,
			&server.Image, &server.Status, &server.NetworkID, &server.CreatedAt)
		if err != nil {
			return nil, fmt.Errorf("failed to scan server: %w", err)
		}
		servers = append(servers, &server)
	}

	return servers, nil
}

// Delete deletes a server by its Hetzner ID
func (r *ServerRepository) Delete(hetznerID int64) error {
	_, err := r.db.conn.Exec("DELETE FROM servers WHERE hetzner_id = ?", hetznerID)
	if err != nil {
		return fmt.Errorf("failed to delete server: %w", err)
	}
	return nil
}

// FirewallRepository handles firewall database operations
type FirewallRepository struct {
	db *DB
}

// NewFirewallRepository creates a new firewall repository
func NewFirewallRepository(db *DB) *FirewallRepository {
	return &FirewallRepository{db: db}
}

// Create creates a new firewall with rules
func (r *FirewallRepository) Create(firewall *models.Firewall) error {
	tx, err := r.db.conn.Begin()
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Insert firewall
	result, err := tx.Exec(`
		INSERT INTO firewalls (hetzner_id, name)
		VALUES (?, ?)
	`, firewall.HetznerID, firewall.Name)
	if err != nil {
		return fmt.Errorf("failed to create firewall: %w", err)
	}

	id, err := result.LastInsertId()
	if err != nil {
		return fmt.Errorf("failed to get firewall ID: %w", err)
	}

	firewall.ID = id

	// Insert rules
	for i := range firewall.Rules {
		rule := &firewall.Rules[i]
		sourceIPsJSON, err := json.Marshal(rule.SourceIPs)
		if err != nil {
			return fmt.Errorf("failed to marshal source IPs: %w", err)
		}

		result, err := tx.Exec(`
			INSERT INTO firewall_rules (firewall_id, direction, port, protocol, source_ips, description)
			VALUES (?, ?, ?, ?, ?, ?)
		`, firewall.ID, rule.Direction, rule.Port, rule.Protocol, sourceIPsJSON, rule.Description)
		if err != nil {
			return fmt.Errorf("failed to create firewall rule: %w", err)
		}

		ruleID, err := result.LastInsertId()
		if err != nil {
			return fmt.Errorf("failed to get rule ID: %w", err)
		}
		rule.ID = ruleID
		rule.FirewallID = firewall.ID
	}

	return tx.Commit()
}

// GetAll retrieves all firewalls
func (r *FirewallRepository) GetAll() ([]*models.Firewall, error) {
	rows, err := r.db.conn.Query(`
		SELECT id, hetzner_id, name, created_at
		FROM firewalls
	`)
	if err != nil {
		return nil, fmt.Errorf("failed to query firewalls: %w", err)
	}
	defer rows.Close()

	var firewalls []*models.Firewall
	for rows.Next() {
		var firewall models.Firewall
		if err := rows.Scan(&firewall.ID, &firewall.HetznerID, &firewall.Name,
			&firewall.CreatedAt); err != nil {
			return nil, fmt.Errorf("failed to scan firewall: %w", err)
		}
		// Note: Rules are stored in separate firewall_rules table but not needed for deletion
		firewalls = append(firewalls, &firewall)
	}

	return firewalls, nil
}

// Delete deletes a firewall by its Hetzner ID
func (r *FirewallRepository) Delete(hetznerID int64) error {
	_, err := r.db.conn.Exec("DELETE FROM firewalls WHERE hetzner_id = ?", hetznerID)
	if err != nil {
		return fmt.Errorf("failed to delete firewall: %w", err)
	}
	return nil
}

// DeploymentRepository handles deployment database operations
type DeploymentRepository struct {
	db *DB
}

// NewDeploymentRepository creates a new deployment repository
func NewDeploymentRepository(db *DB) *DeploymentRepository {
	return &DeploymentRepository{db: db}
}

// Create creates a new deployment
func (r *DeploymentRepository) Create(deployment *models.Deployment) error {
	result, err := r.db.conn.Exec(`
		INSERT INTO deployments (config_hash, status)
		VALUES (?, ?)
	`, deployment.ConfigHash, deployment.Status)
	if err != nil {
		return fmt.Errorf("failed to create deployment: %w", err)
	}

	id, err := result.LastInsertId()
	if err != nil {
		return fmt.Errorf("failed to get deployment ID: %w", err)
	}

	deployment.ID = id
	return nil
}

// UpdateStatus updates a deployment status
func (r *DeploymentRepository) UpdateStatus(id int64, status string) error {
	completedAt := sql.NullTime{}
	if status == "completed" || status == "failed" {
		completedAt = sql.NullTime{Time: time.Now(), Valid: true}
	}

	_, err := r.db.conn.Exec(`
		UPDATE deployments
		SET status = ?, completed_at = ?
		WHERE id = ?
	`, status, completedAt, id)
	if err != nil {
		return fmt.Errorf("failed to update deployment status: %w", err)
	}
	return nil
}

// AddLog adds a log entry to a deployment
func (r *DeploymentRepository) AddLog(deploymentID int64, level, message string) error {
	_, err := r.db.conn.Exec(`
		INSERT INTO deployment_logs (deployment_id, level, message)
		VALUES (?, ?, ?)
	`, deploymentID, level, message)
	if err != nil {
		return fmt.Errorf("failed to add deployment log: %w", err)
	}
	return nil
}

// GetLogs retrieves all logs for a deployment
func (r *DeploymentRepository) GetLogs(deploymentID int64) ([]*models.DeploymentLog, error) {
	rows, err := r.db.conn.Query(`
		SELECT id, deployment_id, timestamp, level, message
		FROM deployment_logs
		WHERE deployment_id = ?
		ORDER BY timestamp ASC
	`, deploymentID)
	if err != nil {
		return nil, fmt.Errorf("failed to query logs: %w", err)
	}
	defer rows.Close()

	var logs []*models.DeploymentLog
	for rows.Next() {
		var log models.DeploymentLog
		err := rows.Scan(&log.ID, &log.DeploymentID, &log.Timestamp, &log.Level, &log.Message)
		if err != nil {
			return nil, fmt.Errorf("failed to scan log: %w", err)
		}
		logs = append(logs, &log)
	}

	return logs, nil
}

// GetLatest retrieves the latest deployment
func (r *DeploymentRepository) GetLatest() (*models.Deployment, error) {
	var deployment models.Deployment
	var completedAt sql.NullTime

	err := r.db.conn.QueryRow(`
		SELECT id, config_hash, status, started_at, completed_at
		FROM deployments
		ORDER BY started_at DESC
		LIMIT 1
	`).Scan(&deployment.ID, &deployment.ConfigHash, &deployment.Status,
		&deployment.StartedAt, &completedAt)

	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get latest deployment: %w", err)
	}

	if completedAt.Valid {
		deployment.CompletedAt = &completedAt.Time
	}

	return &deployment, nil
}

// SSHKeyRepository handles SSH key database operations
type SSHKeyRepository struct {
	db *DB
}

// NewSSHKeyRepository creates a new SSH key repository
func NewSSHKeyRepository(db *DB) *SSHKeyRepository {
	return &SSHKeyRepository{db: db}
}

// Create creates a new SSH key
func (r *SSHKeyRepository) Create(key *models.SSHKey) error {
	result, err := r.db.conn.Exec(`
		INSERT INTO ssh_keys (hetzner_id, name, public_key)
		VALUES (?, ?, ?)
	`, key.HetznerID, key.Name, key.PublicKey)
	if err != nil {
		return fmt.Errorf("failed to create SSH key: %w", err)
	}

	id, err := result.LastInsertId()
	if err != nil {
		return fmt.Errorf("failed to get SSH key ID: %w", err)
	}

	key.ID = id
	return nil
}

// GetAll retrieves all SSH keys
func (r *SSHKeyRepository) GetAll() ([]*models.SSHKey, error) {
	rows, err := r.db.conn.Query(`
		SELECT id, hetzner_id, name, public_key, created_at
		FROM ssh_keys
	`)
	if err != nil {
		return nil, fmt.Errorf("failed to query SSH keys: %w", err)
	}
	defer rows.Close()

	var keys []*models.SSHKey
	for rows.Next() {
		var key models.SSHKey
		if err := rows.Scan(&key.ID, &key.HetznerID, &key.Name,
			&key.PublicKey, &key.CreatedAt); err != nil {
			return nil, fmt.Errorf("failed to scan SSH key: %w", err)
		}
		keys = append(keys, &key)
	}

	return keys, nil
}

// Delete deletes an SSH key by its Hetzner ID
func (r *SSHKeyRepository) Delete(hetznerID int64) error {
	_, err := r.db.conn.Exec("DELETE FROM ssh_keys WHERE hetzner_id = ?", hetznerID)
	if err != nil {
		return fmt.Errorf("failed to delete SSH key: %w", err)
	}
	return nil
}
