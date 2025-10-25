// Package autoscaler provides automatic horizontal scaling based on Prometheus metrics.
//
// The autoscaler monitors CPU and memory usage across server pools (load balancers and app servers)
// and automatically scales infrastructure up or down based on configured thresholds. It integrates
// with the Hetzner Cloud API for server provisioning, APISIX for load balancer configuration,
// and Prometheus for metrics collection.
//
// Key features:
//   - Automatic scale-up when CPU or memory exceeds high thresholds
//   - Automatic scale-down when both CPU and memory are below low thresholds
//   - Configurable cooldown periods to prevent scaling oscillation
//   - Min/max replica constraints
//   - Automatic updates to APISIX upstreams, Prometheus targets, and k6 load test configuration
//
// The autoscaler runs as a standalone daemon container on the control plane server.
package autoscaler
