package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/hazemarian/poor-man-stack/pmcluster/internal/cluster"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/store"
)

// SiteCertService exposes management of the cluster's OWN (main-domain) TLS
// certificate over the Bearer-protected /api router — the same flow as
// per-host certs, but for the cluster's domain (e.g. nextrum-sy.com).
//
// The cert/key arrive as TEXT (the operator or edge UI pastes the PEM). The
// Apply function (wired in serve.go from cluster.ApplySiteCert) validates the
// pair against the persisted domain, writes the local copy under
// <configDir>/site/, re-materializes the versioned Swarm secrets, refreshes
// Traefik and persists the metadata for expiry monitoring.
type SiteCertService struct {
	Store *store.Store // required: domain lookup + metadata persistence

	// Apply uploads a new site cert for domain and returns the stored metadata
	// row. It is the single implementation behind PUT /api/tls/site and the
	// CLI; the daemon only knows its signature.
	Apply func(ctx context.Context, domain, certPEM, keyPEM string) (*store.SiteCertRow, error)
}

func (s *SiteCertService) Mount(r chi.Router) {
	r.Get("/tls/site", s.get)
	r.Put("/tls/site", s.put)
}

func (s *SiteCertService) get(w http.ResponseWriter, r *http.Request) {
	if s.Store == nil {
		writeErr(w, http.StatusInternalServerError, "store unavailable")
		return
	}
	domain := cluster.PersistedDomain(r.Context(), s.Store)
	if domain == "" {
		writeErr(w, http.StatusNotFound, "no persisted cluster domain found — run `pmcluster cluster up` first")
		return
	}
	row, err := s.Store.GetSiteCert(r.Context(), domain)
	if err != nil {
		if errors.Is(err, store.ErrSiteCertNotFound) {
			writeErr(w, http.StatusNotFound, "no site certificate stored for "+domain)
			return
		}
		writeErr(w, http.StatusInternalServerError, "get site cert: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, siteCertResponseFromRow(&row))
}

type putSiteCertRequest struct {
	Cert string `json:"cert"`
	Key  string `json:"key"`
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

func (s *SiteCertService) put(w http.ResponseWriter, r *http.Request) {
	if s.Store == nil || s.Apply == nil {
		writeErr(w, http.StatusInternalServerError, "site-cert service not fully wired")
		return
	}
	domain := cluster.PersistedDomain(r.Context(), s.Store)
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
	row, err := s.Apply(r.Context(), domain, req.Cert, req.Key)
	if err != nil {
		// Validation failures (bad PEM, mismatch, domain not covered) are
		// client errors; refresh failures are infrastructure errors.
		status := http.StatusInternalServerError
		if strings.Contains(err.Error(), "does not cover") || strings.Contains(err.Error(), "valid pair") || strings.Contains(err.Error(), "PEM") {
			status = http.StatusBadRequest
		}
		writeErr(w, status, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, siteCertResponseFromRow(row))
}

// siteCertResponseFromRow converts a stored row into its JSON shape.
func siteCertResponseFromRow(row *store.SiteCertRow) siteCertResponse {
	return siteCertResponse{
		Domain:     row.Domain,
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
