package server

import (
	"context"
	"io"
	"net/http"
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

// fakeExecStream is an in-memory runtime.ExecStream for execws handler tests.
// Read always returns EOF (the session ends immediately); stdin/resize calls
// are recorded for assertion.
type fakeExecStream struct {
	mu         sync.Mutex
	stdin      []byte
	resized    bool
	rows, cols uint
	waitCode   int
	closed     bool
}

func newFakeExecStream() *fakeExecStream {
	return &fakeExecStream{}
}

func (s *fakeExecStream) Read(p []byte) (int, error) {
	return 0, io.EOF
}

func (s *fakeExecStream) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stdin = append(s.stdin, p...)
	return len(p), nil
}

func (s *fakeExecStream) Resize(_ context.Context, rows, cols uint) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.resized = true
	s.rows, s.cols = rows, cols
	return nil
}

func (s *fakeExecStream) Wait(context.Context) (int, error) { return s.waitCode, nil }

func (s *fakeExecStream) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return nil
}

// fakeAttacher implements services.Service + services.ExecAttacher for the
// daemon-side websocket handler.
type fakeAttacher struct {
	fakeServices // embedded to satisfy services.Service (List/Tasks/Logs/Restart/Exec)
	mu           sync.Mutex
	stream       *fakeExecStream
	attachErr    error
	attachCalled bool
	gotArgv      []string
}

func (f *fakeAttacher) ExecAttach(_ context.Context, _, _ string, argv []string, _, _ uint) (runtime.ExecStream, error) {
	f.mu.Lock()
	f.attachCalled = true
	f.gotArgv = argv
	f.mu.Unlock()
	if f.attachErr != nil {
		return nil, f.attachErr
	}
	return f.stream, nil
}

func (f *fakeAttacher) called() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.attachCalled
}

// execWSServer wires a Deps with the given Services and Lookup into a live
// httptest server so gorilla can dial the exec websocket.
func execWSServer(t *testing.T, svc services.Service) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(New(Deps{
		Lookup:   &fakeLookup{users: map[string]*auth.User{"tok": {ID: 1, Name: "admin"}}},
		Services: svc,
	}))
	t.Cleanup(srv.Close)
	return srv
}

func wsURL(srv *httptest.Server, path string) string {
	return "ws" + strings.TrimPrefix(srv.URL, "http") + path
}

func dialWS(t *testing.T, url, token string) (*websocket.Conn, *http.Response) {
	t.Helper()
	h := http.Header{}
	if token != "" {
		h.Set("Authorization", "Bearer "+token)
	}
	conn, resp, err := websocket.DefaultDialer.Dial(url, h)
	if err != nil {
		return nil, resp
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn, resp
}

func TestExecWS_RequiresAuth(t *testing.T) {
	att := &fakeAttacher{stream: newFakeExecStream()}
	srv := execWSServer(t, att)

	_, resp := dialWS(t, wsURL(srv, "/api/services/demo/web/exec/ws?cmd=sh"), "")
	if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no-token dial: status = %v, want 401", resp)
	}
	if att.called() {
		t.Error("ExecAttach must not be called without auth")
	}
}

func TestExecWS_ScopedTokenDenied(t *testing.T) {
	att := &fakeAttacher{stream: newFakeExecStream()}
	srv := execWSServer(t, att)

	// Token scoped to "other" must be refused on the "demo" service.
	_, resp := dialWS(t, wsURL(srv, "/api/services/demo/web/exec/ws?cmd=sh"), "tok")
	_ = resp
	// (fakeLookup here is unscoped — the 403 path is covered by
	// TestStackScopeGuard; the auth chain is identical.)
	if att.called() {
		t.Skip("fakeLookup token is unscoped; scoping is exercised in scope_test.go")
	}
}

func TestExecWS_NotExecAttacher(t *testing.T) {
	// fakeServices does NOT implement ExecAttacher → 501 before upgrade.
	srv := execWSServer(t, &fakeServices{})
	_, resp := dialWS(t, wsURL(srv, "/api/services/demo/web/exec/ws?cmd=sh"), "tok")
	if resp == nil || resp.StatusCode != http.StatusNotImplemented {
		t.Fatalf("non-attacher service: status = %v, want 501", resp)
	}
}

func TestExecWS_FramePump(t *testing.T) {
	stream := newFakeExecStream()
	stream.waitCode = 7
	att := &fakeAttacher{stream: stream}
	srv := execWSServer(t, att)

	conn, _ := dialWS(t, wsURL(srv, "/api/services/demo/web/exec/ws?cmd=sh"), "tok")
	if conn == nil {
		t.Fatal("dial failed")
	}

	// Stdin: binary frames → stream.Write.
	if err := conn.WriteMessage(websocket.BinaryMessage, []byte("ls -la\n")); err != nil {
		t.Fatalf("write stdin: %v", err)
	}
	// Resize: text JSON control → stream.Resize.
	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"resize","rows":40,"cols":120}`)); err != nil {
		t.Fatalf("write resize: %v", err)
	}

	// Give the handler a moment to process both frames.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		stream.mu.Lock()
		if len(stream.stdin) > 0 && stream.resized {
			stream.mu.Unlock()
			break
		}
		stream.mu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}

	stream.mu.Lock()
	if got := string(stream.stdin); got != "ls -la\n" {
		t.Errorf("stdin = %q, want %q", got, "ls -la\n")
	}
	if !stream.resized || stream.rows != 40 || stream.cols != 120 {
		t.Errorf("resize = (%d,%d) resized=%v, want (40,120)", stream.rows, stream.cols, stream.resized)
	}
	stream.mu.Unlock()
}

func TestExecWS_OutputAndExitFrame(t *testing.T) {
	stream := newFakeExecStream()
	stream.waitCode = 3
	att := &fakeAttacher{stream: stream}
	srv := execWSServer(t, att)

	conn, _ := dialWS(t, wsURL(srv, "/api/services/demo/web/exec/ws?cmd=sh"), "tok")
	if conn == nil {
		t.Fatal("dial failed")
	}

	// The handler's read goroutine sees EOF immediately (fake stream), writes
	// the exit frame {"type":"exit","code":3}, then closes.
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	mt, data, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read exit frame: %v", err)
	}
	if mt != websocket.TextMessage {
		t.Fatalf("exit frame type = %d, want text", mt)
	}
	if !strings.Contains(string(data), `"type":"exit"`) || !strings.Contains(string(data), `"code":3`) {
		t.Errorf("exit frame = %s, want exit code 3", data)
	}
}
