-- Networks table
CREATE TABLE networks (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  hetzner_id INTEGER UNIQUE NOT NULL,
  name TEXT NOT NULL,
  ip_range TEXT NOT NULL,
  subnet_range TEXT NOT NULL,
  created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

-- Servers table
CREATE TABLE servers (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  hetzner_id INTEGER UNIQUE NOT NULL,
  name TEXT NOT NULL,
  type TEXT NOT NULL,
  role TEXT NOT NULL CHECK(role IN ('control_plane', 'data_plane', 'app')),
  public_ip TEXT,
  private_ip TEXT,
  location TEXT NOT NULL,
  image TEXT NOT NULL,
  status TEXT NOT NULL DEFAULT 'creating',
  network_id INTEGER NOT NULL,
  created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
  FOREIGN KEY (network_id) REFERENCES networks(id) ON DELETE CASCADE
);

CREATE INDEX idx_servers_role ON servers(role);
CREATE INDEX idx_servers_status ON servers(status);

-- Firewalls table
CREATE TABLE firewalls (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  hetzner_id INTEGER UNIQUE NOT NULL,
  name TEXT NOT NULL,
  created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

-- Firewall rules table
CREATE TABLE firewall_rules (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  firewall_id INTEGER NOT NULL,
  direction TEXT NOT NULL CHECK(direction IN ('in', 'out')),
  port TEXT NOT NULL,
  protocol TEXT NOT NULL CHECK(protocol IN ('tcp', 'udp', 'icmp', 'esp', 'gre')),
  source_ips TEXT NOT NULL, -- JSON array
  description TEXT,
  FOREIGN KEY (firewall_id) REFERENCES firewalls(id) ON DELETE CASCADE
);

CREATE INDEX idx_firewall_rules_firewall_id ON firewall_rules(firewall_id);

-- SSH keys table
CREATE TABLE ssh_keys (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  hetzner_id INTEGER UNIQUE NOT NULL,
  name TEXT NOT NULL,
  public_key TEXT NOT NULL,
  created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

-- Deployments table
CREATE TABLE deployments (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  config_hash TEXT NOT NULL,
  status TEXT NOT NULL DEFAULT 'started' CHECK(status IN ('started', 'in_progress', 'completed', 'failed')),
  started_at DATETIME DEFAULT CURRENT_TIMESTAMP,
  completed_at DATETIME
);

CREATE INDEX idx_deployments_status ON deployments(status);

-- Deployment logs table
CREATE TABLE deployment_logs (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  deployment_id INTEGER NOT NULL,
  timestamp DATETIME DEFAULT CURRENT_TIMESTAMP,
  level TEXT NOT NULL CHECK(level IN ('debug', 'info', 'warn', 'error')),
  message TEXT NOT NULL,
  FOREIGN KEY (deployment_id) REFERENCES deployments(id) ON DELETE CASCADE
);

CREATE INDEX idx_deployment_logs_deployment_id ON deployment_logs(deployment_id);
CREATE INDEX idx_deployment_logs_level ON deployment_logs(level);
