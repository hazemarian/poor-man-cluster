package cluster

import (
	"context"
	"fmt"

	"github.com/hazemarian/poor-man-stack/pmcluster/internal/runtime"
)

type StatusReport struct {
	Preflight     error
	NodeName      string
	ServerVersion string
	SwarmState    string
	IsManager     bool
	NodeCount     int
	ManagerCount  int
}

// Status runs Preflight (read-only) and returns a snapshot.
func Status(ctx context.Context, d runtime.Client) (*StatusReport, error) {
	preflight := Preflight(ctx, d)

	info, infoErr := d.Info(ctx)
	if infoErr != nil {
		return nil, fmt.Errorf("docker info: %w", infoErr)
	}
	return &StatusReport{
		Preflight:     preflight,
		NodeName:      info.Name,
		ServerVersion: info.ServerVersion,
		SwarmState:    info.SwarmLocalNodeState,
		IsManager:     info.SwarmControlAvailable,
		NodeCount:     info.SwarmNodes,
		ManagerCount:  info.SwarmManagers,
	}, nil
}
