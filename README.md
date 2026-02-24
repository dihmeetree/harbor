<p align="center">
  <img src="https://i.ibb.co/LW3T9hN/IMG-0699.jpg" alt="Harbor" width="850">
</p>

Harbor is a CLI tool for provisioning and managing APISIX-based infrastructure on Hetzner Cloud using Flatcar Container Linux. It automates the deployment of a complete API gateway, load balancing, and observability stack with a single command.

## Features

- **Infrastructure as Code**: Define your infrastructure in a simple YAML configuration file
- **Automated Deployment**: Provision servers, networks, firewalls, and SSH keys with a single command
- **Complete Stack**: Deploys APISIX, etcd, Prometheus, and monitoring automatically
- **APISIX Integration**: Fully configured Apache APISIX as your API gateway and load balancer
- **Full Observability**: Built-in Prometheus, cAdvisor, and node-exporter for comprehensive monitoring
- **Private Networking**: All inter-server communication over secure Hetzner private network
- **Auto-scaling**: Automatic horizontal scaling based on CPU/memory metrics
- **Stateless Design**: All infrastructure state managed via Hetzner Cloud API with server labels
- **SSH Key Management**: Auto-generates SSH keys or uses your existing ones

## Architecture

Harbor deploys three types of servers:

### 1. **Control Plane Server** (1x)

- APISIX Control Plane (Admin API)
- etcd (Configuration storage)
- Prometheus (Metrics aggregation)
- Grafana (Metrics visualization with pre-configured dashboards)
- Autoscaler (Automatic horizontal scaling based on CPU/memory metrics)
- k6 (Continuous load testing - automatically targets all data planes)
- cAdvisor (Container metrics)
- node-exporter (System metrics)

### 2. **Data Plane Servers** (Configurable, default: 2x)

- APISIX Data Plane (HTTP/HTTPS traffic routing)
- cAdvisor (Container metrics)
- node-exporter (System metrics)

### 3. **App Servers** (Configurable, default: 2x)

- Your application containers
- cAdvisor (Container metrics)
- node-exporter (System metrics)

All servers communicate over a private Hetzner network for security and performance.

## Quick Start

### Prerequisites

- Go 1.21 or later
- Hetzner Cloud API token
- Flatcar Linux snapshot (built with `flatcar/flatcar.pkr.hcl`)
- 15-30 minutes for initial deployment

### Installation

```bash
# Clone the repository
git clone https://github.com/dihmeetree/harbor.git
cd harbor

# Build the CLI
go build -o harbor ./cmd/cli

# (Optional) Move to PATH
sudo mv harbor /usr/local/bin/
```

### Deploy Your First Stack

1. **Build Flatcar snapshot** (one-time setup):

```bash
cd flatcar
export HCLOUD_TOKEN="your-hetzner-api-token"
packer build flatcar.pkr.hcl
# Note the snapshot ID from the output
```

2. **Initialize configuration**:

```bash
harbor init
```

3. **Edit `harbor.yaml`** with your snapshot ID and settings:

```yaml
provider: hetzner
snapshot_id: 327228288  # Use your snapshot ID from step 1
# ... rest of configuration
```

4. **Set your Hetzner API token**:

```bash
export HETZNER_API_TOKEN="your-hetzner-api-token"
```

5. **Deploy infrastructure**:

```bash
harbor deploy
```

This will:

- ✅ Create private network and firewall
- ✅ Provision Flatcar Linux servers (control plane, data planes, app servers)
- ✅ Generate or use SSH keys
- ✅ Verify Docker and docker-compose on all servers
- ✅ Deploy APISIX control plane, etcd, and Prometheus
- ✅ Deploy APISIX data planes
- ✅ Deploy your application using docker-compose
- ✅ Configure APISIX routes and upstreams

**Total time: 15-30 minutes**

> **Note**: Harbor uses docker-compose for application deployment. Make sure you have a `docker-compose.yml` file and specify its path in your `harbor.yaml` config under `app.compose_file`.

6. **Check status**:

```bash
harbor status
```

7. **Test your deployment**:

