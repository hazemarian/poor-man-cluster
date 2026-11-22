package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/gorilla/websocket"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/auth"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runtime"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/services"
)

// shellWSPathPrefix is the exec endpoint for an on-demand shell:
// /api/shell/<serviceID>/exec/ws. Its own prefix because the target is a container the api created,
// not a (stack, service) pair of the user's stack.
const shellWSPathPrefix = "/api/shell/"

// shellResourcePrefix is the collection a session is deleted from. Distinct from the websocket
// prefix on purpose, and confusing them cost a "page not found" on every teardown.
const shellResourcePrefix = "/api/shells/"

// ShellOpenHandler opens a shell session for a stack: it creates the throwaway container, waits for
// it to run on this node, and answers the IDs the console needs to connect and to tear down.
//
// Creating a container with a stack's secrets and configs is a privileged act, so it sits on the
// same bearer + stack-scope chain as every other mutating API call, and the caller is expected to
// close it (the console does, when the socket ends).
func ShellOpenHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		if d.OpenShell == nil {
			http.Error(w, "shell sessions are not available on this node", http.StatusNotImplemented)
			return
		}
		stack := strings.TrimPrefix(r.URL.Path, "/api/stacks/")
		stack = strings.TrimSuffix(stack, "/shell")
		if stack == "" || strings.Contains(stack, "/") {
			http.Error(w, "stack is required", http.StatusBadRequest)
			return
		}
		image := strings.TrimSpace(r.URL.Query().Get("image"))
		sess, err := d.OpenShell(r.Context(), stack, image)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"service_id":   sess.ServiceID,
			"container_id": sess.ContainerID,
			"node":         sess.Node,
			// The ceiling, so the console can show it and close early if it wants to.
			"max_seconds": int(runtime.ShellMaxLife.Seconds()),
		})
	}
}

// shellExecWSHandler serves the interactive session inside an on-demand shell container and removes
// the container when the socket closes, whatever closed it.
// authorizeShellTarget is the guard C3 of the 2026-11-21 audit was about.
//
// The socket used to require only a valid bearer token and then resolve whatever id the caller named
// through ShellContainer — so any token, including a stack-scoped one, could exec into `infra_traefik`,
// which mounts the Docker socket: root on the manager, and the close handler then removed the service.
//
// A shell target must be a shell session (the label the platform stamps on its own throwaway services)
// and, when the token carries a stack scope, that session's stack must be the token's. Unlabelled
// services are refused rather than "not found": the difference between the two is what tells an
// operator their token is scoped rather than their id being wrong.
func authorizeShellTarget(w http.ResponseWriter, r *http.Request, d Deps, id string) bool {
	u := auth.FromContext(r.Context())
	if u == nil {
		writeErr(w, http.StatusUnauthorized, "authentication required")
		return false
	}
	if d.Docker == nil {
		writeErr(w, http.StatusNotImplemented, "interactive exec unavailable on this node")
		return false
	}
	svc, err := d.Docker.ServiceInspect(r.Context(), id)
	if err != nil || svc.Name == "" {
		writeErr(w, http.StatusNotFound, "no such shell session: "+id)
		return false
	}
	if svc.Labels[runtime.ShellLabel] != "true" {
		writeErr(w, http.StatusForbidden,
			"not a shell session: "+svc.Name+" is not labelled "+runtime.ShellLabel+"=true")
		return false
	}
	if target := svc.Labels[runtime.ShellStackLabel]; u.Stack != "" && target != u.Stack {
		writeErr(w, http.StatusForbidden, "token scoped to stack "+u.Stack)
		return false
	}
	return true
}

func shellExecWSHandler(d Deps) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, shellWSPathPrefix), "/exec/ws")
		if id == "" || strings.Contains(id, "/") {
			http.Error(w, "shell id is required", http.StatusNotFound)
			return
		}
		next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !authorizeShellTarget(w, r, d, id) {
				return
			}
			serveShellExecWS(w, r, d, id)
		})
		auth.Bearer(d.Lookup)(next).ServeHTTP(w, r)
	})
}

func serveShellExecWS(w http.ResponseWriter, r *http.Request, d Deps, serviceID string) {
	attacher, ok := d.Services.(services.ExecAttacher)
	if !ok || d.ShellContainer == nil {
		http.Error(w, "interactive exec unavailable on this node", http.StatusNotImplemented)
		return
	}
	cid, err := d.ShellContainer(r.Context(), serviceID)
	if err != nil {
		// The shell is gone (TTL expired, or the service was already removed).
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}

	up := websocket.Upgrader{
		ReadBufferSize:  4096,
		WriteBufferSize: 4096,
		CheckOrigin:     func(*http.Request) bool { return true },
	}
	conn, err := up.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close() //nolint:errcheck // best-effort socket close

	// Whatever happens next — the operator closes the tab, the shell's sleep ends, the node
	// reboots — the container does not outlive this socket. That is the operator's instruction:
	// no standing shells.
	if d.CloseShell != nil {
		defer func() {
			// Detached: the request context is cancelled the moment the handler returns.
			ctx, cancel := context.WithTimeout(context.Background(), 20*1e9)
			defer cancel()
			_ = d.CloseShell(ctx, serviceID)
		}()
	}

	argv := splitExecArgv(r.URL.Query().Get("cmd"))
	rows, cols := execDims(r.URL.Query().Get("rows"), r.URL.Query().Get("cols"))
	stream, err := attacher.ExecAttachContainer(r.Context(), cid, argv, rows, cols)
	if err != nil {
		writeWSClose(conn, websocket.CloseInternalServerErr, "exec: "+err.Error())
		return
	}
	defer stream.Close()
	pumpExecStream(conn, stream, r.Context())
}

// ShellRemoveHandler destroys a session the console is finished with, so an operator can close one
// without waiting for a socket teardown.
func ShellRemoveHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete && r.Method != http.MethodPost {
			http.Error(w, "DELETE only", http.StatusMethodNotAllowed)
			return
		}
		if d.CloseShell == nil {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		// The resource prefix, not the websocket one: "/api/shell/" never matches "/api/shells/…",
		// so trimming it left the whole path as the id and dockerd answered "page not found".
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, shellResourcePrefix), "/")
		if id == "" || strings.Contains(id, "/") {
			http.Error(w, "shell id is required", http.StatusBadRequest)
			return
		}
		// Same guard as the socket: DELETE by id used to remove ANY service for any valid token.
		if !authorizeShellTarget(w, r, d, id) {
			return
		}
		if err := d.CloseShell(r.Context(), id); err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}
