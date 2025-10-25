// Package orchestrator provides infrastructure provisioning and service deployment orchestration.
//
// This package contains the core orchestration logic for Harbor, managing the complete lifecycle
// of infrastructure deployment on cloud providers. It includes:
//
//   - Provisioner: Creates infrastructure (networks, firewalls, servers) on Hetzner Cloud
//   - Deployer: Deploys and manages services (APISIX, etcd, Prometheus, Grafana, k6, user applications)
//   - ManualScaler: Handles CLI-initiated horizontal scaling of load balancers and app servers
//
// The orchestrator coordinates between cloud provider APIs, SSH connections, Docker deployments,
// and service configuration to provide a seamless infrastructure management experience.
package orchestrator