```bash
# Get data plane IP from status output
curl http://<data-plane-ip>
```

8. **Clean up** (when done):

```bash
harbor destroy
```

## Configuration

### Minimal Configuration

```yaml
provider: hetzner
snapshot_id: 327228288  # Your Flatcar snapshot ID

control:
  name: "my-app-control"
  type: "ccx33"
  location: "ash"

network:
  name: "my-network"
  ip_range: "10.0.0.0/16"
  subnet_range: "10.0.1.0/24"

firewall:
  name: "my-firewall"
  rules:
    - direction: "in"
      port: "22"
      protocol: "tcp"
      source_ips: ["current"] # Auto-replaced with your IP
      description: "SSH access"
    - direction: "in"
      port: "443"
      protocol: "tcp"
      source_ips: ["0.0.0.0/0", "::/0"]
      description: "HTTPS from anywhere"

loadbalancer:
  replicas: 1  # At least 1 required
  server_type: "ccx13"
  location: "ash"

app:
  replicas: 1  # At least 1 required
  server_type: "ccx13"
  location: "ash"
  compose_file: "./docker-compose.yml"  # Path to your docker-compose file

apisix:
  admin_port: 9180
  api_key: "edd1c9f034335f136f87ad84b625c8f1" # Change in production!
  upstreams:
    - id: "web"
      name: "web"
      nodes: {} # Auto-populated with app server IPs
      enable_health_check: true
      health_check_path: "/"
      healthy_interval: 2
      unhealthy_interval: 1
  global_rules:
    - id: "global-rules"
      plugins:
        prometheus: {}
  routes:
    - id: "web"
      name: "web-route"
      methods: ["GET", "POST", "PUT", "DELETE", "OPTIONS"]
      uri: "/*"
      upstream_id: "web"
      plugins: {}
  ssl:
    id: "1"
    cert_path: ""
    key_path: ""
    client_ca_path: ""
    client_ca_depth: 1
    snis: []
    ssl_protocols: ["TLSv1.2", "TLSv1.3"]

monitoring:
  prometheus:
    port: 9090
  cadvisor:
    port: 8080
  node_exporter:
    port: 9100

autoscaler:
  enabled: true
  check_interval: 30 # Check metrics every 30 seconds
  cooldown: 300 # Wait 5 minutes between scaling operations
  cpu_threshold_up: 70.0 # Scale up when CPU > 70%
  cpu_threshold_down: 30.0 # Scale down when CPU < 30%
  mem_threshold_up: 80.0 # Scale up when Memory > 80%
  mem_threshold_down: 40.0 # Scale down when Memory < 40%
  min_replicas: 1 # Minimum servers per pool
  max_replicas: 10 # Maximum servers per pool

k6:
  enabled: false # Set to true to enable continuous load testing
  preallocated_vus: 10
  max_vus: 100
  rate: 10 # Requests per second
  duration: "30s"
  target_path: "/"
  connection_timeout: "10s"
  request_timeout: "30s"
  graceful_stop: "30s"
```

See `configs/harbor.example.yaml` for a complete configuration with all options and comments.

## CLI Commands

```bash
# Initialize configuration
harbor init

# Validate configuration
harbor validate

# Deploy infrastructure
harbor deploy

# Redeploy app services with zero downtime (blue-green rolling deployment)
harbor redeploy                    # Redeploy all services
harbor redeploy nginx              # Redeploy only nginx service
harbor redeploy api                # Redeploy only api service
harbor redeploy --yes              # Skip confirmation prompt
harbor redeploy --compose-file ./docker-compose.yml  # Override compose file

# Manually scale servers
harbor scale lb 5      # Scale load balancers to 5 servers
harbor scale app 10    # Scale app servers to 10 servers

# Restart services with latest configuration
harbor restart k6       # Restart k6 load testing (syncs k6/loadtest.js, applies harbor.yaml config)
harbor restart grafana  # Restart Grafana (syncs dashboards, provisioning, grafana.ini)

# Show infrastructure status
harbor status

# Destroy infrastructure
harbor destroy

# Show version
harbor version
```

