# Harbor Quick Start Guide

## Prerequisites

- Hetzner Cloud account with API token
- Go 1.21+ installed (for building from source)

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

### 1. Initialize Configuration

```bash
harbor init
```

This creates `harbor.yaml` from the example template.

### 2. Edit Configuration

Edit `harbor.yaml` to customize your deployment:

```yaml
provider: hetzner

server:
  name: 'my-app'          # Change this to your app name
  type: 'ccx13'           # Server type
  location: 'ash'         # Hetzner location
  image: 'ubuntu-24.04'

# ... rest of config
```

### 3. Set API Token

```bash
export HETZNER_API_TOKEN="your-hetzner-api-token-here"
```

### 4. Deploy

```bash
harbor deploy
```

This will:
- ✅ Create infrastructure (networks, firewalls, servers)
- ✅ Install Docker on all servers
- ✅ Deploy APISIX control plane + etcd + Prometheus + Grafana + Autoscaler
- ✅ Deploy APISIX data planes
- ✅ Deploy your app
- ✅ Configure APISIX routes and upstreams
- ⏱️ Total time: 15-30 minutes

### 5. Check Status

```bash
harbor status
```

### 6. Test Your Deployment

```bash
# Get data plane IP from status command
DATA_PLANE_IP="<from-status-output>"

# Test HTTP endpoint
curl http://$DATA_PLANE_IP/

# Check Prometheus metrics
curl http://$DATA_PLANE_IP:9091/apisix/prometheus/metrics
```

### 7. Access Services

- **Your App**: `http://<data-plane-ip>`
- **APISIX Admin API**: `http://<control-plane-ip>:9180`
- **Prometheus**: `http://<control-plane-ip>:9090`
- **Grafana**: `http://<control-plane-ip>:3000` (default login: admin/admin)

### 8. Destroy (When Done)

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

# Show deployment logs
harbor logs --follow

# Scale data planes (coming soon)
harbor scale --data-planes 5

# SSH into a server (coming soon)
harbor ssh control-plane

# Update APISIX routes (coming soon)
harbor apisix routes list
harbor apisix routes create
```

## Troubleshooting

### "Failed to connect via SSH"

Wait a bit longer for servers to boot:

```bash
# Check server status on Hetzner dashboard
# Or wait and retry
```

### "Docker installation failed"

SSH to server and check:

```bash
ssh -i .harbor/ssh/your-key root@<server-ip>
systemctl status docker
journalctl -u docker
```

### "APISIX not responding"

Check containers:

```bash
ssh root@<control-plane-ip>
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
6. **Set Up CI/CD**: Automate deployments

## Support

- GitHub Issues: https://github.com/dihmeetree/harbor/issues
- Documentation: See DEPLOYMENT.md for detailed guide
- Examples: See configs/harbor.example.yaml
