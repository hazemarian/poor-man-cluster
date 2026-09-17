// Package service is the compatibility shim for the domain ports.
// CredentialsService now lives in internal/cluster beside CredentialsManager;
// this alias keeps older consumers compiling during the migration.
package service

import "github.com/hazemarian/poor-man-stack/pmcluster/internal/cluster"

// CredentialsService is the bootstrap-credentials port (see
// cluster.CredentialsService).
type CredentialsService = cluster.CredentialsService