## What Gets Deployed

### Services by Server Type

**Control Plane:**

- APISIX Control Plane (ports 9092, 9180)
- etcd (port 2379)
- Prometheus (port 9090)
- Grafana (port 3000)
- Autoscaler (enabled by default)
- cAdvisor (port 8080)
- node-exporter (port 9100)

**Data Planes:**

- APISIX Data Plane (ports 80, 443, 9091)
- cAdvisor (port 8080)
- node-exporter (port 9100)

**App Servers:**

- Your application (port 80)
- cAdvisor (port 8080)
- node-exporter (port 9100)

### Access Points After Deployment

- **Your App**: `http://<data-plane-ip>`
- **APISIX Admin API**: `http://<control-plane-ip>:9180`
- **Prometheus**: `http://<control-plane-ip>:9090`
- **Grafana**: `http://<control-plane-ip>:3000` (default login: admin/admin)
- **APISIX Metrics**: `http://<data-plane-ip>:9091/apisix/prometheus/metrics`

## Development

### Project Structure

```
harbor/
├── cmd/cli/              # CLI entrypoint
├── internal/
│   ├── config/           # Configuration parsing
│   ├── hetzner/          # Hetzner Cloud API client
│   ├── ssh/              # SSH connection manager
│   ├── docker/           # Docker installation
│   ├── apisix/           # APISIX Admin API client
│   ├── autoscaler/       # Automatic horizontal scaling
│   └── orchestrator/     # Deployment orchestration
├── pkg/models/           # Data models
└── configs/              # Configuration templates
```

### Building from Source

```bash
# Clone repository
git clone https://github.com/dihmeetree/harbor.git
cd harbor

# Install dependencies
go mod download

# Build
go build -o harbor ./cmd/cli

# Run tests
go test ./... -race -v

# Format code
gofmt -w .

# Vet code
go vet ./...
```

### Development Guidelines

See `CLAUDE.md` for detailed development guidelines including:

- Code quality standards
- Testing requirements
- Commit message format
- Pre-commit checks

## SSH Key Management

Harbor automatically manages SSH keys:

### Auto-generated (Default)

If `SSH_PRIVATE_KEY_PATH` is not set:

- Generates new 4096-bit RSA key pair
- Stores in `.harbor/ssh/<project-name>-key`
- Uploads public key to Hetzner Cloud
- Reuses existing keys on subsequent deployments

### Use Your Own Key

If `SSH_PRIVATE_KEY_PATH` is set:

- Uses your existing private key
- Looks for public key at `${SSH_PRIVATE_KEY_PATH}.pub`
- Uploads to Hetzner Cloud

## Monitoring

Prometheus automatically collects metrics from:

- **APISIX**: Request rates, latencies, status codes, upstream health
- **cAdvisor**: Container CPU, memory, network, disk usage
- **node-exporter**: System CPU, memory, disk, network

Access Prometheus at `http://<control-plane-ip>:9090`

## Security

- **Private Network**: All inter-server communication uses private IPs
- **Firewall**: Customizable firewall rules applied to all servers
- **SSH Keys**: Strong 4096-bit RSA keys, auto-generated or custom
- **API Keys**: Configurable APISIX Admin API key
- **SSL/TLS**: Optional SSL certificate configuration
- **Current IP Detection**: `'current'` in firewall rules auto-replaced with your IP

## Cost Estimates (Hetzner Cloud)

### Minimal Setup (1 of each, ccx13)

- Control Plane: ~€15/month
- 1 Data Plane: ~€15/month
- 1 App Server: ~€15/month
- **Total**: ~€45/month

### Default Setup (ccx33 + 2×ccx13 + 2×ccx13)

- Control Plane (ccx33): ~€40/month
- 2 Data Planes (ccx13): ~€30/month
- 2 App Servers (ccx13): ~€30/month
- **Total**: ~€100/month

### Production Setup (cax31 + 3×cax21 + 5×cax31)

