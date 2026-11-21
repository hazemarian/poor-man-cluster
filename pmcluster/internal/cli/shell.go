package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// shellCmd opens an interactive session in a stack's busy box WITHOUT the browser.
//
// The console already does this: it mints a two-minute ticket and relays the socket. An operator on a
// manager, already in a terminal with ssh open, had to open a browser to get a shell the CLI could
// reach — this is that path, over the same endpoints, with nothing new on the server.
func newShellCmd() *cobra.Command {
	var (
		command string
		image   string
	)
	cmd := &cobra.Command{
		Use:   "shell <stack>",
		Short: "Open an interactive shell in a stack's busy box",
		Long: `Open an interactive shell in a throwaway container that has the stack's secrets
mounted and joins the stack's networks.

The session is destroyed when the shell exits or after 30 minutes, whichever comes first. /tmp inside
it is a 200 MiB tmpfs, so a database dump is streamed, not stored:

  curl -fsSL <url> | pg_restore -h <stack>_db -U <user> -d <db>`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runShell(cmd, args[0], command, image)
		},
	}
	cmd.Flags().StringVar(&command, "cmd", "sh", "command to run in the shell")
	cmd.Flags().StringVar(&image, "image", "", "override the shell image for this session")
	return cmd
}

// shellAPIBase resolves where the daemon is. Remote mode uses --api-url; on a manager the daemon answers
// on loopback, which is the case this command exists for.
func shellAPIBase() string {
	if apiURL != "" {
		return strings.TrimRight(apiURL, "/")
	}
	return "http://127.0.0.1:9090"
}

func shellAPIToken() string {
	if apiToken != "" {
		return apiToken
	}
	return os.Getenv("PMCLUSTER_API_TOKEN")
}

type shellSession struct {
	ServiceID  string `json:"service_id"`
	MaxSeconds int    `json:"max_seconds"`
	Node       string `json:"node"`
}

func runShell(cmd *cobra.Command, stack, runCmd, image string) error {
	token := shellAPIToken()
	if token == "" {
		return fmt.Errorf("no API token: the shell is served by the daemon, so set --api-token or PMCLUSTER_API_TOKEN")
	}
	base := shellAPIBase()
	ctx := cmd.Context()

	sess, err := openShellSession(ctx, base, token, stack, image)
	if err != nil {
		return err
	}
	// Always end the session: the daemon removes the service when the socket closes, and this is the
	// path that makes "close the tab" and "exit the shell" the same thing.
	defer func() { _ = closeShellSession(context.Background(), base, token, sess.ServiceID) }()

	rows, cols := terminalSize()
	wsURL := strings.Replace(base, "http", "ws", 1) + "/api/shell/" + sess.ServiceID + "/exec/ws" +
		fmt.Sprintf("?cmd=%s&rows=%d&cols=%d", runCmd, rows, cols)

	conn, err := dialShellWS(wsURL, token)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()

	fmt.Fprintf(os.Stderr, "connected to %s on %s (max %ds) — exit the shell or press Ctrl-] to close\n",
		stack, sess.Node, sess.MaxSeconds)

	return pumpTerminal(conn, rows, cols)
}

func openShellSession(ctx context.Context, base, token, stack, image string) (*shellSession, error) {
	body := "{}"
	if image != "" {
		body = fmt.Sprintf(`{"image":%q}`, image)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/api/stacks/"+stack+"/shell", strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("create shell session: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("create shell session: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var sess shellSession
	if err := json.Unmarshal(raw, &sess); err != nil {
		return nil, fmt.Errorf("create shell session: unparseable response %q: %w", strings.TrimSpace(string(raw)), err)
	}
	if sess.ServiceID == "" {
		return nil, fmt.Errorf("create shell session: the daemon returned no service id: %s", strings.TrimSpace(string(raw)))
	}
	return &sess, nil
}

func closeShellSession(ctx context.Context, base, token, id string) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, base+"/api/shells/"+id, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	return nil
}

func dialShellWS(url, token string) (*websocket.Conn, error) {
	h := http.Header{}
	h.Set("Authorization", "Bearer "+token)
	conn, resp, err := websocket.DefaultDialer.Dial(url, h)
	if err != nil {
		if resp != nil {
			return nil, fmt.Errorf("shell socket: HTTP %d (%w)", resp.StatusCode, err)
		}
		return nil, fmt.Errorf("shell socket: %w", err)
	}
	return conn, nil
}

// pumpTerminal is the raw-mode loop: stdin → binary frames, socket output → stdout, a resize frame on
// SIGWINCH, and Ctrl-] or the socket closing ends it.
func pumpTerminal(conn *websocket.Conn, rows, cols int) error {
	fd := int(os.Stdin.Fd())
	raw := false
	if term.IsTerminal(fd) {
		old, err := term.MakeRaw(fd)
		if err == nil {
			raw = true
			defer func() { _ = term.Restore(fd, old) }()
		}
	}

	done := make(chan struct{})
	var readErr error
	go func() { // socket → stdout
		defer close(done)
		for {
			mt, data, err := conn.ReadMessage()
			if err != nil {
				readErr = err
				return
			}
			if mt == websocket.BinaryMessage || mt == websocket.TextMessage {
				_, _ = os.Stdout.Write(data)
			}
		}
	}()

	go func() { // stdin → socket
		buf := make([]byte, 4096)
		for {
			n, err := os.Stdin.Read(buf)
			if n > 0 {
				if buf[0] == 0x1d { // Ctrl-] : leave
					_ = conn.Close()
					return
				}
				if werr := conn.WriteMessage(websocket.BinaryMessage, buf[:n]); werr != nil {
					return
				}
			}
			if err != nil {
				_ = conn.Close()
				return
			}
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGWINCH)
	defer signal.Stop(sig)
	go func() {
		for range sig {
			r, c := terminalSize()
			if r != rows || c != cols {
				rows, cols = r, c
				msg, _ := json.Marshal(map[string]any{"type": "resize", "rows": r, "cols": c})
				_ = conn.WriteMessage(websocket.TextMessage, msg)
			}
		}
	}()

	<-done
	if readErr != nil && !websocket.IsCloseError(readErr, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
		if raw {
			fmt.Fprintln(os.Stderr)
		}
		return fmt.Errorf("shell closed: %w", readErr)
	}
	return nil
}

func terminalSize() (rows, cols int) {
	if w, h, err := term.GetSize(int(os.Stdout.Fd())); err == nil && w > 0 && h > 0 {
		return h, w
	}
	return 24, 80
}
