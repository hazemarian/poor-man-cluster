package controllers

import (
	"fmt"
	"io"
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
	WSURL     string
	ReportURL string
	Domain    string
	Version   string
}

// Page renders the terminal document.
func (t Terminal) Page(g *gin.Context) {
	stack, service := g.Param("name"), g.Query("service")
	// The page carries a per-render ticket and its own bootstrap: a cached copy is a page that
	// cannot connect and cannot be told why. Never serve it from the browser cache.
	g.Header("Cache-Control", "no-store, must-revalidate")
	// One line per open. With this and the WS entry line below, "did the request reach the
	// console at all" stops being a guess: no page line means the gate in front refused it before
	// us, no WS line with a page line means the browser never opened the socket.
	t.Log.Info().Str("stack", stack).Str("service", service).Str("host", g.Request.Host).
		Str("xfp", g.Request.Header.Get("X-Forwarded-Proto")).Msg("terminal: page served")
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
	// The socket carries its own ticket: a WebSocket handshake cannot ride the forward-auth gate
	// in front of /web (see middleware.SignTerminalToken), so the console proves the socket came
	// from a page it rendered, for this stack and this service, two minutes ago.
	ticket := t.Auth.SignTerminalToken(stack, service, time.Now())
	d.WSURL = fmt.Sprintf("%s://%s%s/stacks/%s/terminal/ws?service=%s&t=%s",
		scheme, host, WebBase, url.PathEscape(stack), url.QueryEscape(service), url.QueryEscape(ticket))
	d.ReportURL = fmt.Sprintf("%s/stacks/%s/terminal/report?service=%s",
		WebBase, url.PathEscape(stack), url.QueryEscape(service))
	t.Views.Page(g, "terminal", d)
}

// sameOrigin accepts a WebSocket upgrade whose Origin host matches the request host, and refuses one
// that does not. A browser cannot lie about Origin; a non-browser client (the CLI) sends none, and an
// absent Origin is allowed because there is no ambient session to ride.
//
// This is the difference between "any page on any subdomain of yours can open a terminal with the
// operator's cookie" and "the console can".
func sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	return strings.EqualFold(u.Host, r.Host)
}

// ShellPage opens an on-demand shell for a stack and renders the terminal bound to it.
//
// This is the answer to placement: instead of exec'ing into a service container (which only works
// when that container shares a node with the daemon), the daemon creates a throwaway container ON
// ITS OWN NODE, joins it to the stack's networks with the stack's secrets and configs, and the
// socket execs into that. One node, no cross-node channel, and it reaches every service of the stack
// by name.
func (t Terminal) ShellPage(g *gin.Context) {
	stack := g.Param("name")
	g.Header("Cache-Control", "no-store, must-revalidate")
	ctx := g.Request.Context()
	// No caller-chosen image. The fleet runs ONE shell image (PMCLUSTER_SHELL_IMAGE), and letting a
	// query parameter name any image on the leader — as root, with the stack's secrets mounted — was
	// H4's sharpest edge. An operator who needs a different image changes it for the fleet.
	if g.Request.Method != http.MethodPost {
		g.Status(http.StatusMethodNotAllowed)
		return
	}
	sess, err := t.API.OpenShell(ctx, stack, "")
	if err != nil {
		t.Log.Warn().Err(err).Str("stack", stack).Msg("terminal: could not open a shell session")
		g.Status(http.StatusBadGateway)
		t.Views.Page(g, "terminal_error", map[string]any{
			"Stack": stack, "Reason": err.Error(),
		})
		return
	}
	// If the operator never connects (closed the tab during the redirect), the shell is removed by
	// the daemon when the socket would have closed — and by the TTL regardless. Nothing to do here.

	scheme := "ws"
	if g.Request.TLS != nil || strings.HasPrefix(g.Request.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "wss"
	}
	host := g.Request.Host
	if host == "" {
		host = t.Domain
	}
	ticket := t.Auth.SignTerminalToken("shell:"+sess.ServiceID, "sh", time.Now())
	d := terminalPageData{
		Stack:   stack,
		Service: "shell on " + sess.Node,
		Domain:  t.Domain,
		Version: t.Version,
	}
	d.WSURL = fmt.Sprintf("%s://%s%s/shells/%s/ws?t=%s",
		scheme, host, WebBase, url.PathEscape(sess.ServiceID), url.QueryEscape(ticket))
	d.ReportURL = fmt.Sprintf("%s/shells/%s/report", WebBase, url.PathEscape(sess.ServiceID))
	t.Views.Page(g, "terminal", d)
}