- Control Plane (cax31): ~€30/month
- 3 Data Planes (cax21): ~€40/month
- 5 App Servers (cax31): ~€150/month
- **Total**: ~€220/month

## Troubleshooting

### SSH Connection Fails

```bash
# Check server status
harbor status

# Wait for servers to boot (they need 1-2 minutes)
sleep 120

# Try manual SSH (Flatcar uses 'core' user)
ssh -i .harbor/ssh/<project-name>-key core@<server-ip>
```

### Docker or docker-compose Not Available

```bash
# SSH to server
ssh -i .harbor/ssh/<project-name>-key core@<server-ip>

# Check Docker status
systemctl status docker

# Check docker-compose is loaded via sysext
docker-compose version

# Verify sysext is active
systemctl status systemd-sysext
```

### APISIX Not Starting

```bash
# SSH to control plane (Flatcar uses 'core' user)
ssh -i .harbor/ssh/<project-name>-key core@<control-plane-ip>

# Check running containers
docker ps -a

# Check APISIX logs
docker logs apisix-control-plane
docker logs etcd

# Check configuration
cat /var/lib/harbor/apisix-control.yaml
```

### Routes Not Working

```bash
# Check APISIX configuration via Admin API
curl -H "X-API-KEY: your-api-key" \
  http://<control-plane-ip>:9180/apisix/admin/routes

# Check upstreams
curl -H "X-API-KEY: your-api-key" \
  http://<control-plane-ip>:9180/apisix/admin/upstreams

# Check data plane logs
ssh -i .harbor/ssh/<project-name>-key core@<data-plane-ip>
docker logs apisix-data-plane
```

## Auto-scaling

Harbor includes an automatic horizontal scaler that monitors your infrastructure and scales load balancers and app servers based on CPU and memory metrics.

### How It Works

1. **Metrics Collection**: Prometheus collects CPU and memory metrics from all servers
2. **Threshold Monitoring**: Autoscaler checks metrics every 30 seconds (configurable)
3. **Scaling Decisions**:
   - **Scale Up**: When CPU > 70% OR Memory > 80%
   - **Scale Down**: When CPU < 30% AND Memory < 40%
4. **Server Management**:
   - Creates new Hetzner servers with Docker and services
   - Adds/removes servers from APISIX upstream pools
   - Respects min/max replica limits
5. **Cooldown Period**: Waits 5 minutes between scaling operations

### Configuration

Enable autoscaling in your `harbor.yaml`:

```yaml
autoscaler:
  enabled: true # Enable/disable autoscaling
  check_interval: 30 # Check metrics every 30 seconds
  cooldown: 300 # Wait 5 minutes between scaling operations
  cpu_threshold_up: 70.0 # Scale up when CPU > 70%
  cpu_threshold_down: 30.0 # Scale down when CPU < 30%
  mem_threshold_up: 80.0 # Scale up when Memory > 80%
  mem_threshold_down: 40.0 # Scale down when Memory < 40%
  min_replicas: 1 # Minimum servers per pool
  max_replicas: 10 # Maximum servers per pool
```

### Server Labels

The autoscaler uses Hetzner server labels for service discovery:

- **Manually created servers** (via `harbor deploy`): `autoscale=false` - Won't be deleted
- **Auto-created servers**: `autoscale=true` - Can be deleted during scale-down

### Monitoring Autoscaler

Check autoscaler logs:

```bash
# Get control plane IP
harbor status

# View autoscaler logs
ssh -i .harbor/ssh/<project-name>-key core@<control-plane-ip> "docker logs autoscaler --tail 100"
```

Example output:

```
[autoscaler] [info] Running autoscaler check cycle...
[autoscaler] [info] [loadbalancer] Metrics - Replicas: 2 | CPU: 45.32% (threshold: 30%/70%) | Memory: 62.15% (threshold: 40%/80%)
[autoscaler] [info] [app] Metrics - Replicas: 2 | CPU: 78.45% (threshold: 30%/70%) | Memory: 55.20% (threshold: 40%/80%)
[autoscaler] [info] [app] Scaling UP (CPU: 78.45%, Memory: 55.20%)
[autoscaler] [info] [app] Creating server: harbor-app-3
```

