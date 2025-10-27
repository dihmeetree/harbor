# Harbor Quick Start Guide

## Prerequisites

- Hetzner Cloud account with API token
- Go 1.21+ installed (for building from source)
- Packer installed (for building Flatcar snapshot)

## Installation

```bash
# Clone the repository
git clone https://github.com/dihmeetree/harbor.git
cd harbor

# Build the CLI
go build -o harbor ./cmd/cli

# Move to PATH (optional)
sudo mv harbor /usr/local/bin/
```

## Quick Deploy

### 1. Build Flatcar Snapshot (One-time setup)

```bash
cd flatcar
export HCLOUD_TOKEN="your-hetzner-api-token"
packer build flatcar.pkr.hcl
# Note the snapshot ID from the output
```

### 2. Initialize Configuration

```bash
cd ..
harbor init
```

This creates `harbor.yaml` from the example template.

### 3. Edit Configuration

Edit `harbor.yaml` with your snapshot ID and settings:

```yaml
provider: hetzner
snapshot_id: 327228288  # Use your snapshot ID from step 1

control:
  name: 'my-app-control'
  type: 'ccx13'
  location: 'ash'

# ... rest of config
```

### 4. Set API Token

```bash
export HETZNER_API_TOKEN="your-hetzner-api-token-here"
```

### 5. Deploy

```bash
harbor deploy
```

This will:
- ✅ Create infrastructure (networks, firewalls, servers)
- ✅ Provision Flatcar Linux servers from snapshot
- ✅ Install docker-compose on all servers
- ✅ Deploy APISIX control plane + etcd + Prometheus + Grafana + Autoscaler + k6 (if enabled)
- ✅ Deploy APISIX data planes
- ✅ Deploy your app
- ✅ Configure APISIX routes and upstreams
- ⏱️ Total time: 15-30 minutes

### 6. Check Status

```bash
harbor status
```

### 7. Test Your Deployment

```bash
# Get data plane IP from status command
DATA_PLANE_IP="<from-status-output>"

# Test HTTP endpoint
curl http://$DATA_PLANE_IP/

# Check Prometheus metrics
curl http://$DATA_PLANE_IP:9091/apisix/prometheus/metrics
```

### 8. Access Services

- **Your App**: `http://<data-plane-ip>`
- **APISIX Admin API**: `http://<control-plane-ip>:9180`
- **Prometheus**: `http://<control-plane-ip>:9090`
- **Grafana**: `http://<control-plane-ip>:3000` (default login: admin/admin)

### 9. Destroy (When Done)

```bash
harbor destroy
```

## Configuration Options

### Minimal Setup (1 server of each type)

```yaml
loadbalancer:
  enabled: true
  replicas: 1           # Just 1 data plane
  server_type: 'ccx13'  # Smaller instance

app:
  enabled: true
  replicas: 1           # Just 1 app server
  server_type: 'ccx13'
```

### Production Setup (HA with multiple servers)

```yaml
loadbalancer:
  enabled: true
  replicas: 3           # 3 data planes for HA
  server_type: 'ccx23'  # Larger instances

app:
  enabled: true
  replicas: 5           # 5 app servers
  server_type: 'ccx33'
```

## Custom Application

Replace nginx with your app:

```yaml
container:
  name: 'my-app'
  image: 'myregistry/myapp:latest'
```

## SSH Keys

Harbor automatically manages SSH keys:

**Option 1: Auto-generate (default)**
```bash
# Harbor creates keys in .harbor/ssh/
harbor deploy
```

**Option 2: Use your own key**
```bash
export SSH_PRIVATE_KEY_PATH="~/.ssh/id_rsa"
harbor deploy
```

## Firewall Rules

Allow your IP for SSH:

```yaml
firewall:
  rules:
    - direction: 'in'
      port: '22'
      protocol: 'tcp'
      source_ips: ['current']  # Replaced with your IP automatically
      description: 'SSH from my IP'
```

## Common Commands

```bash
# Validate config before deploying
harbor validate

# Redeploy app services with zero downtime (blue-green deployment)
harbor redeploy                # Redeploy all services
harbor redeploy nginx          # Redeploy specific service
harbor redeploy --yes          # Skip confirmation

# Scale servers manually
harbor scale lb 5              # Scale load balancers to 5
harbor scale app 10            # Scale app servers to 10

# Restart services with latest configuration
harbor restart k6              # Restart k6 (syncs script, applies config)
harbor restart grafana         # Restart Grafana (syncs dashboards, config)

# Show infrastructure status
harbor status
```

## Troubleshooting

### "Failed to connect via SSH"

Wait a bit longer for servers to boot:

```bash
# Check server status
harbor status

# Flatcar servers need 1-2 minutes to fully boot
```

### "docker-compose installation failed"

SSH to server and check:

```bash
ssh -i .harbor/ssh/your-key core@<server-ip>
/opt/bin/docker-compose version
```

### "APISIX not responding"

Check containers:

```bash
# SSH to control plane (Flatcar uses 'core' user)
ssh -i .harbor/ssh/your-key core@<control-plane-ip>
docker ps -a
docker logs apisix-control-plane
docker logs etcd
```

## Next Steps

1. **Access Grafana**: Open `http://<control-plane-ip>:3000` (admin/admin) to view pre-configured dashboards
2. **Add SSL Certificates**: Configure HTTPS for production
3. **Configure Custom Routes**: Add more APISIX routes (including Grafana domain routing)
4. **Deploy Your App**: Replace nginx with your application
5. **Tune Autoscaler**: Adjust CPU/memory thresholds in `harbor.yaml`
6. **Enable Load Testing**: Set `k6.enabled: true` in `harbor.yaml` to continuously test your load balancers
7. **Set Up CI/CD**: Automate deployments

## Support

- GitHub Issues: https://github.com/dihmeetree/harbor/issues
- Documentation: See DEPLOYMENT.md for detailed guide
- Examples: See configs/harbor.example.yaml
