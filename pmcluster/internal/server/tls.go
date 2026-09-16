package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/hazemarian/poor-man-stack/pmcluster/internal/cluster"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/store"
)

// HostCertService exposes per-host (bring-your-own) TLS certificate
// management over the Bearer-protected /api router. Certs + keys arrive as
// TEXT (the operator or edge UI pastes the PEM), never as a file upload.
//
// Per-host certs use the SAME table and flow as the cluster's own
// certificate (site_certs + versioned Swarm secrets): Apply validates the
// pair, materializes hostcert-<host>_vNNN / hostkey-<host>_vNNN secrets and
// refreshes Traefik. The list endpoint returns every stored row EXCEPT the
// cluster's own domain (that one is served by /api/tls/site).
type HostCertService struct {
	Store *store.Store

	// Apply uploads a per-host cert for host and returns the stored metadata
	// row. It is the single implementation behind PUT /api/tls/hosts/{host}
	// and the CLI; the daemon only knows its signature.
	Apply func(ctx context.Context, host, certPEM, keyPEM string) (*store.SiteCertRow, error)

	// Remove deletes the per-host cert (metadata + secrets) and refreshes
	// Traefik. Returns store.ErrSiteCertNotFound when nothing is stored.
	Remove func(ctx context.Context, host string) error
}

func (h *HostCertService) Mount(r chi.Router) {
	r.Get("/tls/hosts", h.list)
	r.Put("/tls/hosts/{host}", h.put)
	r.Delete("/tls/hosts/{host}", h.remove)
}

func (h *HostCertService) list(w http.ResponseWriter, r *http.Request) {
	if h.Store == nil {
		writeErr(w, http.StatusInternalServerError, "store unavailable")
		return
	}
	rows, err := h.Store.ListSiteCerts(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "list host certs: "+err.Error())
		return
	}
	main := cluster.PersistedDomain(r.Context(), h.Store)
	hosts := make([]hostCertResponse, 0, len(rows))
	for _, row := range rows {
		if row.Domain == main {
			continue
		}
		hosts = append(hosts, hostCertResponseFromRow(&row))
	}
	writeJSON(w, http.StatusOK, map[string]any{"hosts": hosts})
}

type putHostCertRequest struct {
	Cert string `json:"cert"`
	Key  string `json:"key"`
}

// hostCertResponse is the JSON shape for one stored per-host cert metadata
// row. Times serialize as RFC3339; SANs as a string slice.
type hostCertResponse struct {
	Host       string   `json:"host"`
	CertSecret string   `json:"cert_secret,omitempty"`
	KeySecret  string   `json:"key_secret,omitempty"`
	NotBefore  string   `json:"not_before,omitempty"`
	NotAfter   string   `json:"not_after,omitempty"`
	SANs       []string `json:"sans,omitempty"`
	CertHash   string   `json:"cert_hash,omitempty"`
	KeyHash    string   `json:"key_hash,omitempty"`
	CreatedAt  string   `json:"created_at,omitempty"`
	UpdatedAt  string   `json:"updated_at,omitempty"`
}

func (h *HostCertService) put(w http.ResponseWriter, r *http.Request) {
	if h.Store == nil || h.Apply == nil {
		writeErr(w, http.StatusInternalServerError, "host-cert service not fully wired")
		return
	}
	host := chi.URLParam(r, "host")
	if host == "" {
		writeErr(w, http.StatusBadRequest, "host is required")
		return
	}
	var req putHostCertRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	if strings.TrimSpace(req.Cert) == "" || strings.TrimSpace(req.Key) == "" {
		writeErr(w, http.StatusBadRequest, "cert and key are both required")
		return
	}
	got, err := h.Apply(r.Context(), host, req.Cert, req.Key)
	if err != nil {

		status := http.StatusInternalServerError
		if strings.Contains(err.Error(), "does not cover") || strings.Contains(err.Error(), "valid pair") ||
			strings.Contains(err.Error(), "PEM") || strings.Contains(err.Error(), "invalid host") {
			status = http.StatusBadRequest
		}
		writeErr(w, status, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, hostCertResponseFromRow(got))
}

func (h *HostCertService) remove(w http.ResponseWriter, r *http.Request) {
	if h.Store == nil || h.Remove == nil {
		writeErr(w, http.StatusInternalServerError, "host-cert service not fully wired")
		return
	}
	host := chi.URLParam(r, "host")
	if host == "" {
		writeErr(w, http.StatusBadRequest, "host is required")
		return
	}
	if err := h.Remove(r.Context(), host); err != nil {
		if errors.Is(err, store.ErrSiteCertNotFound) {
			writeErr(w, http.StatusNotFound, "no cert stored for host "+host)
			return
		}
		writeErr(w, http.StatusInternalServerError, "remove host cert: "+err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// hostCertResponseFromRow converts a stored site_certs row (domain == host)
// into its JSON shape.
func hostCertResponseFromRow(row *store.SiteCertRow) hostCertResponse {
	return hostCertResponse{
		Host:       row.Domain,
		CertSecret: row.CertSecret,
		KeySecret:  row.KeySecret,
		NotBefore:  rfTime(row.NotBefore),
		NotAfter:   rfTime(row.NotAfter),
		SANs:       row.SANs,
		CertHash:   row.CertHash,
		KeyHash:    row.KeyHash,
		CreatedAt:  rfTime(row.CreatedAt),
		UpdatedAt:  rfTime(row.UpdatedAt),
	}
}

// rfTime formats a time as RFC3339 UTC, or "" when zero.
func rfTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format("2006-01-02T15:04:05Z")
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"error": msg})
}
