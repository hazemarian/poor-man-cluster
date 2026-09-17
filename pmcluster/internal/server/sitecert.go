package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/hazemarian/poor-man-stack/pmcluster/internal/service"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/store"
)

// SiteCertService exposes management of the cluster's OWN (main-domain) TLS
// certificate over the Bearer-protected /api router — the same flow as
// per-host certs, but for the cluster's domain (e.g. nextrum-sy.com).
//
// The cert/key arrive as TEXT (the operator or edge UI pastes the PEM). The
// service validates the pair against the persisted domain, writes the local
// copy under <configDir>/site/, re-materializes the versioned Swarm secrets,
// refreshes Traefik and persists the metadata for expiry monitoring.
type SiteCertService struct {
	Svc service.TLSService
}

func (s *SiteCertService) Mount(r chi.Router) {
	r.Get("/tls/site", s.get)
	r.Put("/tls/site", s.put)
}

func (s *SiteCertService) get(w http.ResponseWriter, r *http.Request) {
	if s.Svc == nil {
		writeErr(w, http.StatusInternalServerError, "site-cert service not wired")
		return
	}
	domain, err := s.Svc.MainDomain(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "resolve cluster domain: "+err.Error())
		return
	}
	if domain == "" {
		writeErr(w, http.StatusNotFound, "no persisted cluster domain found — run `pmcluster cluster up` first")
		return
	}
	row, err := s.Svc.GetSiteCert(r.Context(), domain)
	if err != nil {
		if errors.Is(err, store.ErrSiteCertNotFound) {
			writeErr(w, http.StatusNotFound, "no site certificate stored for "+domain)
			return
		}
		writeErr(w, http.StatusInternalServerError, "get site cert: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, siteCertResponseFromRow(row))
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
	if s.Svc == nil {
		writeErr(w, http.StatusInternalServerError, "site-cert service not wired")
		return
	}
	domain, err := s.Svc.MainDomain(r.Context())
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
	row, err := s.Svc.SiteCert(r.Context(), domain, req.Cert, req.Key)
	if err != nil {
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