// ShellWS relays the browser to the daemon's shell socket.
func (t Terminal) ShellWS(g *gin.Context) {
	ctx := g.Request.Context()
	id := g.Param("id")
	if !t.Auth.VerifyTerminalToken(g.Query("t"), "shell:"+id, "sh", time.Now()) {
		t.Log.Warn().Str("shell", id).Msg("terminal: refused a shell socket without a valid ticket")
		g.AbortWithStatus(http.StatusForbidden)
		return
	}
	apiURL, token, configured := t.loadParams(ctx)
	if !configured {
		g.AbortWithStatus(http.StatusServiceUnavailable)
		return
	}
	daemonURL := terminalDaemonShellURL(apiURL, id, g.Query("rows"), g.Query("cols"))

	up := websocket.Upgrader{
		ReadBufferSize:  4096,
		WriteBufferSize: 4096,
		CheckOrigin:     sameOrigin,
	}
	client, err := up.Upgrade(g.Writer, g.Request, nil)
	if err != nil {
		t.Log.Warn().Err(err).Str("shell", id).Msg("terminal: the browser connection could not be upgraded")
		return
	}
	defer client.Close() //nolint:errcheck // best-effort socket close

	header := http.Header{}
	header.Set("Authorization", "Bearer "+token)
	daemon, resp, err := websocket.DefaultDialer.Dial(daemonURL, header)
	if err != nil {
		code := 0
		if resp != nil {
			code = resp.StatusCode
		}
		t.Log.Warn().Err(err).Int("status", code).Str("shell", id).
			Str("daemon", daemonURL).Msg("terminal: the daemon refused the shell socket")
		writeJSONFrame(client, map[string]any{"type": "error", "error": "daemon shell socket: " + err.Error()})
		return
	}
	defer daemon.Close() //nolint:errcheck // best-effort socket close
	relaySockets(client, daemon)
}

// ShellReport is the shell page's failure line. Without it the page's report POST 404s and the one
// sentence that explains a dead socket never reaches a log — the operator sees "could not start" and
// nobody can say why.
func (t Terminal) ShellReport(g *gin.Context) {
	id := g.Param("id")
	body, _ := io.ReadAll(io.LimitReader(g.Request.Body, 4<<10))
	t.Log.Warn().Str("shell", id).Str("client", string(body)).Str("ua", g.Request.UserAgent()).
		Msg("terminal: the browser could not open the shell socket")
	g.Status(http.StatusNoContent)
}

// Report is where the page tells the console why its socket failed.
//
// The browser is the only place a failed handshake is explained — "Could not start the terminal
// session" is all it shows, while the reason (a proxy refusing the upgrade, a policy, a wrong
// scheme) never reaches the server. The page beacons here on error, so the console's log carries
// the browser's own view instead of an operator having to open DevTools on someone else's machine.
func (t Terminal) Report(g *gin.Context) {
	stack := g.Param("name")
	body, _ := io.ReadAll(io.LimitReader(g.Request.Body, 4<<10))
	t.Log.Warn().Str("stack", stack).Str("service", g.Query("service")).
		Str("client", string(body)).Str("ua", g.Request.UserAgent()).
		Msg("terminal: the browser could not open the socket")
	g.Status(http.StatusNoContent)
}