## Load Testing with k6

Harbor includes integrated Grafana k6 for continuous load testing of your data plane servers. When enabled, k6 runs on the control plane and automatically targets all data planes.

### How It Works

1. **Automatic Target Discovery**: k6 automatically targets all data plane private IPs
2. **Continuous Testing**: Runs continuously at configured request rates
3. **Dynamic Updates**: When data planes scale up/down, k6 automatically updates its targets
4. **Performance Thresholds**: Built-in thresholds for response times and error rates
5. **Metrics Integration**: Results feed into Prometheus for monitoring

### Configuration

Enable k6 in your `harbor.yaml`:

```yaml
k6:
  enabled: true # Enable continuous load testing
  preallocated_vus: 10 # Pre-allocated virtual users
  max_vus: 500 # Maximum virtual users (scales based on rate)
  rate: 50 # Target requests per second
  duration: "30m" # Test duration ("30s", "5m", "1h", or "0" for infinite)
  target_path: "/" # Path to test (e.g., "/api/health")
  connection_timeout: "10s" # Connection timeout
  request_timeout: "30s" # Request timeout per request
  graceful_stop: "30s" # Graceful shutdown duration
```

### Performance Testing Scenarios

**Light Testing** (Development/Staging):

```yaml
k6:
  enabled: true
  rate: 10 # 10 req/s
  preallocated_vus: 5
  max_vus: 50
  duration: "10m"
```

**Moderate Testing** (Pre-production):

```yaml
k6:
  enabled: true
  rate: 50 # 50 req/s
  preallocated_vus: 10
  max_vus: 200
  duration: "30m"
```

**Heavy Testing** (Load/Stress testing):

```yaml
k6:
  enabled: true
  rate: 500 # 500 req/s
  preallocated_vus: 50
  max_vus: 1000
  duration: "1h"
```

### Performance Thresholds

The k6 script includes built-in thresholds:

- **p95 Latency**: 95% of requests must complete under 500ms
- **Error Rate**: Less than 10% error rate

### Monitoring k6

Check k6 test progress and results:

```bash
# Get control plane IP
harbor status

# View k6 logs (live test output)
ssh root@<control-plane-ip> "docker logs k6 --tail 100 -f"

# Check if k6 is running
ssh root@<control-plane-ip> "docker ps | grep k6"
```

Example k6 output:

```
Starting load test
Targets: http://10.0.1.3, http://10.0.1.4, http://10.0.1.5
Path: /
Rate: 50 req/s
Duration: 30m
Preallocated VUs: 10
Max VUs: 500

running (01m30s), 000/010 VUs, 4500 complete and 0 interrupted iterations
constant_rate ✓ [======================================] 010/500 VUs  01m30s  50 iters/s

     ✓ status is 200
     ✓ response time < 500ms

     checks.........................: 100.00% ✓ 9000      ✗ 0
     data_received..................: 2.1 MB  23 kB/s
     data_sent......................: 360 kB  4.0 kB/s
     http_req_duration..............: avg=45ms   min=12ms med=42ms max=180ms p(95)=85ms  p(99)=120ms
     http_reqs......................: 4500    50/s
     iteration_duration.............: avg=46ms   min=13ms med=43ms max=182ms p(95)=86ms  p(99)=121ms
```

### Integration with Autoscaling

When used with autoscaling, k6 provides realistic load that can trigger scale events:

```yaml
autoscaler:
  enabled: true
  cpu_threshold_up: 70.0
  cpu_threshold_down: 30.0

k6:
  enabled: true
  rate: 100 # Generate load to test autoscaling
  duration: "1h"
```

This creates a feedback loop where:

1. k6 generates consistent load
2. Autoscaler monitors metrics from Prometheus
3. Servers scale up/down based on load
4. k6 automatically updates to target new data planes

### Updating k6 Configuration

#### Automatic Updates

When you scale data planes, k6 targets are automatically updated:

