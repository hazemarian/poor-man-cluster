package server

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/hazemarian/poor-man-stack/pmcluster/internal/credentials"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/store"
)

// WebhookService exposes deploy-webhook source management over the
// Bearer-protected /api router. The shared HMAC secret is generated on the
// daemon, encrypted at rest, and returned to the caller ONCE on creation —
// listing a source never reveals its secret (the operator edge UI shows the
// secret in the create response only).
type WebhookService struct {
	Store  *store.Store
	Cipher *credentials.Cipher
}

func (w *WebhookService) Mount(r chi.Router) {
	r.Get("/webhooks", w.list)
	r.Post("/webhooks", w.create)
	r.Delete("/webhooks/{source}", w.remove)
}

type webhookRow struct {
	Source      string `json:"source"`
	Description string `json:"description,omitempty"`
	CreatedAt   int64  `json:"created_at"`
	LastUsedAt  int64  `json:"last_used_at,omitempty"`
}

func (w *WebhookService) list(res http.ResponseWriter, req *http.Request) {
	srcs, err := w.Store.ListWebhookSources(req.Context())
	if err != nil {
		writeErr(res, http.StatusInternalServerError, "list webhook sources: "+err.Error())
		return
	}
	rows := make([]webhookRow, 0, len(srcs))
	for _, s := range srcs {
		row := webhookRow{Source: s.Source, CreatedAt: s.CreatedAt}
		if s.Description.Valid {
			row.Description = s.Description.String
		}
		if s.LastUsedAt.Valid {
			row.LastUsedAt = s.LastUsedAt.Int64
		}
		rows = append(rows, row)
	}
	writeJSON(res, http.StatusOK, map[string]any{"webhooks": rows})
}

type createWebhookRequest struct {
	Source      string `json:"source"`
	Description string `json:"description,omitempty"`
}

func (w *WebhookService) create(res http.ResponseWriter, req *http.Request) {
	var body createWebhookRequest
	dec := json.NewDecoder(http.MaxBytesReader(res, req.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		writeErr(res, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	source := strings.TrimSpace(body.Source)
	if source == "" {
		writeErr(res, http.StatusBadRequest, "source is required")
		return
	}

	// 32 bytes → 64 hex chars, matching the CLI so CI can paste the printed
	// value verbatim. Stored as hex *string* and HMAC'd with that same string.
	secretBytes := make([]byte, 32)
	if _, err := rand.Read(secretBytes); err != nil {
		writeErr(res, http.StatusInternalServerError, "generate secret: "+err.Error())
		return
	}
	secretHex := hex.EncodeToString(secretBytes)

	ciphertext, err := w.Cipher.Encrypt([]byte(secretHex))
	if err != nil {
		writeErr(res, http.StatusInternalServerError, "encrypt secret: "+err.Error())
		return
	}
	if err := w.Store.CreateWebhookSource(req.Context(), source, body.Description, ciphertext); err != nil {
		if errors.Is(err, store.ErrWebhookSourceExists) {
			writeErr(res, http.StatusConflict, "webhook source already exists: "+source)
			return
		}
		writeErr(res, http.StatusInternalServerError, "create webhook source: "+err.Error())
		return
	}

	writeJSON(res, http.StatusCreated, map[string]any{"source": source, "secret": secretHex})
}

func (w *WebhookService) remove(res http.ResponseWriter, req *http.Request) {
	source := chi.URLParam(req, "source")
	if source == "" {
		writeErr(res, http.StatusBadRequest, "source is required")
		return
	}
	if err := w.Store.DeleteWebhookSource(req.Context(), source); err != nil {
		if errors.Is(err, store.ErrWebhookSourceNotFound) {
			writeErr(res, http.StatusNotFound, "webhook source not found: "+source)
			return
		}
		writeErr(res, http.StatusInternalServerError, "delete webhook source: "+err.Error())
		return
	}
	res.WriteHeader(http.StatusNoContent)
}