// WS upgrades the browser's socket, dials the daemon's interactive exec
// websocket with the stored API token, and relays frames verbatim in both
// directions. The browser <-> console frames use the same wire protocol as
// console <-> daemon (binary = stdin/stdout bytes, text JSON = resize/exit
// controls), so the relay is a pure pass-through.
func (t Terminal) WS(g *gin.Context) {
	ctx := g.Request.Context()
	stack, service := g.Param("name"), g.Query("service")
	// Verify the ticket BEFORE the upgrade: a bad one is a plain 403, which the browser reports as
	// a failed handshake rather than a mysterious "closed" after a successful one.
	if !t.Auth.VerifyTerminalToken(g.Query("t"), stack, service, time.Now()) {
		t.Log.Warn().Str("stack", stack).Str("service", service).
			Msg("terminal: refused a socket without a valid ticket")
		g.AbortWithStatus(http.StatusForbidden)
		return
	}

	apiURL, token, configured := t.loadParams(ctx)
	t.Log.Info().Str("stack", stack).Str("service", service).Bool("configured", configured).
		Str("upgrade", g.GetHeader("Upgrade")).Str("origin", g.GetHeader("Origin")).
		Msg("terminal: websocket handshake arrived")

	up := websocket.Upgrader{
		ReadBufferSize:  4096,
		WriteBufferSize: 4096,
		// Same-origin by construction (the page and this route share the
		// console host); be permissive for X-Forwarded-Proto variations.
		CheckOrigin: sameOrigin,
	}
	client, err := up.Upgrade(g.Writer, g.Request, nil)
	if err != nil {
		// The upgrade already wrote the HTTP error; log it so a browser-side failure that never
		// becomes a websocket is still visible from the server.
		t.Log.Warn().Err(err).Str("stack", stack).Str("service", service).
			Msg("terminal: the browser connection could not be upgraded")
		return
	}
	defer client.Close() //nolint:errcheck // best-effort socket close

	if service == "" || !configured {
		t.Log.Warn().Str("stack", stack).Str("service", service).Bool("configured", configured).
			Msg("terminal: no service in the query, or no daemon address/token for this console")
		writeJSONFrame(client, map[string]any{"type": "error", "error": "terminal: service or daemon not configured"})
		return
	}

	daemonURL, err := terminalDaemonWSURL(apiURL, stack, service)
	if err != nil {
		t.Log.Warn().Err(err).Str("stack", stack).Msg("terminal: could not build the daemon address")
		writeJSONFrame(client, map[string]any{"type": "error", "error": err.Error()})
		return
	}

	header := http.Header{}
	header.Set("Authorization", "Bearer "+token)
	daemon, resp, err := websocket.DefaultDialer.Dial(daemonURL, header)
	if err != nil {
		// This is THE line that was missing. When the daemon refused the exec socket — a token
		// without the stack in scope, no ExecAttacher on that node, a service that is not running
		// — the browser saw a one-line status and the server recorded nothing at all.
		code := 0
		if resp != nil {
			code = resp.StatusCode
		}
		t.Log.Warn().Err(err).Int("status", code).Str("stack", stack).Str("service", service).
			Str("daemon", daemonURL).Msg("terminal: the daemon refused the exec socket")
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
			// A close code other than normal is the browser or an intermediary dropping the
			// socket — worth a line, because it is indistinguishable from "nothing happened"
			// from the operator's side.
			if websocket.IsUnexpectedCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
				t.Log.Warn().Err(err).Str("stack", stack).Str("service", service).
					Msg("terminal: the browser socket closed unexpectedly")
			}
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

// terminalDaemonShellURL is the daemon address for the on-demand shell socket.
func terminalDaemonShellURL(apiURL, serviceID, rows, cols string) string {
	u, err := url.Parse(apiURL)
	if err != nil {
		return apiURL
	}
	switch u.Scheme {
	case "http":
		u.Scheme = "ws"
	case "https":
		u.Scheme = "wss"
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/api/shell/" + url.PathEscape(serviceID) + "/exec/ws"
	q := u.Query()
	q.Set("cmd", "sh")
	if rows != "" {
		q.Set("rows", rows)
	}
	if cols != "" {
		q.Set("cols", cols)
	}
	u.RawQuery = q.Encode()
	return u.String()
}

// relaySockets copies frames in both directions until either side ends. Shared by the
// service-container relay and the shell relay so the two cannot drift.
func relaySockets(client, daemon *websocket.Conn) {
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
	for {
		mt, data, err := client.ReadMessage()
		if err != nil {
			_ = daemon.Close()
			<-done
			return
		}
		if werr := daemon.WriteMessage(mt, data); werr != nil {
			return
		}
	}
}

func writeJSONFrame(conn *websocket.Conn, v any) {
	_ = conn.WriteJSON(v)
}