```bash
# Scale up - k6 automatically targets new data planes
harbor scale lb 5

# Scale down - k6 stops targeting removed data planes
harbor scale lb 2

# Redeploy services - k6 refreshes all targets
harbor redeploy
```

#### Manual Configuration Update

When you change k6 settings in `harbor.yaml` (rate, VUs, duration, path, CPU/memory limits, etc.) or modify the k6 test script (`k6/loadtest.js`), restart k6 to apply the new configuration:

```bash
# Edit harbor.yaml to change k6 settings
vim harbor.yaml

# Or edit the k6 test script
vim k6/loadtest.js

# Restart k6 with new configuration
harbor restart k6
```

The restart command will:

- Copy `k6/loadtest.js` script to control plane (if it exists)
- Stop the current k6 container
- Query Hetzner API for current data plane IPs (ensures accurate targets)
- Recreate k6 container with updated settings from harbor.yaml
- Apply CPU and memory limits from configuration
- Start load testing with new configuration

Example output:

```
[info] Restarting k6...
[info] Control plane: harbor-control (X.X.X.X)
[info] Copying k6 load test script to control plane...
[info] ✓ k6 script copied to control plane
[info] Targeting 3 data plane(s): http://10.0.1.3,http://10.0.1.4,http://10.0.1.5
[info] Stopping existing k6 container...
[info] Starting k6 with updated configuration...
[info]   Rate: 100 req/s | VUs: 20-500 | Duration: 1h | Path: /api/health
[info]   CPU Limit: 2.0 cores
[info]   Memory Limit: 2g
[info] ✓ k6 successfully restarted with latest configuration
```

### Disabling k6

To permanently disable k6 in your deployment:

```yaml
k6:
  enabled: false
```

Then redeploy:

```bash
harbor redeploy
```

## Grafana Monitoring

Harbor deploys Grafana on the control plane with pre-configured dashboards for monitoring your infrastructure. Grafana automatically connects to Prometheus for metrics collection.

### Accessing Grafana

After deployment, access Grafana at:

```
http://<control-plane-ip>:3000
```

