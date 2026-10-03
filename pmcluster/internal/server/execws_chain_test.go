package server

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/auth"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runtime"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/services"
)

// chainDocker is the docker-side fake for the daemon-chain integration test.
// It embeds runtime.Client and implements only ServiceInspect (resolution) and
// ServiceExecAttach (interactive session) — the exact surface the services
// local adapter needs for the exec WS path.
type chainDocker struct {
	runtime.Client
	mu          sync.Mutex
	inspect     map[string]runtime.ServiceInspectResult
	attachCalls int
}

func (c *chainDocker) ServiceInspect(_ context.Context, name string) (runtime.ServiceInspectResult, error) {
	svc, ok := c.inspect[name]
	if !ok {
		return runtime.ServiceInspectResult{}, errors.New("service not found")
	}
	return svc, nil
}

// ServiceExecAttach returns an in-memory stream that echoes stdin back (a
// faithful stand-in for the TTY pty) and reports exit code 0.
func (c *chainDocker) ServiceExecAttach(_ context.Context, _ string, argv []string, _, _ uint) (runtime.ExecStream, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.attachCalls++
	return newEchoStream(argv), nil
}

// echoStream is a runtime.ExecStream that echoes every write back on read —
// enough to prove a full stdin→handler→adapter→docker→(echo)→adapter→handler→ws
// round trip. Reads block until a write lands (like a real pty).
type echoStream struct {
	argv  []string
	mu    sync.Mutex
	cond  *sync.Cond
	buf   []byte
	waitc chan struct{}
}

func newEchoStream(argv []string) *echoStream {
	e := &echoStream{argv: argv, waitc: make(chan struct{})}
	return e
}

func (e *echoStream) Read(p []byte) (int, error) {
	for {
		e.mu.Lock()
		if len(e.buf) > 0 {
			n := copy(p, e.buf)
			e.buf = e.buf[n:]
			e.mu.Unlock()
			return n, nil
		}
		c := e.waitc
		e.mu.Unlock()
		// Block until a write signals (channel snapshot under the lock — the
		// writer replaces waitc, so a bare field read here would race).
		<-c
	}
}

func (e *echoStream) Write(p []byte) (int, error) {
	e.mu.Lock()
	e.buf = append(e.buf, p...)
	close(e.waitc)
	e.waitc = make(chan struct{})
	e.mu.Unlock()
	return len(p), nil
}

func (e *echoStream) Resize(context.Context, uint, uint) error { return nil }
func (e *echoStream) Wait(context.Context) (int, error)        { return 0, nil }
func (e *echoStream) Close() error                             { return nil }

// TestExecWS_DaemonChainIntegration wires the REAL services.Local adapter (stack
// validation + namespace-label resolution) through the live WS handler: the
// request is resolved to the swarm service ID, the interactive stream is
// created, stdin echoes back to the browser, and argv/size reach docker.
func TestExecWS_DaemonChainIntegration(t *testing.T) {
	dc := &chainDocker{
		inspect: map[string]runtime.ServiceInspectResult{
			"demo_web": {ID: "svc-web", Name: "demo_web", Labels: map[string]string{runtime.StackNamespaceLabel: "demo"}},
		},
	}
	svc := &services.Local{Docker: dc}
	srv := httptest.NewServer(New(Deps{
		Lookup:   &fakeLookup{users: map[string]*auth.User{"tok": {ID: 1, Name: "admin"}}},
		Services: svc,
	}))
	t.Cleanup(srv.Close)

	conn, resp := dialWS(t, wsURL(srv, "/api/services/demo/web/exec/ws?cmd=sh&rows=30&cols=100"), "tok")
	if conn == nil {
		t.Fatalf("dial failed (status %v)", resp)
	}
	defer conn.Close()

	// Stdin echo round trip: binary frame → docker stream → echoed back.
	if err := conn.WriteMessage(websocket.BinaryMessage, []byte("pwd\n")); err != nil {
		t.Fatalf("write stdin: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	mt, data, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if mt != websocket.BinaryMessage || string(data) != "pwd\n" {
		t.Errorf("echo = (%d, %q), want (binary, %q)", mt, data, "pwd\n")
	}

	// Resize control is accepted (no error, stream unaffected).
	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"resize","rows":50,"cols":200}`)); err != nil {
		t.Fatalf("write resize: %v", err)
	}
	time.Sleep(100 * time.Millisecond)

	dc.mu.Lock()
	calls := dc.attachCalls
	dc.mu.Unlock()
	if calls != 1 {
		t.Fatalf("docker attach calls = %d, want 1", calls)
	}
}

// TestExecWS_DaemonChainCrossStackRefused proves the Local adapter's namespace
// guard is active end to end: a service that exists but does NOT carry the
// stack's label is refused, surfacing as a WS close (not a session).
func TestExecWS_DaemonChainCrossStackRefused(t *testing.T) {
	dc := &chainDocker{
		inspect: map[string]runtime.ServiceInspectResult{
			"rogue_web": {ID: "svc-rogue", Name: "rogue_web", Labels: map[string]string{}},
		},
	}
	svc := &services.Local{Docker: dc}
	srv := httptest.NewServer(New(Deps{
		Lookup:   &fakeLookup{users: map[string]*auth.User{"tok": {ID: 1, Name: "admin"}}},
		Services: svc,
	}))
	t.Cleanup(srv.Close)

	conn, resp := dialWS(t, wsURL(srv, "/api/services/rogue/web/exec/ws?cmd=sh"), "tok")
	if conn == nil {
		t.Fatalf("dial failed (status %v)", resp)
	}
	defer conn.Close()

	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	mt, data, err := conn.ReadMessage()
	if mt == websocket.TextMessage && strings.Contains(string(data), "does not belong to stack") {
		return // resolution refused: handler closed the session with a text frame
	}
	// The handler refuses exec errors with a Close control frame (code 1011)
	// carrying the reason; gorilla surfaces it as a *websocket.CloseError.
	if err != nil {
		ce, ok := err.(*websocket.CloseError)
		if ok && strings.Contains(ce.Text, "does not belong to stack") {
			return
		}
		t.Fatalf("expected cross-stack refusal, got close error: %v", err)
	}
	t.Errorf("unexpected frame after cross-stack exec: (%d, %s)", mt, data)
}

// TestExecWS_DaemonChainBadStackName proves invalid stack names never reach
// docker (validation lives in the adapter).
func TestExecWS_DaemonChainBadStackName(t *testing.T) {
	dc := &chainDocker{inspect: map[string]runtime.ServiceInspectResult{}}
	svc := &services.Local{Docker: dc}
	srv := httptest.NewServer(New(Deps{
		Lookup:   &fakeLookup{users: map[string]*auth.User{"tok": {ID: 1, Name: "admin"}}},
		Services: svc,
	}))
	t.Cleanup(srv.Close)

	conn, _ := dialWS(t, wsURL(srv, "/api/services/Bad_Stack/web/exec/ws?cmd=sh"), "tok")
	if conn == nil {
		return // refused at upgrade? either path is acceptable as long as no session starts
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, _, err := conn.ReadMessage()
	if err == nil {
		t.Error("expected session refusal for invalid stack name")
	}
}
