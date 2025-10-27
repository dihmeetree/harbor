# Harbor Deployment Guide

## Overview

Harbor is now fully functional and deploys a complete APISIX-based infrastructure on Hetzner Cloud. Here's what gets deployed:

## Architecture

### Server Types

1. **Control Plane Server** (1x)

   - APISIX Control Plane (Admin API + etcd integration)
   - etcd (Configuration store)
   - Prometheus (Metrics aggregation)
   - Grafana (Metrics visualization)
   - Autoscaler (Automatic server scaling based on metrics)
   - k6 (Continuous load testing - automatically targets all data planes with dynamic updates)
   - cAdvisor (Container metrics)
   - node-exporter (System metrics)

2. **Data Plane Servers** (Configurable, default: 2x)

   - APISIX Data Plane (HTTP/HTTPS traffic routing)
   - cAdvisor (Container metrics)
   - node-exporter (System metrics)

3. **App Servers** (Configurable, default: 2x)
   - Your application containers (default: nginx)
   - cAdvisor (Container metrics)
   - node-exporter (System metrics)

## Deployment Flow

When you run `harbor deploy`, the following happens:

### Phase 1: Infrastructure Provisioning (5-10 minutes)

1. **SSH Key Management**

   - If `SSH_PRIVATE_KEY_PATH` is set: Uses your existing key
   - If not set: Generates new keys in `.harbor/ssh/` directory
   - Uploads public key to Hetzner Cloud

2. **Network Creation**

   - Creates private network with specified IP range
   - Creates subnet for server communication

3. **Firewall Creation**

   - Converts config rules to Hetzner firewall
   - Replaces `'current'` with your actual IP
   - Creates firewall with all rules

4. **Server Creation**
   - Creates control plane server
   - Creates data plane servers (parallel)
   - Creates app servers (parallel)
   - Attaches all to private network
   - Applies firewall rules

### Phase 2: Docker Installation (5-10 minutes)

5. **Wait for Servers**

   - Waits 30 seconds for servers to boot
   - Establishes SSH connections

6. **Install Docker**
   - Runs official Docker install script on each server
   - Enables and starts Docker service
   - Verifies installation

### Phase 3: Service Deployment (3-5 minutes)

7. **Deploy Control Plane**

   - Generates docker-compose.yml from template
   - Generates APISIX control plane config
   - Generates Prometheus config with all targets
   - Deploys via `docker compose up -d`
   - Waits 30 seconds for services to start

8. **Deploy Data Planes**

   - For each data plane server:
     - Generates docker-compose.yml
     - Generates APISIX data plane config (points to control plane etcd)
     - Deploys services

9. **Deploy App Servers**
   - For each app server:
     - Generates docker-compose.yml
     - Deploys application container
     - Deploys monitoring

### Phase 4: APISIX Configuration (1-2 minutes)

10. **Wait for APISIX**

    - Waits for APISIX Admin API to be ready
    - Retries up to 30 times with 2-second intervals

11. **Configure Upstreams**

    - Creates upstreams with app server private IPs
    - Configures health checks
    - Sets up keepalive pools

12. **Configure Routes**

    - Creates all routes from config
    - Applies plugins (caching, custom plugins, etc.)

13. **Configure Global Rules**

    - Enables Prometheus metrics
    - Applies other global plugins

14. **Configure SSL (Optional)**
    - Reads certificate files
    - Uploads to APISIX
    - Configures SNIs and protocols

## What You Get

After deployment completes, you have:

### Access Points

- **Load Balancer**: `http://<data-plane-public-ip>` (port 80/443)
- **APISIX Admin API**: `http://<control-plane-public-ip>:9180`
- **Prometheus**: `http://<control-plane-public-ip>:9090`
- **Grafana**: `http://<control-plane-public-ip>:3000` or via APISIX route (e.g., `https://grafana.yourdomain.com`)
- **Prometheus Metrics**: `http://<data-plane-public-ip>:9091/apisix/prometheus/metrics`

### Monitoring

All servers report metrics to Prometheus:

- **APISIX metrics**: Request rates, latencies, status codes
- **Container metrics**: CPU, memory, network, disk
- **System metrics**: CPU, memory, disk, network

### High Availability

- Multiple data plane servers for load distribution
- Health checks automatically remove failing upstreams
- Services restart automatically on failure

## Configuration

### Minimal Configuration

