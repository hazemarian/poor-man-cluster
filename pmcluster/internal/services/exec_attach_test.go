package services

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync"
	"testing"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runtime"
)

// attachStubDocker embeds the runtime.Client interface and overrides only the
// calls the interactive-exec path uses: ServiceInspect (resolution) and
// ServiceExecAttach (the TTY session). Everything else panics if reached,
// proving the adapter only touches the whitelisted surface.
type attachStubDocker struct {
	runtime.Client
	inspect     map[string]runtime.ServiceInspectResult
	stream      runtime.ExecStream
	attachCalls []struct {
		id   string
		argv []string
		rows uint
		cols uint
	}
	attachErr error
}

func (a *attachStubDocker) ServiceInspect(_ context.Context, name string) (runtime.ServiceInspectResult, error) {
	svc, ok := a.inspect[name]
	if !ok {
		return runtime.ServiceInspectResult{}, errors.New("service not found")
	}
	return svc, nil
}

func (a *attachStubDocker) ServiceExecAttach(_ context.Context, id string, argv []string, rows, cols uint) (runtime.ExecStream, error) {
	a.attachCalls = append(a.attachCalls, struct {
		id   string
		argv []string
		rows uint
		cols uint
	}{id, argv, rows, cols})
	if a.attachErr != nil {
		return nil, a.attachErr
	}
	return a.stream, nil
}

func newAttachStub() *attachStubDocker {
	return &attachStubDocker{
		inspect: map[string]runtime.ServiceInspectResult{
			"demo_web": {ID: "svc-web", Name: "demo_web", Labels: map[string]string{runtime.StackNamespaceLabel: "demo"}},
			"demo_db":  {ID: "svc-db", Name: "demo_db", Labels: map[string]string{runtime.StackNamespaceLabel: "demo"}},
			// A service that does NOT carry the stack label — resolution must refuse it.
			"rogue_web": {ID: "svc-rogue", Name: "rogue_web", Labels: map[string]string{}},
		},
	}
}

// pipeStream is an in-memory runtime.ExecStream: reads what the adapter writes,
// echoes it back (like a real TTY pty does), and records resize calls.
type pipeStream struct {
	mu       sync.Mutex
	in       *io.PipeReader
	out      *io.PipeWriter
	resized  []struct{ rows, cols uint }
	waitCode int
	closed   bool
}

func newPipeStream() *pipeStream {
	pr, pw := io.Pipe()
	return &pipeStream{in: pr, out: pw}
}

func (p *pipeStream) Read(b []byte) (int, error) { return p.in.Read(b) }

func (p *pipeStream) Write(b []byte) (int, error) {
	// Echo the bytes back so the "session" is observable from the read side.
	go func() { _, _ = p.out.Write(b) }()
	return len(b), nil
}

func (p *pipeStream) Resize(_ context.Context, rows, cols uint) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.resized = append(p.resized, struct{ rows, cols uint }{rows, cols})
	return nil
}

func (p *pipeStream) Wait(context.Context) (int, error) { return p.waitCode, nil }

func (p *pipeStream) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = true
	_ = p.in.Close()
	return nil
}

// TestExecAttach_ArgvValidation proves the interactive path enforces the same
// argv contract as the non-interactive Exec (and rejects empty argv).
func TestExecAttach_ArgvValidation(t *testing.T) {
	l := Local{Docker: newAttachStub()}
	ctx := context.Background()

	if _, err := l.ExecAttach(ctx, "demo", "web", nil, 30, 100); err == nil {
		t.Fatal("expected empty-argv error, got nil")
	}
	tooMany := make([]string, 17)
	for i := range tooMany {
		tooMany[i] = "x"
	}
	if _, err := l.ExecAttach(ctx, "demo", "web", tooMany, 30, 100); err == nil {
		t.Fatal("expected >16 argv error, got nil")
	}
	if _, err := l.ExecAttach(ctx, "demo", "web", []string{"sh", string(bytes.Repeat([]byte("a"), 201))}, 30, 100); err == nil {
		t.Fatal("expected >200-byte arg error, got nil")
	}
	if _, err := l.ExecAttach(ctx, "demo", "web", []string{"sh"}, 0, 0); err != nil {
		t.Fatalf("valid argv rejected: %v", err)
	}
}

// TestExecAttach_ResolutionAndStream wires the full local-adapter path: stack
// validation, namespace-label verification, ServiceExecAttach delegation, and
// stream usability (echo round trip + resize + wait + close).
func TestExecAttach_ResolutionAndStream(t *testing.T) {
	stub := newAttachStub()
	stub.stream = newPipeStream()
	l := Local{Docker: stub}

	// Invalid stack name is refused before any Docker call.
	if _, err := l.ExecAttach(context.Background(), "Bad_Stack", "web", []string{"sh"}, 30, 100); err == nil {
		t.Fatal("expected bad-stack-name error")
	}

	// A service that does not carry the stack's namespace label is refused.
	if _, err := l.ExecAttach(context.Background(), "rogue", "web", []string{"sh"}, 30, 100); err == nil {
		t.Fatal("expected cross-stack refusal for rogue_web")
	}

	// Success: argv + size forwarded to the docker client.
	s, err := l.ExecAttach(context.Background(), "demo", "web", []string{"sh", "-c", "echo hi"}, 33, 120)
	if err != nil {
		t.Fatalf("ExecAttach: %v", err)
	}
	defer s.Close()
	if len(stub.attachCalls) != 1 {
		t.Fatalf("attach calls = %d, want 1", len(stub.attachCalls))
	}
	call := stub.attachCalls[0]
	if call.id != "svc-web" {
		t.Errorf("attach target id = %q, want svc-web", call.id)
	}
	if call.argv[0] != "sh" || call.argv[1] != "-c" || call.argv[2] != "echo hi" {
		t.Errorf("attach argv = %v", call.argv)
	}
	if call.rows != 33 || call.cols != 120 {
		t.Errorf("attach size = %dx%d, want 33x120", call.rows, call.cols)
	}

	// The stream is live: writes echo back, resize records, Wait returns code.
	if _, err := s.Write([]byte("hello")); err != nil {
		t.Fatalf("stream write: %v", err)
	}
	buf := make([]byte, 16)
	n, err := s.Read(buf)
	if err != nil {
		t.Fatalf("stream read: %v", err)
	}
	if string(buf[:n]) != "hello" {
		t.Errorf("stream echo = %q, want %q", buf[:n], "hello")
	}
	if err := s.Resize(context.Background(), 50, 200); err != nil {
		t.Fatalf("stream resize: %v", err)
	}
	ps := stub.stream.(*pipeStream)
	ps.mu.Lock()
	if len(ps.resized) != 1 || ps.resized[0].rows != 50 || ps.resized[0].cols != 200 {
		t.Errorf("resize recorded = %+v", ps.resized)
	}
	ps.mu.Unlock()
	if code, err := s.Wait(context.Background()); err != nil || code != 0 {
		t.Errorf("Wait = (%d, %v), want (0, nil)", code, err)
	}
}

// TestExecAttach_ErrorPropagation proves a docker-side attach failure surfaces
// verbatim (the handler turns it into a WS close frame).
func TestExecAttach_ErrorPropagation(t *testing.T) {
	stub := newAttachStub()
	stub.attachErr = errors.New("task not running on this node")
	l := Local{Docker: stub}
	_, err := l.ExecAttach(context.Background(), "demo", "web", []string{"sh"}, 30, 100)
	if err == nil || err.Error() != "task not running on this node" {
		t.Fatalf("error = %v, want the docker error surfaced", err)
	}
}
