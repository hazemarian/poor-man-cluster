package services

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
)

// HTTP serves the whitelisted service-ops REST surface (Bearer-auth wrapped
// by the parent router):
//
//	GET  /api/services              — every swarm service
//	GET  /api/services/{stack}      — services of one stack
//	GET  /api/services/{stack}/{service}/tasks — task (crash/restart) history
//	GET  /api/services/{stack}/{service}/logs?tail=N
//	POST /api/services/{stack}/{service}/restart
//	POST /api/services/{stack}/{service}/exec  — body {argv:[...]}
type HTTP struct {
	Svc Service
}

// Mount expects to be wrapped with Bearer auth in the parent router.
func (h *HTTP) Mount(r chi.Router) {
	r.Get("/services", h.list)
	r.Get("/services/{stack}", h.listStack)
	r.Get("/services/{stack}/{service}/tasks", h.tasks)
	r.Get("/services/{stack}/{service}/logs", h.logs)
	r.Post("/services/{stack}/{service}/restart", h.restart)
	r.Post("/services/{stack}/{service}/exec", h.exec)
}

func (h *HTTP) list(w http.ResponseWriter, r *http.Request) {
	svcs, err := h.Svc.List(r.Context(), "")
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"services": serviceJSONList(svcs)})
}

func (h *HTTP) listStack(w http.ResponseWriter, r *http.Request) {
	stack := chi.URLParam(r, "stack")
	svcs, err := h.Svc.List(r.Context(), stack)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"stack": stack, "services": serviceJSONList(svcs)})
}

func (h *HTTP) tasks(w http.ResponseWriter, r *http.Request) {
	stack, service := chi.URLParam(r, "stack"), chi.URLParam(r, "service")
	tasks, err := h.Svc.Tasks(r.Context(), stack, service)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	out := make([]map[string]any, 0, len(tasks))
	for _, t := range tasks {
		out = append(out, map[string]any{
			"task_id":     t.TaskID,
			"node":        t.Node,
			"slot":        t.Slot,
			"state":       t.State,
			"error":       t.Error,
			"started_at":  t.StartedAt,
			"finished_at": t.FinishedAt,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"service": stack + "_" + service, "tasks": out})
}

func (h *HTTP) logs(w http.ResponseWriter, r *http.Request) {
	stack, service := chi.URLParam(r, "stack"), chi.URLParam(r, "service")
	tail := 200
	if v := r.URL.Query().Get("tail"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "tail must be an integer"})
			return
		}
		tail = n
	}
	lines, err := h.Svc.Logs(r.Context(), stack, service, tail)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	out := make([]map[string]any, 0, len(lines))
	for _, ln := range lines {
		out = append(out, map[string]any{"stream": ln.Stream, "line": ln.Line})
	}
	writeJSON(w, http.StatusOK, map[string]any{"service": stack + "_" + service, "logs": out})
}

func (h *HTTP) restart(w http.ResponseWriter, r *http.Request) {
	stack, service := chi.URLParam(r, "stack"), chi.URLParam(r, "service")
	if err := h.Svc.Restart(r.Context(), stack, service); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"service": stack + "_" + service, "restarted": true})
}

func (h *HTTP) exec(w http.ResponseWriter, r *http.Request) {
	stack, service := chi.URLParam(r, "stack"), chi.URLParam(r, "service")
	var body struct {
		Argv []string `json:"argv"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 8<<10)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON: " + err.Error()})
		return
	}
	res, err := h.Svc.Exec(r.Context(), stack, service, body.Argv)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"service":   stack + "_" + service,
		"exit_code": res.ExitCode,
		"stdout":    res.Stdout,
		"stderr":    res.Stderr,
	})
}

func serviceJSONList(svcs []ServiceSummary) []map[string]any {
	out := make([]map[string]any, 0, len(svcs))
	for _, s := range svcs {
		out = append(out, map[string]any{
			"name":     s.Name,
			"stack":    s.Stack,
			"replicas": s.Replicas,
			"desired":  s.Desired,
			"image":    s.Image,
			"mode":     s.Mode,
			"updated":  s.Updated,
		})
	}
	return out
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