```yaml
provider: hetzner

server:
  name: "my-app"
  type: "ccx13"
  location: "ash"
  image: "ubuntu-24.04"

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
      source_ips: ["current"]
      description: "SSH"
    - direction: "in"
      port: "80"
      protocol: "tcp"
      source_ips: ["0.0.0.0/0"]
      description: "HTTP"

container:
  name: "app"
  image: "nginx:latest"

loadbalancer:
  enabled: true
  replicas: 1
  server_type: "ccx13"
  location: "ash"
  image: "ubuntu-24.04"
  service_name: "apisix-data-plane"

app:
  enabled: true
  replicas: 1
  server_type: "ccx13"
  location: "ash"
  image: "ubuntu-24.04"
  service_name: "app"

apisix:
  admin_port: 9180
  api_key: "change-me-in-production"
  upstreams:
    - id: "web"
      name: "web"
      nodes: {}
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
      methods: ["GET"]
      uri: "/*"
      upstream_id: "web"
      plugins: {}
  ssl:
    id: "1"
    cert_path: ""
    key_path: ""

monitoring:
  prometheus_port: 9090
  cadvisor_port: 8080
  node_exporter_port: 9100

autoscaler:
  enabled: true
  check_interval: 30
  cooldown: 300
  cpu_threshold_up: 70.0
  cpu_threshold_down: 30.0
  mem_threshold_up: 80.0
  mem_threshold_down: 40.0
  min_replicas: 1
  max_replicas: 10

k6:
  enabled: false # Set to true to enable continuous load testing
  preallocated_vus: 10 # Pre-allocated virtual users
  max_vus: 100 # Maximum virtual users (auto-scales based on rate)
  rate: 10 # Target requests per second
  duration: "30s" # Test duration ("30s", "5m", "1h", or "0" for infinite)
  target_path: "/" # Path to test (e.g., "/api/health")
  connection_timeout: "10s" # Connection timeout
  request_timeout: "30s" # Request timeout per request
  graceful_stop: "30s" # Graceful shutdown duration
```

**Note**: k6 automatically targets all data plane private IPs. When data planes scale up/down (via autoscaler or manual scaling), k6 targets are automatically updated.

## Usage

### Deploy

```bash
export HETZNER_API_TOKEN="your-token"
harbor deploy
```

### Check Status

```bash
harbor status
```

Output:

```
Infrastructure Status:

Control Plane:
  - harbor-control
    Public IP:  X.X.X.X
    Private IP: 10.0.1.2
    Status:     running

Data Planes (2):
  - harbor-control-lb-1
    Public IP:  X.X.X.X
    Private IP: 10.0.1.3
    Status:     running
  - harbor-control-lb-2
    Public IP:  X.X.X.X
    Private IP: 10.0.1.4
    Status:     running

App Servers (2):
  - harbor-control-app-1
    Public IP:  X.X.X.X
    Private IP: 10.0.1.5
    Status:     running
  - harbor-control-app-2
    Public IP:  X.X.X.X
    Private IP: 10.0.1.6
    Status:     running
```

### Destroy

```bash
harbor destroy
```

## Time Estimates

| Phase                       | Duration      | Description                                      |
| --------------------------- | ------------- | ------------------------------------------------ |
| Infrastructure Provisioning | 5-10 min      | Creating servers, networks, firewalls            |
| docker-compose Installation | 2-5 min       | Installing docker-compose on all servers         |
| Service Deployment          | 3-5 min       | Starting all containers                          |
| APISIX Configuration        | 1-2 min       | Configuring routes and upstreams                 |
| **Total**                   | **15-30 min** | Complete deployment                              |

## Troubleshooting

### SSH Connection Fails

```bash
# Check server status
harbor status

# Manually SSH to server (Flatcar uses 'core' user)
ssh -i .harbor/ssh/your-key core@<server-ip>
```

### docker-compose Not Installing

```bash
# SSH to server (Flatcar uses 'core' user)
ssh -i .harbor/ssh/your-key core@<server-ip>

# Check docker-compose
/opt/bin/docker-compose version

# Check Docker status
systemctl status docker
```

### APISIX Not Starting

```bash
# SSH to control plane (Flatcar uses 'core' user)
ssh -i .harbor/ssh/your-key core@<control-plane-ip>

# Check containers
docker ps -a

# Check APISIX logs
docker logs apisix-control-plane
docker logs etcd

# Check configs
cat /var/lib/harbor/apisix-control.yaml
```

### Routes Not Working

```bash
# Check APISIX routes
curl -H "X-API-KEY: your-key" \
  http://<control-plane-ip>:9180/apisix/admin/routes

# Check upstreams
curl -H "X-API-KEY: your-key" \
  http://<control-plane-ip>:9180/apisix/admin/upstreams

# Check APISIX logs (Flatcar uses 'core' user)
ssh -i .harbor/ssh/your-key core@<data-plane-ip>
docker logs apisix-data-plane
```

## Next Steps

1. **Add Your Application**: Replace nginx with your actual app in `container.image`
2. **Configure SSL**: Add your certificates to enable HTTPS
3. **Add More Routes**: Configure additional routes in the config (including Grafana routing via APISIX)
4. **Access Grafana**: Login at `http://<control-plane-ip>:3000` (default: admin/admin) to visualize metrics
5. **Configure Autoscaling**: Customize autoscaler thresholds in `harbor.yaml` (already enabled by default)
6. **Enable Load Testing**: Set `k6.enabled: true` in `harbor.yaml` to continuously test your load balancers

## State Management

Harbor stores all state in `~/.harbor/state.db` (SQLite):

- Server IDs and IPs
- Network and firewall IDs
- Deployment history and logs

**Important**: Back up this file! You need it to manage and destroy infrastructure.

## Security Considerations

