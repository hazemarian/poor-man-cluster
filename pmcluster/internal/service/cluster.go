// Package service is the port shim of the pmcluster domain.
//
// The package is being dissolved into per-domain packages; these aliases keep
// existing consumers compiling while the migration is in progress. Each
// interface's real home is its domain package (internal/cluster,
// internal/apikeys, ...). No implementation lives here.
package service

import "github.com/hazemarian/poor-man-stack/pmcluster/internal/cluster"

// ClusterService is the platform-lifecycle port (see cluster.Service).
type ClusterService = cluster.Service
