// Package database provides SQLite-based state persistence for Harbor infrastructure.
//
// This package manages the local SQLite database that tracks all deployed infrastructure
// including networks, servers, firewalls, SSH keys, and deployment history. It uses
// embedded migrations for schema management and provides repository patterns for
// data access.
//
// The database serves as Harbor's single source of truth for infrastructure state,
// enabling operations like status queries, redeployments, and cleanup even if cloud
// provider state has diverged.
package database
