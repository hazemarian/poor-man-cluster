package githubapp

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/rs/zerolog"
)

// maxSetBodyBytes bounds the JSON body of POST /github/app. A 4096-bit RSA PEM
// is under 4 KiB and a 2048-bit one under 2 KiB, so 64 KiB is generous for a
// key while still refusing a body that is really a file.
const maxSetBodyBytes = 64 << 10

// HTTP exposes the app's configuration state, the Test button and the
// set/rotate route over the Bearer-protected /api router. It never returns key
// material or a minted token: the token broker is a separate, HMAC-authenticated
// endpoint, and there is deliberately no UI that displays tokens.
type HTTP struct {
	App *Local
	// Log receives one audit line per accepted or refused registration change.
	// It is the only place this route writes to a log, and it never receives the
	// request body: the body is a private key.
	Log *zerolog.Logger
}

// Mount registers the console-facing routes. The write sits in the same router
// group as the reads here, and internal/server mounts that group behind the
// operator role — rotating a credential is not a read.
func (h *HTTP) Mount(r chi.Router) {
	r.Get("/github/app", h.status)
	r.Post("/github/app/test", h.test)
	r.Post("/github/app", h.set)
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
			Configured:      false,
			AppID:           0,
			InstallationID:  0,
			ProbeRepo:       "",
			ProbePackage:    "",
			Slug:            "",
			Permissions:     map[string]string{},
			Warning:         "",
			KeyFingerprint:  "",
			LastProbeOK:     false,
			LastProbeDetail: "",
			LastProbeAt:     0,
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

// setAppRequest is the body of POST /github/app. Zero ids and an empty key mean
// "keep what is stored"; the probe fields are pointers so an absent field keeps
// the stored targets while an explicit empty string clears them.
type setAppRequest struct {
	AppID          int64   `json:"app_id"`
	InstallationID int64   `json:"installation_id"`
	PrivateKeyPEM  string  `json:"private_key_pem"`
	ProbeRepo      *string `json:"probe_repo"`
	ProbePackage   *string `json:"probe_package"`
}

// setAppResponse is what the console gets back. It carries the fingerprint —
// never the key, and never a partially masked key, which is just the key with
// extra steps.
type setAppResponse struct {
	AppID          int64         `json:"app_id"`
	InstallationID int64         `json:"installation_id"`
	KeyFingerprint string        `json:"key_fingerprint"`
	Probe          probeResponse `json:"probe"`
}

type probeResponse struct {
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}

// ErrInsecureTransport is returned (as a 400) when a private key would cross
// cleartext to a non-loopback caller.
var ErrInsecureTransport = errors.New("refusing to accept the GitHub App private key over cleartext HTTP")

// set stores or rotates the app registration. Validation is Apply's: parse,
// mint, probe, and only then write — so a request that is wrong anywhere leaves
// the previous registration working.
func (h *HTTP) set(w http.ResponseWriter, r *http.Request) {
	if h.App == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": ErrNoApp.Error()})
		return
	}
	if err := requireSecureTransport(r); err != nil {
		h.auditError(err)
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"code":  "insecure_transport",
			"error": err.Error(),
		})
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxSetBodyBytes)
	var in setAppRequest
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		// The decoder's message names a JSON offset, never the document.
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "read request body: " + err.Error()})
		return
	}

	res, err := h.App.Apply(r.Context(), SetRequest{
		AppID:          in.AppID,
		InstallationID: in.InstallationID,
		PrivateKeyPEM:  []byte(in.PrivateKeyPEM),
		ProbeRepo:      in.ProbeRepo,
		ProbePackage:   in.ProbePackage,
	})
	if err != nil {
		h.auditError(err)
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}

	if h.Log != nil {
		h.Log.Info().
			Int64("app_id", res.AppID).
			Int64("installation_id", res.InstallationID).
			Str("key_fingerprint", res.KeyFingerprint).
			Bool("key_rotated", res.Rotated).
			Bool("probe_ok", res.Probe.OK).
			Msg("github app registration updated from the console")
	}

	writeJSON(w, http.StatusOK, &setAppResponse{
		AppID:          res.AppID,
		InstallationID: res.InstallationID,
		KeyFingerprint: res.KeyFingerprint,
		Probe:          probeResponse{OK: res.Probe.OK, Detail: res.Probe.Detail},
	})
}

// auditError writes one WRN line per refused change. The error is sanitised
// first: a parser that echoed its input would otherwise put a PEM body in the
// daemon log, which is the one thing this route must never do.
func (h *HTTP) auditError(err error) {
	if h.Log == nil || err == nil {
		return
	}
	h.Log.Warn().
		Str("error", auditSafe(err)).
		Msg("github app registration change refused")
}

// auditSafe renders an error for a log line: everything from a PEM header on is
// dropped, newlines are folded, and the result is capped. A parse error may name
// the PEM line and the block type; it may not quote the key.
func auditSafe(err error) string {
	msg := err.Error()
	if i := strings.Index(msg, "-----BEGIN"); i >= 0 {
		msg = msg[:i] + "(pem omitted)"
	}
	msg = strings.Join(strings.Fields(msg), " ")
	const maxLogLen = 300
	if len(msg) > maxLogLen {
		msg = msg[:maxLogLen] + "…"
	}
	return msg
}

// requireSecureTransport refuses a private key that would cross cleartext.
//
// The daemon is normally reached over plain HTTP on loopback (the console runs
// beside it) or through the edge, which terminates TLS and forwards the scheme
// in X-Forwarded-Proto — the same header internal/ui/middleware.IsSecureRequest
// already trusts for the session cookie. Both are accepted; anything else that
// arrives without TLS from another host is not.
func requireSecureTransport(r *http.Request) error {
	if r.TLS != nil {
		return nil
	}
	proto := r.Header.Get("X-Forwarded-Proto")
	if i := strings.IndexByte(proto, ','); i >= 0 {
		proto = proto[:i]
	}
	if strings.EqualFold(strings.TrimSpace(proto), "https") {
		return nil
	}
	if peerIsLoopback(r.RemoteAddr) {
		return nil
	}
	return fmt.Errorf("%w from %s: submit this over HTTPS (the edge sets X-Forwarded-Proto), or run it on the leader itself", ErrInsecureTransport, peerHost(r.RemoteAddr))
}

// peerIsLoopback reports whether the requesting socket is on this host. The
// server's real-IP middleware rewrites RemoteAddr to the forwarded client for
// trusted proxies, so this is the address the request effectively came from —
// for a loopback call it stays 127.0.0.1 or ::1.
func peerIsLoopback(remoteAddr string) bool {
	ip := net.ParseIP(peerHost(remoteAddr))
	return ip != nil && ip.IsLoopback()
}

// peerHost strips the port from RemoteAddr. It tolerates a bare IP, which is the
// shape the real-IP middleware leaves behind.
func peerHost(remoteAddr string) string {
	if host, _, err := net.SplitHostPort(remoteAddr); err == nil {
		return host
	}
	return remoteAddr
}
