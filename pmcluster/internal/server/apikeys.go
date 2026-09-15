package server

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/hazemarian/poor-man-stack/pmcluster/internal/auth"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/store"
)

// edgeAPITokenUser is the daemon user row that owns the edge console's API
// token. Deleting it would break console → daemon authentication, so the
// API refuses to remove it.
const edgeAPITokenUser = "edge"

// APIKeyService exposes API-user (bearer token) management over the
// Bearer-protected /api router. The bearer token is generated on the daemon,
// hashed (argon2id) at rest, and returned to the caller ONCE on creation —
// listing users never reveals token material (the operator edge UI shows the
// token in the create response only).
type APIKeyService struct {
	Store *store.Store
}

func (a *APIKeyService) Mount(r chi.Router) {
	r.Get("/api_keys", a.list)
	r.Post("/api_keys", a.create)
	r.Delete("/api_keys/{id}", a.remove)
}

type apiKeyRow struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	CreatedAt int64  `json:"created_at"`
}

func (a *APIKeyService) list(res http.ResponseWriter, req *http.Request) {
	users, err := a.Store.ListUsers(req.Context())
	if err != nil {
		writeErr(res, http.StatusInternalServerError, "list api keys: "+err.Error())
		return
	}
	rows := make([]apiKeyRow, 0, len(users))
	for _, u := range users {
		rows = append(rows, apiKeyRow{ID: u.ID, Name: u.Name, CreatedAt: u.CreatedAt})
	}
	writeJSON(res, http.StatusOK, map[string]any{"keys": rows})
}

type createAPIKeyRequest struct {
	Name string `json:"name"`
}

func (a *APIKeyService) create(res http.ResponseWriter, req *http.Request) {
	var body createAPIKeyRequest
	dec := json.NewDecoder(http.MaxBytesReader(res, req.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		writeErr(res, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	name := strings.TrimSpace(body.Name)
	if name == "" {
		writeErr(res, http.StatusBadRequest, "name is required")
		return
	}

	token, err := auth.GenerateToken()
	if err != nil {
		writeErr(res, http.StatusInternalServerError, "generate token: "+err.Error())
		return
	}
	tokenID, secret := auth.SplitToken(token)
	hash, err := auth.HashToken(secret)
	if err != nil {
		writeErr(res, http.StatusInternalServerError, "hash token: "+err.Error())
		return
	}
	id, err := a.Store.CreateUser(req.Context(), name, tokenID, hash)
	if err != nil {
		if errors.Is(err, store.ErrUserExists) {
			writeErr(res, http.StatusConflict, "user already exists: "+name)
			return
		}
		writeErr(res, http.StatusInternalServerError, "create user: "+err.Error())
		return
	}

	writeJSON(res, http.StatusCreated, map[string]any{"id": id, "name": name, "token": token})
}

// remove deletes an API user by id, revoking its bearer token immediately.
// Guarded against removing the "edge" console user (would break the operator
// console) and against removing the currently-authenticated user.
func (a *APIKeyService) remove(res http.ResponseWriter, req *http.Request) {
	idStr := chi.URLParam(req, "id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil || id <= 0 {
		writeErr(res, http.StatusBadRequest, "invalid user id")
		return
	}

	user, err := a.Store.UserByID(req.Context(), id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeErr(res, http.StatusNotFound, "api key not found")
			return
		}
		writeErr(res, http.StatusInternalServerError, "lookup api key: "+err.Error())
		return
	}

	if user.Name == edgeAPITokenUser {
		writeErr(res, http.StatusConflict, "the 'edge' user is required by the operator console and cannot be removed")
		return
	}
	if me := auth.FromContext(req.Context()); me != nil && me.ID == id {
		writeErr(res, http.StatusConflict, "cannot remove the API key you are currently authenticated with")
		return
	}

	if err := a.Store.DeleteUser(req.Context(), id); err != nil {
		if errors.Is(err, store.ErrUserNotFound) {
			writeErr(res, http.StatusNotFound, "api key not found")
			return
		}
		writeErr(res, http.StatusInternalServerError, "delete api key: "+err.Error())
		return
	}
	res.WriteHeader(http.StatusNoContent)
}
