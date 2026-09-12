package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/hazemarian/poor-man-stack/pmcluster/internal/cluster/tlscerts"
)

// HostCertService exposes per-host TLS certificate management over the
// Bearer-protected /api router. Certs + keys arrive as TEXT (the operator or
// edge UI pastes the PEM), never as a file upload; pmcluster writes them to
// disk and refreshes Traefik so it serves them for the matching SNI.
type HostCertService struct {
	Manager *tlscerts.Manager // required

	// Refresh re-renders + re-deploys Traefik after a write. Optional; when
	// nil the store changes take effect on the next `pmcluster cluster update`.
	Refresh func(ctx context.Context) error
}

func (h *HostCertService) Mount(r chi.Router) {
	r.Get("/tls/hosts", h.list)
	r.Put("/tls/hosts/{host}", h.put)
	r.Delete("/tls/hosts/{host}", h.remove)
}

func (h *HostCertService) list(w http.ResponseWriter, r *http.Request) {
	hosts, err := h.Manager.List(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "list host certs: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"hosts": hosts})
}

type putHostCertRequest struct {
	Cert string `json:"cert"`
	Key  string `json:"key"`
}

func (h *HostCertService) put(w http.ResponseWriter, r *http.Request) {
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
	got, err := h.Manager.Put(r.Context(), host, req.Cert, req.Key)
	if err != nil {
		// Validation failures (bad PEM, mismatch, traversal, unsupported host)
		// are client errors.
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := h.refresh(w, r); err != nil {
		return
	}
	writeJSON(w, http.StatusOK, got)
}

func (h *HostCertService) remove(w http.ResponseWriter, r *http.Request) {
	host := chi.URLParam(r, "host")
	if host == "" {
		writeErr(w, http.StatusBadRequest, "host is required")
		return
	}
	removed, err := h.Manager.Remove(r.Context(), host)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "remove host cert: "+err.Error())
		return
	}
	if !removed {
		writeErr(w, http.StatusNotFound, "no cert stored for host "+host)
		return
	}
	if err := h.refresh(w, r); err != nil {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// refresh runs the optional Traefik refresh and reports a 503 with a clear
// message when it fails (leaving the on-disk change in place for retry).
func (h *HostCertService) refresh(w http.ResponseWriter, r *http.Request) error {
	if h.Refresh == nil {
		return nil
	}
	if err := h.Refresh(r.Context()); err != nil {
		writeErr(w, http.StatusServiceUnavailable, "cert stored but Traefik refresh failed: "+err.Error())
		return err
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"error": msg})
}
