package runners

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
)

// HTTP is the console-facing runner surface. The console goes through this same
// API rather than poking the store or the docker socket, so a kill from the
// panel and a kill from curl are one code path with one set of rules.
type HTTP struct {
	Mgr *Manager
}

// Mount registers the routes under the Bearer-protected /api prefix.
func (h *HTTP) Mount(r chi.Router) {
	r.Get("/github/runners", h.list)
	r.Post("/github/runners/{name}/kill", h.kill)
	r.Post("/github/runners/{name}/restart", h.restart)
}

// runnerJSON is one row of the table. Every key is always present so the
// documented schema is the shape of every response, not only the populated one.
type runnerJSON struct {
	Name         string `json:"name"`
	Org          string `json:"org"`
	Node         string `json:"node"`
	ContainerID  string `json:"container_id"`
	Mode         string `json:"mode"`
	State        string `json:"state"`
	RegisteredAt int64  `json:"registered_at"`
	LastSeen     int64  `json:"last_seen"`
	LastJobURL   string `json:"last_job_url"`
	// Reason is why the row is in this state. Always present (empty when there is
	// nothing to explain), so the schema does not change shape per row.
	Reason string `json:"reason"`
	// Caches is this runner's persistent cache volumes and their sizes, measured by
	// the daemon ON ITS NODE. Empty for a runner on another node: nobody here can
	// measure it, and "unknown" must not render as "empty".
	Caches []CacheUsage `json:"caches,omitempty"`
	// CacheTotal is every cache on that node added up, formatted for a table cell.
	CacheTotal string `json:"cache_total,omitempty"`
	// CacheWarnings are the caches that have passed their ceiling. The console shows
	// them as a banner and in the row, because these volumes are the one thing the
	// platform never removes and a full disk surfaces first in an unrelated job.
	CacheWarnings []string `json:"cache_warnings,omitempty"`
}

// runnersJSON is the list response. enabled travels with the list because the
// panel's empty state depends on it and a second round trip would let the two
// disagree.
type runnersJSON struct {
	Enabled bool         `json:"enabled"`
	Runners []runnerJSON `json:"runners"`
}

// killJSON is the outcome of a kill or restart. It is an explicit projection of
// KillOutcome for the same reason runnerJSON exists: every key is always present,
// so the documented schema is the shape of every response — including the half-done
// ones, where deregister_error is the whole point.
type killJSON struct {
	Name              string `json:"name"`
	Org               string `json:"org"`
	Node              string `json:"node"`
	ContainerRemoved  bool   `json:"container_removed"`
	Deregistered      bool   `json:"deregistered"`
	DeregisterError   string `json:"deregister_error"`
	Held              bool   `json:"held"`
	Registered        bool   `json:"registered"`
	RegistrationError string `json:"registration_error"`
	// Registration says where a re-creation will happen when the runner lives on
	// another node (a worker rebuilds its own container; the leader cannot).
	Registration string `json:"registration"`
}

func toKillJSON(out KillOutcome) killJSON { return killJSON(out) }

// killRequest is the confirmation body for kill and restart. confirm is
// required (true) to act on a runner that is executing a job: that action
// cancels the workflow run.
type killRequest struct {
	Confirm bool `json:"confirm"`
}

// list reports the current runners, bounded at 200 rows by the store.
func (h *HTTP) list(w http.ResponseWriter, r *http.Request) {
	if h.Mgr == nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "runner manager unavailable"})
		return
	}
	rows, err := h.Mgr.Runners(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "read runners: " + err.Error()})
		return
	}
	// Measure this daemon's own caches ONCE, then attach them to the rows that live
	// here. Only the daemon on the node can measure its volumes, so a runner on
	// another node reports nothing rather than zero — "not observable from here" and
	// "empty" are different facts and the console renders them differently.
	var cacheUsage []CacheUsage
	var cacheTotal string
	var cacheWarn []string
	if h.Mgr.Docker != nil {
		if u, cerr := ReadCacheUsage(r.Context(), h.Mgr.Docker, RunnerName(h.Mgr.Node)); cerr == nil {
			cacheUsage, cacheTotal = u, HumanBytes(TotalCacheBytes(u))
			cacheWarn = CacheWarnings(u)
		} else {
			cacheWarn = []string{"could not measure this node's runner caches: " + cerr.Error()}
		}
	}
	out := runnersJSON{Enabled: h.Mgr.Enabled(r.Context()), Runners: make([]runnerJSON, 0, len(rows))}
	for _, row := range rows {
		local := strings.EqualFold(row.Node, h.Mgr.Node)
		item := runnerJSON{
			Name:         row.Name,
			Org:          row.Org,
			Node:         row.Node,
			ContainerID:  row.ContainerID,
			Mode:         row.Mode,
			State:        row.State,
			RegisteredAt: row.RegisteredAt,
			LastSeen:     row.LastSeen,
			LastJobURL:   row.LastJobURL,
			Reason:       row.Reason,
		}
		switch {
		case local:
			item.Caches, item.CacheTotal, item.CacheWarnings = cacheUsage, cacheTotal, cacheWarn
		case h.Mgr.CacheReports != nil:
			// Measured on the node itself and reported over the assignment channel: the
			// leader cannot read another machine's volumes.
			if rep, fresh := h.Mgr.CacheReports.Get(row.Node); fresh && len(rep.Caches) > 0 {
				item.Caches = rep.Caches
				item.CacheTotal = HumanBytes(TotalCacheBytes(rep.Caches))
				item.CacheWarnings = CacheWarnings(rep.Caches)
			}
		}
		out.Runners = append(out.Runners, item)
	}
	writeJSON(w, http.StatusOK, out)
}

// kill removes a runner's container and de-registers it. A busy runner is 409
// until the caller confirms: killing it fails the job it is running, and that
// must be a deliberate act, not a mis-click.
func (h *HTTP) kill(w http.ResponseWriter, r *http.Request) {
	if h.Mgr == nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "runner manager unavailable"})
		return
	}
	var req killRequest
	if err := decodeOptionalJSON(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	out, err := h.Mgr.Kill(r.Context(), chi.URLParam(r, "name"), req.Confirm)
	if err != nil {
		writeRunnerError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toKillJSON(out))
}

// restart kills a runner and registers it again immediately. It shares kill's
// confirmation rule for a busy runner, because it cancels that job first.
func (h *HTTP) restart(w http.ResponseWriter, r *http.Request) {
	if h.Mgr == nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "runner manager unavailable"})
		return
	}
	var req killRequest
	if err := decodeOptionalJSON(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	out, err := h.Mgr.Restart(r.Context(), chi.URLParam(r, "name"), req.Confirm)
	if err != nil {
		writeRunnerError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toKillJSON(out))
}

// writeRunnerError maps the two refusals a caller can act on, and everything
// else to a 500. A GitHub failure is NOT an error here: the kill already
// succeeded on the container and the outcome carries the de-registration
// failure in its body, which is the difference between "we could not do it" and
// "we did half of it".
func writeRunnerError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrRunnerNotFound):
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "unknown runner: no row with that name (it may have been removed by a reconcile pass)"})
	case errors.Is(err, ErrRunnerBusy):
		writeJSON(w, http.StatusConflict, map[string]any{"error": ErrRunnerBusy.Error()})
	default:
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
	}
}

// decodeOptionalJSON accepts an absent body as the zero value and refuses a
// malformed one. A kill with no body is the normal non-busy case.
func decodeOptionalJSON(r *http.Request, dst any) error {
	if r.Body == nil || r.ContentLength == 0 {
		return nil
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return errors.New("body must be JSON: {\"confirm\": true|false}")
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
