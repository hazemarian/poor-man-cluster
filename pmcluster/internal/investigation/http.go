package investigation

import (
	"encoding/json"
	"io"
	"net/http"
	"regexp"

	"github.com/go-chi/chi/v5"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/auth"
)

var jobID = regexp.MustCompile(`^[a-f0-9]{32}$`)

// HTTP exposes policy and accounting behind daemon bearer and scope guards.
type HTTP struct{ Svc Service }

// Mount registers role-gated policy and reservation routes.
func (h *HTTP) Mount(r chi.Router) {
	r.Get("/investigation/policy", func(w http.ResponseWriter, r *http.Request) { p, e := h.Svc.Policy(r.Context()); reply(w, p, e) })
	r.Get("/investigation/budget", func(w http.ResponseWriter, r *http.Request) { b, e := h.Svc.Budget(r.Context()); reply(w, b, e) })
	r.With(auth.RequireRole(auth.RoleAdmin)).Put("/investigation/policy", func(w http.ResponseWriter, r *http.Request) {
		var p Policy
		if !decode(w, r, &p) {
			return
		}
		if err := h.Svc.SetPolicy(r.Context(), p); err != nil {
			http.Error(w, "invalid investigation policy", 400)
			return
		}
		reply(w, p, nil)
	})
	r.With(auth.RequireRole(auth.RoleAdmin)).Post("/investigation/reservations/{id}/{action}", h.reservation)
}

func (h *HTTP) reservation(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if !jobID.MatchString(id) {
		http.Error(w, "invalid job id", 400)
		return
	}
	var result Reservation
	var err error
	switch chi.URLParam(r, "action") {
	case "reserve":
		result, err = h.Svc.Reserve(r.Context(), id)
	case "dispatch":
		result, err = h.Svc.Dispatch(r.Context(), id)
	case "settle":
		var body struct {
			Usage *int64 `json:"usage"`
		}
		if !decode(w, r, &body) {
			return
		}
		result, err = h.Svc.Settle(r.Context(), id, body.Usage)
	default:
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "investigation accounting rejected", http.StatusConflict)
		return
	}
	reply(w, result, nil)
}

func decode(w http.ResponseWriter, r *http.Request, out any) bool {
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16384))
	d.DisallowUnknownFields()
	if d.Decode(out) != nil {
		http.Error(w, "invalid request", 400)
		return false
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		http.Error(w, "invalid request", 400)
		return false
	}
	return true
}

func reply(w http.ResponseWriter, value any, err error) {
	if err != nil {
		http.Error(w, "investigation state unavailable", 503)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}
