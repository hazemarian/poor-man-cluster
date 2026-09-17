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

// SecretService exposes DB-backed secret management over the Bearer-protected
// /api router. The plaintext value is encrypted (AES-GCM) at rest via Cipher
// and only ever appears in the create request body; every other read returns
// the name/scope/hash metadata without revealing the payload.
type SecretService struct {
	Svc service.SecretsService
}

func (s *SecretService) Mount(r chi.Router) {
	r.Get("/secrets", s.list)
	r.Post("/secrets", s.create)
	r.Put("/secrets/{name}", s.update)
	r.Delete("/secrets/{name}", s.remove)
	r.Get("/secrets/{name}/value", s.value)
}

type secretRow struct {
	ID        int64  `json:"id"`
	Scope     string `json:"scope"`
	Stack     string `json:"stack,omitempty"`
	Name      string `json:"name"`
	Hash      string `json:"hash"`
	CreatedAt int64  `json:"created_at"`
}

func (s *SecretService) list(res http.ResponseWriter, req *http.Request) {
	secs, err := s.Svc.List(req.Context(),
		req.URL.Query().Get("scope"), req.URL.Query().Get("stack"))
	if err != nil {
		writeErr(res, http.StatusInternalServerError, "list secrets: "+err.Error())
		return
	}
	rows := make([]secretRow, 0, len(secs))
	for _, x := range secs {
		rows = append(rows, secretRow{ID: x.ID, Scope: x.Scope, Stack: x.Stack, Name: x.Name, Hash: x.Hash, CreatedAt: x.CreatedAt})
	}
	writeJSON(res, http.StatusOK, map[string]any{"secrets": rows})
}

type createSecretRequest struct {
	Name  string `json:"name"`
	Scope string `json:"scope"`
	Stack string `json:"stack"`
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
	stack := strings.TrimSpace(body.Stack)
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
	if stack != "" && scope != "service" {
		writeErr(res, http.StatusBadRequest, "stack is only valid for service-scope secrets")
		return
	}
	if body.Value == "" {
		writeErr(res, http.StatusBadRequest, "value is required")
		return
	}

	id, err := s.Svc.Create(req.Context(), scope, stack, name, body.Value)
	if err != nil {
		if errors.Is(err, store.ErrSecretExists) {
			writeErr(res, http.StatusConflict, "secret already exists: "+name)
			return
		}
		writeErr(res, http.StatusInternalServerError, "create secret: "+err.Error())
		return
	}
	writeJSON(res, http.StatusCreated, map[string]any{
		"id": id, "scope": scope, "stack": stack, "name": name, "hash": store.SecretHash(body.Value),
	})
}

type updateSecretRequest struct {
	Value string `json:"value"`
}

// update replaces a stored secret's value (new ciphertext + hash), keeping its
// scope, stack, name and creation time. 404 when the secret is unknown.
func (s *SecretService) update(res http.ResponseWriter, req *http.Request) {
	name := chi.URLParam(req, "name")
	if name == "" {
		writeErr(res, http.StatusBadRequest, "secret name is required")
		return
	}
	var body updateSecretRequest
	dec := json.NewDecoder(http.MaxBytesReader(res, req.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		writeErr(res, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	if body.Value == "" {
		writeErr(res, http.StatusBadRequest, "value is required")
		return
	}
	if err := s.Svc.Update(req.Context(), name, body.Value); err != nil {
		if errors.Is(err, store.ErrSecretNotFound) {
			writeErr(res, http.StatusNotFound, "secret not found: "+name)
			return
		}
		writeErr(res, http.StatusInternalServerError, "update secret: "+err.Error())
		return
	}
	writeJSON(res, http.StatusOK, map[string]any{"name": name, "hash": store.SecretHash(body.Value)})
}

// remove deletes a secret by name.
func (s *SecretService) remove(res http.ResponseWriter, req *http.Request) {
	name := chi.URLParam(req, "name")
	if name == "" {
		writeErr(res, http.StatusBadRequest, "secret name is required")
		return
	}
	if err := s.Svc.Delete(req.Context(), name); err != nil {
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
	plain, err := s.Svc.Reveal(req.Context(), name)
	if err != nil {
		if errors.Is(err, store.ErrSecretNotFound) {
			writeErr(res, http.StatusNotFound, "secret not found: "+name)
			return
		}
		writeErr(res, http.StatusInternalServerError, "reveal secret: "+err.Error())
		return
	}
	writeJSON(res, http.StatusOK, map[string]any{"name": name, "value": plain})
}
