package certs

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
)

// HTTP exposes cert management over the Bearer-protected /api router. Certs +
// keys arrive as TEXT (the operator or edge UI pastes the PEM), never as a
// file upload. The handler only knows the Service port; the implementation
// (local core or a remote adapter) is chosen by the caller.
type HTTP struct {
	Svc Service
}

// MountHosts registers the per-host certificate routes. Per-host certs use
// the SAME table and flow as the cluster's own certificate (site_certs +
// versioned Swarm secrets). The list endpoint returns every stored row EXCEPT
// the cluster's own domain (that one is served by MountSite).
func (h *HTTP) MountHosts(r chi.Router) {
	r.Get("/tls/hosts", h.listHosts)
	r.Put("/tls/hosts/{host}", h.putHost)
	r.Delete("/tls/hosts/{host}", h.removeHost)
}

// MountSite registers the cluster's OWN (main-domain) certificate routes —
// the same flow as per-host certs, but for the cluster's domain.
func (h *HTTP) MountSite(r chi.Router) {
	r.Get("/tls/site", h.getSite)
	r.Put("/tls/site", h.putSite)
}

type putHostCertRequest struct {
	Cert string `json:"cert"`
	Key  string `json:"key"`
}

type putSiteCertRequest struct {
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

// siteCertResponse is the JSON shape for the stored site-cert metadata. Times
// serialize as RFC3339; SANs as a string slice.
type siteCertResponse struct {
	Domain     string   `json:"domain"`
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

func (h *HTTP) listHosts(w http.ResponseWriter, r *http.Request) {
	if h.Svc == nil {
		writeErr(w, http.StatusInternalServerError, "host-cert service not wired")
		return
	}
	rows, err := h.Svc.List(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "list host certs: "+err.Error())
		return
	}
	main, err := h.Svc.MainDomain(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "resolve cluster domain: "+err.Error())
		return
	}
	hosts := make([]hostCertResponse, 0, len(rows))
	for i := range rows {
		if rows[i].Domain == main {
			continue
		}
		hosts = append(hosts, hostCertResponseFromCert(&rows[i]))
	}
	writeJSON(w, http.StatusOK, map[string]any{"hosts": hosts})
}

func (h *HTTP) putHost(w http.ResponseWriter, r *http.Request) {
	if h.Svc == nil {
		writeErr(w, http.StatusInternalServerError, "host-cert service not wired")
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
	got, err := h.Svc.ApplyHostCert(r.Context(), host, req.Cert, req.Key, true)
	if err != nil {
		status := http.StatusInternalServerError
		if strings.Contains(err.Error(), "does not cover") || strings.Contains(err.Error(), "valid pair") ||
			strings.Contains(err.Error(), "PEM") || strings.Contains(err.Error(), "invalid host") {
			status = http.StatusBadRequest
		}
		writeErr(w, status, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, hostCertResponseFromCert(got))
}

func (h *HTTP) removeHost(w http.ResponseWriter, r *http.Request) {
	if h.Svc == nil {
		writeErr(w, http.StatusInternalServerError, "host-cert service not wired")
		return
	}
	host := chi.URLParam(r, "host")
	if host == "" {
		writeErr(w, http.StatusBadRequest, "host is required")
		return
	}
	if err := h.Svc.RemoveHostCert(r.Context(), host, true); err != nil {
		if errors.Is(err, store.ErrSiteCertNotFound) {
			writeErr(w, http.StatusNotFound, "no cert stored for host "+host)
			return
		}
		writeErr(w, http.StatusInternalServerError, "remove host cert: "+err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *HTTP) getSite(w http.ResponseWriter, r *http.Request) {
	if h.Svc == nil {
		writeErr(w, http.StatusInternalServerError, "site-cert service not wired")
		return
	}
	domain, err := h.Svc.MainDomain(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "resolve cluster domain: "+err.Error())
		return
	}
	if domain == "" {
		writeErr(w, http.StatusNotFound, "no persisted cluster domain found — run `pmcluster cluster up` first")
		return
	}
	row, err := h.Svc.GetSiteCert(r.Context(), domain)
	if err != nil {
		if errors.Is(err, store.ErrSiteCertNotFound) {
			writeErr(w, http.StatusNotFound, "no site certificate stored for "+domain)
			return
		}
		writeErr(w, http.StatusInternalServerError, "get site cert: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, siteCertResponseFromCert(row))
}

func (h *HTTP) putSite(w http.ResponseWriter, r *http.Request) {
	if h.Svc == nil {
		writeErr(w, http.StatusInternalServerError, "site-cert service not wired")
		return
	}
	domain, err := h.Svc.MainDomain(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "resolve cluster domain: "+err.Error())
		return
	}
	if domain == "" {
		writeErr(w, http.StatusNotFound, "no persisted cluster domain found — run `pmcluster cluster up` first")
		return
	}

	var req putSiteCertRequest
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
	row, err := h.Svc.SiteCert(r.Context(), domain, req.Cert, req.Key)
	if err != nil {
		status := http.StatusInternalServerError
		if strings.Contains(err.Error(), "does not cover") || strings.Contains(err.Error(), "valid pair") || strings.Contains(err.Error(), "PEM") {
			status = http.StatusBadRequest
		}
		writeErr(w, status, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, siteCertResponseFromCert(row))
}

// hostCertResponseFromCert converts a stored Cert (domain == host) into its
// JSON shape.
func hostCertResponseFromCert(c *Cert) hostCertResponse {
	return hostCertResponse{
		Host:       c.Domain,
		CertSecret: c.CertSecret,
		KeySecret:  c.KeySecret,
		NotBefore:  rfTime(c.NotBefore),
		NotAfter:   rfTime(c.NotAfter),
		SANs:       c.SANs,
		CertHash:   c.CertHash,
		KeyHash:    c.KeyHash,
		CreatedAt:  rfTime(c.CreatedAt),
		UpdatedAt:  rfTime(c.UpdatedAt),
	}
}

// siteCertResponseFromCert converts a stored Cert into its JSON shape.
func siteCertResponseFromCert(c *Cert) siteCertResponse {
	return siteCertResponse{
		Domain:     c.Domain,
		CertSecret: c.CertSecret,
		KeySecret:  c.KeySecret,
		NotBefore:  rfTime(c.NotBefore),
		NotAfter:   rfTime(c.NotAfter),
		SANs:       c.SANs,
		CertHash:   c.CertHash,
		KeyHash:    c.KeyHash,
		CreatedAt:  rfTime(c.CreatedAt),
		UpdatedAt:  rfTime(c.UpdatedAt),
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
