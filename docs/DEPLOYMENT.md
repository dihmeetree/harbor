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
  name: 'my-app'
  type: 'ccx13'
  location: 'ash'
  image: 'ubuntu-24.04'

network:
  name: 'my-network'
  ip_range: '10.0.0.0/16'
  subnet_range: '10.0.1.0/24'

firewall:
  name: 'my-firewall'
  rules:
    - direction: 'in'
      port: '22'
      protocol: 'tcp'
      source_ips: ['current']
      description: 'SSH'
    - direction: 'in'
      port: '80'
      protocol: 'tcp'
      source_ips: ['0.0.0.0/0']
      description: 'HTTP'

container:
  name: 'app'
  image: 'nginx:latest'

loadbalancer:
  enabled: true
  replicas: 1
  server_type: 'ccx13'
  location: 'ash'
  image: 'ubuntu-24.04'
  service_name: 'apisix-data-plane'

app:
  enabled: true
  replicas: 1
  server_type: 'ccx13'
  location: 'ash'
  image: 'ubuntu-24.04'
  service_name: 'app'

apisix:
  admin_port: 9180
  api_key: 'change-me-in-production'
  upstreams:
    - id: 'web'
      name: 'web'
      nodes: {}
      enable_health_check: true
      health_check_path: '/'
      healthy_interval: 2
      unhealthy_interval: 1
  global_rules:
    - id: 'global-rules'
      plugins:
        prometheus: {}
  routes:
    - id: 'web'
      name: 'web-route'
      methods: ['GET']
      uri: '/*'
      upstream_id: 'web'
      plugins: {}
  ssl:
    id: '1'
    cert_path: ''
    key_path: ''

monitoring:
  prometheus_port: 9090
  cadvisor_port: 8080
  node_exporter_port: 9100
```

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

| Phase | Duration | Description |
|-------|----------|-------------|
| Infrastructure Provisioning | 5-10 min | Creating servers, networks, firewalls |
| Docker Installation | 5-10 min | Installing Docker on all servers |
| Service Deployment | 3-5 min | Starting all containers |
| APISIX Configuration | 1-2 min | Configuring routes and upstreams |
| **Total** | **15-30 min** | Complete deployment |

## Troubleshooting

### SSH Connection Fails

```bash
# Check server status
harbor status

# Manually SSH to server
ssh -i .harbor/ssh/your-key root@<server-ip>
```

### Docker Not Installing

```bash
# SSH to server
ssh -i .harbor/ssh/your-key root@<server-ip>

# Check Docker status
systemctl status docker

# Check installation logs
journalctl -u docker
```

### APISIX Not Starting

```bash
# SSH to control plane
ssh root@<control-plane-ip>

# Check containers
docker ps -a

# Check APISIX logs
docker logs apisix-control-plane
docker logs etcd

# Check configs
cat /opt/harbor/apisix-control.yaml
```

### Routes Not Working

```bash
# Check APISIX routes
curl -H "X-API-KEY: your-key" \
  http://<control-plane-ip>:9180/apisix/admin/routes

# Check upstreams
curl -H "X-API-KEY: your-key" \
  http://<control-plane-ip>:9180/apisix/admin/upstreams

# Check APISIX logs
ssh root@<data-plane-ip>
docker logs apisix-data-plane
```

## Next Steps

1. **Add Your Application**: Replace nginx with your actual app in `container.image`
2. **Configure SSL**: Add your certificates to enable HTTPS
3. **Add More Routes**: Configure additional routes in the config (including Grafana routing via APISIX)
4. **Access Grafana**: Login at `http://<control-plane-ip>:3000` (default: admin/admin) to visualize metrics
5. **Configure Autoscaling**: Customize autoscaler thresholds in `harbor.yaml` (already enabled by default)

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

## Cost Estimate (Hetzner)

With default configuration (ccx33 + 2×ccx13 + 2×ccx13):
- Control Plane (ccx33): ~€40/month
- Data Planes (2×ccx13): ~€15/month each
- App Servers (2×ccx13): ~€15/month each
- **Total**: ~€100/month

Minimum configuration (1 of each with ccx13): ~€45/month
