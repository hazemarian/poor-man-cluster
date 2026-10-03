package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/websocket"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/auth"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/services"
)

// execWSPathPrefix is the top-level dispatch match for the interactive exec
// websocket endpoint. The handler is served OUTSIDE the chi router — the
// router's httpStatusSpan wrapper and the final otelhttp wrapper do not
// implement http.Hijacker, which websocket.Upgrade requires.
const execWSPathPrefix = "/api/services/"

// execWSPathSuffix completes the route: /api/services/<stack>/<service>/exec/ws.
const execWSPathSuffix = "/exec/ws"

// resizeControl is the client→server text frame: ask the exec TTY to resize.
// Binary frames are raw stdin bytes; server→client binary frames are raw TTY
// output; the terminal session ends with {"type":"exit","code":N} then close.
type resizeControl struct {
	Type string `json:"type"`
	Rows uint   `json:"rows"`
	Cols uint   `json:"cols"`
}

// exitControl is the server→client final frame.
type exitControl struct {
	Type string `json:"type"`
	Code int    `json:"code"`
}

// execWSHandler serves the daemon-side interactive exec endpoint. It is
// dispatched before the otelhttp / statusRecorder wrappers so the websocket
// upgrade sees a real http.Hijacker, then re-applies the same auth chain the
// /api subtree uses (Bearer + per-stack scope guard).
func execWSHandler(d Deps) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		stack, service, ok := parseExecWSPath(r.URL.Path)
		if !ok {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			serveExecWS(w, r, d.Services, stack, service)
		})
		auth.Bearer(d.Lookup)(stackScopeGuard(next)).ServeHTTP(w, r)
	})
}

// parseExecWSPath splits /api/services/<stack>/<service>/exec/ws.
func parseExecWSPath(path string) (stack, service string, ok bool) {
	if !strings.HasPrefix(path, execWSPathPrefix) || !strings.HasSuffix(path, execWSPathSuffix) {
		return "", "", false
	}
	mid := strings.TrimSuffix(strings.TrimPrefix(path, execWSPathPrefix), execWSPathSuffix)
	parts := strings.Split(mid, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}

// serveExecWS upgrades the connection, starts an interactive exec session in
// the named service's running task, and pumps frames until either side ends.
func serveExecWS(w http.ResponseWriter, r *http.Request, svc services.Service, stack, service string) {
	attacher, ok := svc.(services.ExecAttacher)
	if !ok {
		http.Error(w, "interactive exec unavailable on this node", http.StatusNotImplemented)
		return
	}

	argv := splitExecArgv(r.URL.Query().Get("cmd"))
	rows, cols := execDims(r.URL.Query().Get("rows"), r.URL.Query().Get("cols"))

	up := websocket.Upgrader{
		ReadBufferSize:  4096,
		WriteBufferSize: 4096,
		// The console dials with an explicit Origin (its own /web pages);
		// the daemon may be reached from several console hosts.
		CheckOrigin: func(*http.Request) bool { return true },
	}
	conn, err := up.Upgrade(w, r, nil)
	if err != nil {
		return // upgrade already wrote the error
	}
	defer conn.Close() //nolint:errcheck // closing the session socket is best-effort

	stream, err := attacher.ExecAttach(r.Context(), stack, service, argv, rows, cols)
	if err != nil {
		writeWSClose(conn, websocket.CloseInternalServerErr, "exec: "+err.Error())
		return
	}
	defer stream.Close()

	// Read the exec session's output and forward it as binary frames; when
	// the session ends, send the exit frame and close the socket.
	go func() {
		buf := make([]byte, 32*1024)
		for {
			n, rerr := stream.Read(buf)
			if n > 0 {
				if werr := conn.WriteMessage(websocket.BinaryMessage, buf[:n]); werr != nil {
					return
				}
			}
			if rerr != nil {
				break
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		code, _ := stream.Wait(ctx)
		_ = conn.WriteJSON(exitControl{Type: "exit", Code: code})
		_ = conn.WriteControl(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, "session ended"),
			time.Now().Add(5*time.Second))
	}()

	for {
		mt, data, err := conn.ReadMessage()
		if err != nil {
			return // client went away → Close() kills the session
		}
		switch mt {
		case websocket.BinaryMessage:
			if _, werr := stream.Write(data); werr != nil {
				writeWSClose(conn, websocket.CloseInternalServerErr, "stdin: "+werr.Error())
				return
			}
		case websocket.TextMessage:
			var c resizeControl
			if json.Unmarshal(data, &c) == nil && c.Type == "resize" && c.Rows > 0 && c.Cols > 0 {
				_ = stream.Resize(r.Context(), c.Rows, c.Cols)
			}
		}
	}
}

func writeWSClose(conn *websocket.Conn, code int, msg string) {
	_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(code, msg), time.Now().Add(5*time.Second))
}

// splitExecArgv splits a cmd query value into argv (whitespace-separated).
// Empty → ["sh"].
func splitExecArgv(cmd string) []string {
	if cmd == "" {
		return []string{"sh"}
	}
	return strings.Fields(cmd)
}

// execDims parses rows/cols query values; 0/absent → defaults applied by the
// backend.
func execDims(rows, cols string) (uint, uint) {
	var r, c uint
	_, _ = fmt.Sscanf(rows, "%d", &r)
	_, _ = fmt.Sscanf(cols, "%d", &c)
	return r, c
}
