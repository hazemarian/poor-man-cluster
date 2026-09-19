// Package settings is the domain for the editable cluster settings surface
// (GET/PUT /api/cluster/settings). It exposes the fixed allowlist of settings
// the console can edit; everything else (TLS install state, credential
// secrets) stays under the setup wizard.
//
// Architecture: model + port here, local adapter in local.go (store-backed),
// HTTP handler in http.go, remote adapter in internal/remote/settings.go.
package settings

import (
	"context"
)

// Settings is a snapshot of the editable cluster settings keys.
type Settings map[string]string

// Service is the port for reading and updating editable cluster settings.
// Update persists keys only — it never redeploys; `cluster update` applies
// them.
type Service interface {
	// Get returns the current value of every editable setting ("" when unset).
	Get(ctx context.Context) (Settings, error)
	// Update persists the given settings and returns the updated snapshot.
	// Unknown keys are rejected.
	Update(ctx context.Context, values Settings) (Settings, error)
}
