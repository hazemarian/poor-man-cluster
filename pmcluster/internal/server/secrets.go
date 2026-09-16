package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/hazemarian/poor-man-stack/pmcluster/internal/credentials"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/store"
)

// SecretService exposes DB-backed secret management over the Bearer-protected
// /api router. The plaintext value is encrypted (AES-GCM) at rest via Cipher
// and only ever appears in the create request body; every other read returns
// the name/scope/hash metadata without revealing the payload.
type SecretService struct {
	Store  *store.Store
	Cipher *credentials.Cipher
}

func (s *SecretService) Mount(r chi.Router) {
	r.Get("/secrets", s.list)
	r.Post("/secrets", s.create)
	r.Delete("/secrets/{name}", s.remove)
	r.Get("/secrets/{name}/value", s.value)
}

type secretRow struct {
	ID        int64  `json:"id"`
	Scope     string `json:"scope"`
	Name      string `json:"name"`
	Hash      string `json:"hash"`
	CreatedAt int64  `json:"created_at"`
}

func (s *SecretService) list(res http.ResponseWriter, req *http.Request) {
	secs, err := s.Store.ListSecrets(req.Context())
	if err != nil {
		writeErr(res, http.StatusInternalServerError, "list secrets: "+err.Error())
		return
	}
	rows := make([]secretRow, 0, len(secs))
	for _, x := range secs {
		rows = append(rows, secretRow{ID: x.ID, Scope: x.Scope, Name: x.Name, Hash: x.Hash, CreatedAt: x.CreatedAt})
	}
	writeJSON(res, http.StatusOK, map[string]any{"secrets": rows})
}

type createSecretRequest struct {
	Name  string `json:"name"`
	Scope string `json:"scope"`
	Value string `json:"value"`
}

func (s *SecretService) create(res http.ResponseWriter, req *http.Request) {
	var body createSecretRequest
	dec := json.NewDecoder(http.MaxBytesReader(res, req.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		writeErr(res, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	name := strings.TrimSpace(body.Name)
	scope := strings.TrimSpace(body.Scope)
	if name == "" {
		writeErr(res, http.StatusBadRequest, "name is required")
		return
	}
	if scope == "" {
		scope = "service"
	}
	if scope != "cluster" && scope != "service" {
		writeErr(res, http.StatusBadRequest, "scope must be 'cluster' or 'service'")
		return
	}
	if body.Value == "" {
		writeErr(res, http.StatusBadRequest, "value is required")
		return
	}

	payload, err := s.Cipher.Encrypt([]byte(body.Value))
	if err != nil {
		writeErr(res, http.StatusInternalServerError, "encrypt secret: "+err.Error())
		return
	}
	id, err := s.Store.CreateSecret(req.Context(), scope, name, payload, store.SecretHash(body.Value))
	if err != nil {
		if errors.Is(err, store.ErrSecretExists) {
			writeErr(res, http.StatusConflict, "secret already exists: "+name)
			return
		}
		writeErr(res, http.StatusInternalServerError, "create secret: "+err.Error())
		return
	}
	writeJSON(res, http.StatusCreated, map[string]any{
		"id": id, "scope": scope, "name": name, "hash": store.SecretHash(body.Value),
	})
}

// remove deletes a secret by name.
func (s *SecretService) remove(res http.ResponseWriter, req *http.Request) {
	name := chi.URLParam(req, "name")
	if name == "" {
		writeErr(res, http.StatusBadRequest, "secret name is required")
		return
	}
	if err := s.Store.DeleteSecret(req.Context(), name); err != nil {
		if errors.Is(err, store.ErrSecretNotFound) {
			writeErr(res, http.StatusNotFound, "secret not found: "+name)
			return
		}
		writeErr(res, http.StatusInternalServerError, "delete secret: "+err.Error())
		return
	}
	res.WriteHeader(http.StatusNoContent)
}

// value decrypts and returns the plaintext of a stored secret. Only the
// authenticated operator can read it; the value is never cached server-side.
func (s *SecretService) value(res http.ResponseWriter, req *http.Request) {
	name := chi.URLParam(req, "name")
	if name == "" {
		writeErr(res, http.StatusBadRequest, "secret name is required")
		return
	}
	sec, err := s.Store.GetSecret(req.Context(), name)
	if err != nil {
		if errors.Is(err, store.ErrSecretNotFound) {
			writeErr(res, http.StatusNotFound, "secret not found: "+name)
			return
		}
		writeErr(res, http.StatusInternalServerError, "get secret: "+err.Error())
		return
	}
	plain, err := s.Cipher.Decrypt(sec.Payload)
	if err != nil {
		writeErr(res, http.StatusInternalServerError, "decrypt secret: "+err.Error())
		return
	}
	writeJSON(res, http.StatusOK, map[string]any{"name": sec.Name, "value": string(plain)})
}