1. **Change Default API Key**: Update `apisix.api_key` in production
2. **Restrict Firewall Rules**: Use specific IPs instead of `0.0.0.0/0`
3. **Use SSH Keys**: Never use passwords for SSH
4. **Protect State DB**: Keep `~/.harbor/state.db` secure
5. **Enable HTTPS**: Configure SSL certificates for production
6. **Private Network**: All inter-server communication uses private IPs

## Load Testing with k6

Harbor includes integrated Grafana k6 for continuous load testing. k6 runs on the control plane and automatically targets all data plane servers.

### Key Features

- **Automatic Target Discovery**: k6 targets all data plane private IPs automatically
- **Dynamic Updates**: When scaling (manual or auto), k6 targets update automatically
- **Performance Thresholds**: Built-in thresholds for latency (p95 < 500ms) and error rate (< 10%)
- **Flexible Configuration**: Configurable request rates, VUs, duration, and target paths

### Configuration Examples

**Development Testing** (Light load):

```yaml
k6:
  enabled: true
  rate: 10 # 10 requests/second
  preallocated_vus: 5
  max_vus: 50
  duration: "10m"
  target_path: "/"
```

**Production Testing** (Moderate load):

```yaml
k6:
  enabled: true
  rate: 100 # 100 requests/second
  preallocated_vus: 20
  max_vus: 500
  duration: "1h"
  target_path: "/api/health"
```

**Stress Testing** (Heavy load):

```yaml
k6:
  enabled: true
  rate: 500 # 500 requests/second
  preallocated_vus: 50
  max_vus: 1000
  duration: "30m"
  target_path: "/"
```

### Monitoring k6

View k6 logs to see test progress and results:

```bash
# Get control plane IP
harbor status

# View live k6 output (Flatcar uses 'core' user)
ssh -i .harbor/ssh/your-key core@<control-plane-ip> "docker logs k6 --tail 100 -f"

# Check k6 container status
ssh -i .harbor/ssh/your-key core@<control-plane-ip> "docker ps | grep k6"
```

Example output:

```
Starting load test
Targets: http://10.0.1.3, http://10.0.1.4
Path: /
Rate: 50 req/s
Duration: 30m

running (05m00s), 000/010 VUs, 15000 complete and 0 interrupted iterations
     ✓ status is 200
     ✓ response time < 500ms

     checks.........................: 100.00% ✓ 30000     ✗ 0
     http_req_duration..............: avg=45ms   p(95)=85ms  p(99)=120ms
     http_reqs......................: 15000   50/s
```

### Integration with Autoscaling

When combined with autoscaling, k6 creates a realistic load testing environment:

```yaml
autoscaler:
  enabled: true
  cpu_threshold_up: 70.0

k6:
  enabled: true
  rate: 100 # Generate steady load
  duration: "1h"
```

This creates a feedback loop:

1. k6 generates consistent load on data planes
2. Autoscaler monitors CPU/memory via Prometheus
3. New data planes are created when thresholds are exceeded
4. k6 automatically includes new data planes in its target list
5. Load is distributed across all data planes

### Automatic Target Updates

k6 targets are automatically updated when:

1. **Initial Deployment**: `harbor deploy` configures k6 with all data plane IPs
2. **Manual Scaling**: `harbor scale lb 5` recreates k6 container with updated targets
3. **Autoscaling**: Autoscaler updates k6 when creating/destroying data planes
4. **Redeployment**: `harbor redeploy` refreshes k6 with current infrastructure state

No manual intervention required!

### Manual Configuration Updates

When you update k6 settings in `harbor.yaml` (rate, VUs, duration, target path, timeouts), use the restart command to apply changes:

```bash
# Edit k6 configuration in harbor.yaml
vim harbor.yaml

# Or edit the k6 test script
vim k6/loadtest.js

# Restart k6 with new settings
harbor restart k6
```

The restart command:

- Copies `k6/loadtest.js` script to control plane (if it exists locally)
- Stops the current k6 container
- Queries Hetzner API for current data plane IPs (always uses fresh data)
- Recreates k6 with updated configuration from harbor.yaml
- Applies CPU and memory limits from configuration
- Starts load testing immediately

Example:

```bash
$ harbor restart k6
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

### Restarting Grafana

To apply Grafana configuration changes (dashboards, provisioning, grafana.ini):

```bash
# Edit Grafana configuration
vim grafana/config/grafana.ini

# Or update dashboards
vim grafana/dashboards/my-dashboard.json

# Restart Grafana with new configuration
harbor restart grafana
```

The restart command:

- Copies `grafana/provisioning` directory to control plane (datasources, dashboard configs)
- Copies `grafana/dashboards` directory to control plane (dashboard JSON files)
- Copies `grafana/config/grafana.ini` to control plane (main configuration)
- Restarts Grafana container to load new configuration

Example:

```bash
$ harbor restart grafana
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

## Cost Estimate (Hetzner)

With default configuration (ccx33 + 2×ccx13 + 2×ccx13):

- Control Plane (ccx33): ~€40/month
- Data Planes (2×ccx13): ~€15/month each
- App Servers (2×ccx13): ~€15/month each
- **Total**: ~€100/month

Minimum configuration (1 of each with ccx13): ~€45/month
