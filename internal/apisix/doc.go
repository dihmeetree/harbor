// Package apisix provides a client for the Apache APISIX Admin API.
//
// This package enables programmatic configuration of APISIX, including:
//   - Creating and updating upstreams with health checks
//   - Defining routes and their matching rules
//   - Configuring global rules and plugins
//   - Managing SSL/TLS certificates
//
// The client supports both direct HTTP connections and SSH-tunneled connections,
// allowing Harbor to configure APISIX remotely via the control plane server.
package apisix
