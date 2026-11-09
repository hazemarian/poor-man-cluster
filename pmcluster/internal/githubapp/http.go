package githubapp

import (
	"net/http"

	"github.com/go-chi/chi/v5"
)

// HTTP exposes the app's configuration state and the Test button over the
// Bearer-protected /api router. It never returns key material or a minted
// token: the token broker is a separate, HMAC-authenticated endpoint, and there
// is deliberately no UI that displays tokens.
type HTTP struct {
	App *Local
}

// Mount registers the console-facing routes.
func (h *HTTP) Mount(r chi.Router) {
	r.Get("/github/app", h.status)
	r.Post("/github/app/test", h.test)
}

// status reports the app id, installation id, probe targets and the granted
// permissions — the permission set is the ceiling the broker cannot narrow, so
// showing it is the only honest thing to do.
func (h *HTTP) status(w http.ResponseWriter, r *http.Request) {
	if h.App == nil {
		// Every key is spelled out even on the "not configured" path: the
		// console reads the same shape whether or not an app exists, and the
		// documented schema is the union of both branches.
		writeJSON(w, http.StatusOK, &Status{
			Configured:     false,
			AppID:          0,
			InstallationID: 0,
			ProbeRepo:      "",
			ProbePackage:   "",
			Slug:           "",
			Permissions:    map[string]string{},
			Warning:        "",
		})
		return
	}
	st, err := h.App.Status(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "read github app status: " + err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// test mints a token and runs the configured probes. A failed test is still a
// 200: it is an answer ("this does not work, here is why"), not a server fault,
// and the console renders the message.
func (h *HTTP) test(w http.ResponseWriter, r *http.Request) {
	if h.App == nil {
		writeJSON(w, http.StatusOK, TestResult{OK: false, Message: ErrNoApp.Error()})
		return
	}
	writeJSON(w, http.StatusOK, h.App.Test(r.Context()))
}