Default credentials:
- Username: `admin`
- Password: `admin` (you'll be prompted to change this on first login)

### Pre-configured Dashboards

Harbor includes dashboards for:

- **cAdvisor Metrics**: Container CPU, memory, network, and disk usage
- **Node Metrics**: System-level CPU, memory, disk, and network metrics
- **APISIX Metrics**: Request rates, latencies, status codes, upstream health

### Updating Grafana Configuration

When you modify Grafana configuration files or dashboards locally, restart Grafana to apply the changes:

```bash
# Edit Grafana configuration
vim grafana/config/grafana.ini

# Or update dashboards
vim grafana/dashboards/my-dashboard.json

# Or modify datasource provisioning
vim grafana/provisioning/datasources/prometheus.yml

# Restart Grafana with new configuration
harbor restart grafana
```

The restart command will:

- Copy `grafana/provisioning` directory to control plane (datasources, dashboard configs)
- Copy `grafana/dashboards` directory to control plane (dashboard JSON files)
- Copy `grafana/config/grafana.ini` to control plane (main configuration)
- Restart Grafana container to load new configuration

Example output:

```
[info] Restarting grafana...
[info] Control plane: harbor-control (X.X.X.X)
[info] Copying Grafana provisioning directory...
[info] ✓ Grafana provisioning directory copied
[info] Copying Grafana dashboards directory...
[info] ✓ Grafana dashboards directory copied
[info] Copying Grafana configuration file...
[info] ✓ Grafana configuration file copied
[info] Restarting Grafana container...
[info] Grafana container restarted successfully
[info] ✓ Grafana successfully restarted
```

### Grafana File Structure

```
grafana/
├── config/
│   └── grafana.ini              # Main Grafana configuration
├── dashboards/
│   ├── cadvisor.json           # Container metrics dashboard
│   ├── node-exporter.json      # System metrics dashboard
│   └── custom-dashboard.json   # Your custom dashboards
└── provisioning/
    ├── dashboards/
    │   └── default.yml         # Dashboard provisioning config
    └── datasources/
        └── prometheus.yml       # Prometheus datasource config
```

## Advanced Usage

### Custom Application Deployment

Harbor uses docker-compose for deploying applications. Create a `docker-compose.yml` file:

```yaml
services:
  app:
    container_name: my-app
    image: myregistry/myapp:latest
    restart: always
    ports:
      - "80:80"
    environment:
      - APP_ENV=production
    volumes:
      - ./config.yml:/app/config.yml:ro
```

Then specify it in your harbor.yaml:

```yaml
app:
  enabled: true
  replicas: 2
  server_type: "ccx13"
  location: "ash"
  compose_file: "./docker-compose.yml"
```

Harbor will automatically:
- Copy docker-compose.yml to each app server
- Copy all volume-mounted files (e.g., `./config.yml`)
- Replace `SERVER_ID_PLACEHOLDER` with actual server names
- Run `docker compose up` on each server

**Zero-downtime updates:**
```bash
# Update all services with blue-green rolling deployment
harbor redeploy

# Update specific service only
harbor redeploy nginx
harbor redeploy api

# Override compose file location
harbor redeploy --compose-file ./production-compose.yml

# Skip confirmation (useful for CI/CD)
harbor redeploy --yes
```

### SSL/TLS Configuration

Add SSL certificates:

```yaml
apisix:
  ssl:
    id: "1"
    cert_path: "./certs/domain.crt"
    key_path: "./certs/domain.key"
    client_ca_path: "./certs/ca.pem"
    client_ca_depth: 1
    snis:
      - "yourdomain.com"
      - "*.yourdomain.com"
    ssl_protocols:
      - "TLSv1.2"
      - "TLSv1.3"
```

### Custom Routes

Add more routes:

```yaml
apisix:
  routes:
    - id: "api"
      name: "api-route"
      methods: ["GET", "POST"]
      uri: "/api/*"
      upstream_id: "web"
      plugins:
        rate-limit:
          rate: 100
          burst: 200
          key: remote_addr
```

### Health Checks

Configure custom health checks:

```yaml
apisix:
  upstreams:
    - id: "web"
      name: "web"
      enable_health_check: true
      health_check_path: "/health"
      healthy_interval: 2
      unhealthy_interval: 1
```

## Documentation

- **[QUICKSTART.md](QUICKSTART.md)**: Quick start guide for first-time users
- **[DEPLOYMENT.md](DEPLOYMENT.md)**: Detailed deployment guide with troubleshooting
- **[CLAUDE.md](CLAUDE.md)**: Development guidelines and code standards
- **[configs/harbor.example.yaml](configs/harbor.example.yaml)**: Full configuration example

## Roadmap

- [ ] Digital Ocean support
- [x] Auto-scaling based on CPU/memory metrics
- [x] Grafana deployment with pre-configured dashboards
- [ ] HTTP API server for web UI integration
- [ ] `harbor ssh` command for easy server access
- [ ] `harbor logs` command with log streaming
- [ ] `harbor apisix` subcommands for route management
- [ ] Rolling updates support
- [ ] Backup and restore functionality
- [ ] Multi-region deployments

## Contributing

Contributions are welcome! Please:

1. Fork the repository
2. Create a feature branch
3. Make your changes following CLAUDE.md guidelines
4. Run tests and code quality checks:
   ```bash
   gofmt -w .
   go build ./...
   go vet ./...
   go test ./... -race
   ```
5. Submit a pull request

## License

MIT License - see LICENSE file for details

## Support

- **Issues**: [GitHub Issues](https://github.com/dihmeetree/harbor/issues)
- **Documentation**: See docs in this repository
- **Examples**: Check `configs/` directory

## Acknowledgments

Built with:

- [Apache APISIX](https://apisix.apache.org/) - Cloud-native API Gateway
- [Hetzner Cloud](https://www.hetzner.com/cloud) - Infrastructure provider
- [Prometheus](https://prometheus.io/) - Monitoring and alerting
- [etcd](https://etcd.io/) - Distributed key-value store

---

**Ready to deploy?** Run `harbor init` to get started! 🚀
