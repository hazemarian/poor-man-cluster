package controllers

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

// Terminal renders a standalone xterm.js page that opens an interactive shell
// in a running service container. The browser connects to THIS console route;
// the console upgrades the socket and dials the daemon's exec websocket with
// the stored API token server-side (the browser never sees the token).
type Terminal struct{ *Controller }

// terminalPageData is the standalone document payload.
type terminalPageData struct {
	Stack   string
	Service string
	// WSURL is the /web route the page's javascript connects to. The console
	// dials the daemon itself; the browser never sees the daemon address.
	WSURL   string
	Domain  string
	Version string
}

// Page renders the terminal document.
func (t Terminal) Page(g *gin.Context) {
	stack, service := g.Param("name"), g.Query("service")
	d := terminalPageData{Stack: stack, Service: service, Domain: t.Domain, Version: t.Version}
	// The page's WS targets this console route; the console bridges to the
	// daemon. ws(s) follows the /web scheme the browser used.
	scheme := "ws"
	if g.Request.TLS != nil || strings.HasPrefix(g.Request.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "wss"
	}
	host := g.Request.Host
	if host == "" {
		host = t.Domain
	}
	d.WSURL = fmt.Sprintf("%s://%s%s/stacks/%s/terminal/ws?service=%s",
		scheme, host, WebBase, url.PathEscape(stack), url.QueryEscape(service))
	t.Views.Page(g, "terminal", d)
}

// WS upgrades the browser's socket, dials the daemon's interactive exec
// websocket with the stored API token, and relays frames verbatim in both
// directions. The browser <-> console frames use the same wire protocol as
// console <-> daemon (binary = stdin/stdout bytes, text JSON = resize/exit
// controls), so the relay is a pure pass-through.
func (t Terminal) WS(g *gin.Context) {
	ctx := g.Request.Context()
	stack, service := g.Param("name"), g.Query("service")
	apiURL, token, configured := t.loadParams(ctx)

	up := websocket.Upgrader{
		ReadBufferSize:  4096,
		WriteBufferSize: 4096,
		// Same-origin by construction (the page and this route share the
		// console host); be permissive for X-Forwarded-Proto variations.
		CheckOrigin: func(*http.Request) bool { return true },
	}
	client, err := up.Upgrade(g.Writer, g.Request, nil)
	if err != nil {
		return // upgrade already wrote the error
	}
	defer client.Close() //nolint:errcheck // best-effort socket close

	if service == "" || !configured {
		writeJSONFrame(client, map[string]any{"type": "error", "error": "terminal: service or daemon not configured"})
		return
	}

	daemonURL, err := terminalDaemonWSURL(apiURL, stack, service)
	if err != nil {
		writeJSONFrame(client, map[string]any{"type": "error", "error": err.Error()})
		return
	}

	header := http.Header{}
	header.Set("Authorization", "Bearer "+token)
	daemon, _, err := websocket.DefaultDialer.Dial(daemonURL, header)
	if err != nil {
		writeJSONFrame(client, map[string]any{"type": "error", "error": "daemon exec socket: " + err.Error()})
		return
	}
	defer daemon.Close() //nolint:errcheck // best-effort socket close

	// daemon -> browser
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			mt, data, err := daemon.ReadMessage()
			if err != nil {
				_ = client.WriteControl(websocket.CloseMessage,
					websocket.FormatCloseMessage(websocket.CloseNormalClosure, "session ended"),
					time.Now().Add(5*time.Second))
				return
			}
			if werr := client.WriteMessage(mt, data); werr != nil {
				return
			}
		}
	}()

	// browser -> daemon
	for {
		mt, data, err := client.ReadMessage()
		if err != nil {
			_ = daemon.Close()
			return
		}
		if werr := daemon.WriteMessage(mt, data); werr != nil {
			writeJSONFrame(client, map[string]any{"type": "error", "error": "daemon exec socket: " + werr.Error()})
			return
		}
	}
}

// terminalDaemonWSURL converts the stored http(s) daemon base URL into the
// exec websocket URL for (stack, service).
func terminalDaemonWSURL(apiURL, stack, service string) (string, error) {
	u, err := url.Parse(apiURL)
	if err != nil {
		return "", fmt.Errorf("daemon url: %w", err)
	}
	switch u.Scheme {
	case "http":
		u.Scheme = "ws"
	case "https":
		u.Scheme = "wss"
	default:
		return "", fmt.Errorf("daemon url: unsupported scheme %q", u.Scheme)
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/api/services/" + url.PathEscape(stack) + "/" + url.PathEscape(service) + "/exec/ws"
	q := u.Query()
	q.Set("cmd", "sh")
	u.RawQuery = q.Encode()
	return u.String(), nil
}

func writeJSONFrame(conn *websocket.Conn, v any) {
	_ = conn.WriteJSON(v)
}
