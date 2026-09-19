package server

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/hazemarian/poor-man-stack/pmcluster/internal/cluster"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/store"
)

// clusterSettingsHTTP exposes the editable cluster_settings keys under
// /api/cluster/settings. PUTs only persist to the store — they do NOT
// redeploy; `cluster update` applies them (mirroring existing settings
// behaviour).
type clusterSettingsHTTP struct {
	Store *store.Store
}

// clusterSettingKeys is the fixed allowlist of editable settings surfaced to
// the console. TLS install state (mode/cert/key/acme) is deliberately excluded
// — those are managed by the setup wizard, not edited here.
var clusterSettingKeys = []string{
	cluster.SettingVolumeRoot(),
	cluster.SettingBackupAllNodes(),
	cluster.SettingSSOEnabled(),
	cluster.SettingSSOProvider(),
	cluster.SettingSSOClientID(),
	cluster.SettingSSOClientSecret(),
	cluster.SettingSSOGitHubOrg(),
	cluster.SettingSSOCookieExpire(),
	cluster.SettingEdgeLoginDisabled(),
	cluster.SettingDomain(),
	cluster.SettingOOEmail(),
	cluster.SettingTraefikAdminUser(),
}

func clusterSettingAllowed() map[string]bool {
	m := make(map[string]bool, len(clusterSettingKeys))
	for _, k := range clusterSettingKeys {
		m[k] = true
	}
	return m
}

func (h *clusterSettingsHTTP) Mount(r chi.Router) {
	r.Get("/cluster/settings", h.get)
	r.Put("/cluster/settings", h.put)
}

// read returns the current value of every known setting ("" when unset).
func (h *clusterSettingsHTTP) read(r *http.Request) map[string]string {
	settings := make(map[string]string, len(clusterSettingKeys))
	for _, k := range clusterSettingKeys {
		settings[k] = h.Store.GetSettingDefault(r.Context(), k, "")
	}
	return settings
}

func (h *clusterSettingsHTTP) get(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"settings": h.read(r)})
}

func (h *clusterSettingsHTTP) put(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Settings map[string]string `json:"settings"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	if len(body.Settings) == 0 {
		writeErr(w, http.StatusBadRequest, "settings: required")
		return
	}

	allowed := clusterSettingAllowed()
	for k := range body.Settings {
		if !allowed[k] {
			writeErr(w, http.StatusBadRequest, "unknown setting: "+k)
			return
		}
	}
	for k, v := range body.Settings {
		if err := h.Store.SetSetting(r.Context(), k, v); err != nil {
			writeErr(w, http.StatusInternalServerError, "set setting "+k+": "+err.Error())
			return
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{"settings": h.read(r)})
}
