package docker

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/docker/docker/api/types/swarm"
	"github.com/docker/docker/client"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runtime"
)

// mockDaemon is a minimal in-process Docker Engine API server used to exercise
// the realClient adapter against real HTTP request/response handling. It only
// implements the endpoints the tested methods touch — anything else yields 404.
type mockDaemon struct {
	mu       sync.Mutex
	services []swarm.Service
	tasks    map[string][]swarm.Task
	nodes    []swarm.Node
	// logBody is returned (raw, undemultiplexed) by the logs endpoint.
	logBody []byte
	// updateForce tracks ServiceRestart calls per service ID.
	updateForce map[string]int
	// execResults tracks ContainerExecCreate ids → exit codes.
	execExit map[string]int
}

func newMockDaemon() *mockDaemon {
	return &mockDaemon{
		tasks:       map[string][]swarm.Task{},
		updateForce: map[string]int{},
		execExit:    map[string]int{},
	}
}

func (m *mockDaemon) handler() http.Handler {
	mux := http.NewServeMux()
	// The Docker SDK prefixes every request with the negotiated API version
	// (e.g. /v1.44/services). Strip it before routing so the mux only sees
	// the version-less path.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if strings.HasPrefix(p, "/v1.") {
			rest := strings.TrimPrefix(p, "/v1.")
			// rest looks like "44/services" — cut up to the first '/'.
			if i := strings.IndexByte(rest, '/'); i >= 0 {
				p = rest[i:]
			}
		}
		r2 := r.Clone(r.Context())
		r2.URL.Path = p
		inner := http.NewServeMux()
		inner.HandleFunc("/_ping", func(w http.ResponseWriter, r *http.Request) {
			if v := r.Header.Get("Api-Version"); v != "" {
				w.Header().Set("Api-Version", v)
			}
			w.Header().Set("OSType", "linux")
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, "OK")
		})
		inner.HandleFunc("/services", func(w http.ResponseWriter, r *http.Request) {
			m.mu.Lock()
			defer m.mu.Unlock()
			if err := json.NewEncoder(w).Encode(m.services); err != nil {
				http.Error(w, err.Error(), 500)
			}
		})
		inner.HandleFunc("/services/", func(w http.ResponseWriter, r *http.Request) {
			parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/services/"), "/")
			id := parts[0]
			m.mu.Lock()
			defer m.mu.Unlock()

			switch {
			case len(parts) == 1 && r.Method == http.MethodGet:
				for i := range m.services {
					if m.services[i].ID == id || m.services[i].Spec.Name == id {
						raw, _ := json.Marshal(m.services[i])
						w.Header().Set("Content-Type", "application/json")
						w.Write(raw)
						return
					}
				}
				http.Error(w, `{"message":"service not found"}`, 404)
			case len(parts) >= 2 && parts[1] == "tasks" && r.Method == http.MethodGet:
				_ = json.NewEncoder(w).Encode(m.tasks[id])
			case len(parts) >= 2 && parts[1] == "logs" && r.Method == http.MethodGet:
				w.Write(m.logBody)
			case len(parts) >= 2 && parts[1] == "update" && r.Method == http.MethodPost:
				m.updateForce[id]++
				w.WriteHeader(http.StatusOK)
				_ = json.NewEncoder(w).Encode(map[string][]string{"Warnings": {}})
			default:
				http.Error(w, `{"message":"not found"}`, 404)
			}
		})
		inner.HandleFunc("/nodes", func(w http.ResponseWriter, r *http.Request) {
			m.mu.Lock()
			defer m.mu.Unlock()
			if err := json.NewEncoder(w).Encode(m.nodes); err != nil {
				http.Error(w, err.Error(), 500)
			}
		})
		inner.HandleFunc("/tasks", func(w http.ResponseWriter, r *http.Request) {
			m.mu.Lock()
			defer m.mu.Unlock()
			// SDK filters by service via query params; collect all mock tasks
			// (each test seeds tasks only for the service under test).
			var all []swarm.Task
			for _, ts := range m.tasks {
				all = append(all, ts...)
			}
			if err := json.NewEncoder(w).Encode(all); err != nil {
				http.Error(w, err.Error(), 500)
			}
		})
		inner.HandleFunc("/containers/", func(w http.ResponseWriter, r *http.Request) {
			m.mu.Lock()
			defer m.mu.Unlock()
			raw, _ := json.Marshal(map[string]string{"Id": "exec-1"})
			w.Header().Set("Content-Type", "application/json")
			w.Write(raw)
		})
		inner.HandleFunc("/exec/", func(w http.ResponseWriter, r *http.Request) {
			parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/exec/"), "/")
			id := parts[0]
			m.mu.Lock()
			defer m.mu.Unlock()
			if len(parts) == 2 && parts[1] == "json" {
				raw, _ := json.Marshal(map[string]any{
					"ID":         id,
					"Running":    false,
					"ExitCode":   m.execExit[id],
					"Pid":        0,
					"OpenStdin":  false,
					"OpenStderr": true,
					"OpenStdout": true,
				})
				w.Header().Set("Content-Type", "application/json")
				w.Write(raw)
				return
			}
			if len(parts) == 2 && parts[1] == "start" {
				// ContainerExecAttach hijacks the connection. Emulate the
				// Docker daemon: 101 Switching Protocols, then a single
				// stdcopy stdout frame ("root\n").
				hj, ok := w.(http.Hijacker)
				if !ok {
					http.Error(w, "hijacking not supported", 500)
					return
				}
				conn, brw, err := hj.Hijack()
				if err != nil {
					http.Error(w, err.Error(), 500)
					return
				}
				_, _ = brw.WriteString("HTTP/1.1 101 UPGRADED\r\nContent-Type: application/vnd.docker.raw-stream\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n")
				if err := brw.Flush(); err != nil {
					_ = conn.Close()
					return
				}
				out := "root\n"
				frame := make([]byte, 8+len(out))
				frame[0] = 1 // stdout
				binary.BigEndian.PutUint32(frame[4:8], uint32(len(out)))
				copy(frame[8:], out)
				_, _ = conn.Write(frame)
				// CloseWrite sends a clean FIN: the client's StdCopy sees
				// EOF and returns. Closing the whole socket while the
				// client still has unread buffered data would surface as
				// "connection reset by peer" — timing-dependent and flaky
				// under -race and in CI. The FIN handshake makes teardown
				// deterministic on every host.
				if tcp, ok := conn.(interface{ CloseWrite() error }); ok {
					_ = tcp.CloseWrite()
				}
				time.Sleep(50 * time.Millisecond)
				_ = conn.Close()
				return
			}
			w.WriteHeader(http.StatusOK)
		})
		inner.ServeHTTP(w, r2)
	})
	return mux
}

// realClientFromServer wires a realClient to the mock daemon.
func realClientFromServer(t *testing.T, m *mockDaemon) *realClient {
	t.Helper()
	srv := httptest.NewServer(m.handler())
	t.Cleanup(srv.Close)
	addr := srv.Listener.Addr().String()
	cli, err := client.NewClientWithOpts(
		client.WithHost("tcp://"+addr),
		client.WithAPIVersionNegotiation(),
		// ContainerExecAttach hijacks the connection: the SDK dials with
		// the URL scheme as the network, so a bare http:// host fails with
		// 'dial http: unknown network http'. Route every dial to the mock.
		client.WithDialContext(func(ctx context.Context, network, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "tcp", addr)
		}),
	)
	if err != nil {
		t.Fatalf("sdk client: %v", err)
	}
	t.Cleanup(func() { _ = cli.Close() })
	return &realClient{c: cli}
}

func TestRealClient_ServiceListAndStackSecretNames(t *testing.T) {
	m := newMockDaemon()
	m.services = []swarm.Service{
		{
			ID: "svc-1",
			Spec: swarm.ServiceSpec{
				Annotations: swarm.Annotations{
					Name: "demo_web",
					Labels: map[string]string{
						"com.docker.stack.namespace": "demo",
					},
				},
				TaskTemplate: swarm.TaskSpec{
					ContainerSpec: &swarm.ContainerSpec{
						Image: "nginx:latest",
						Secrets: []*swarm.SecretReference{
							{SecretName: "demo_db_pass"},
						},
					},
				},
				Mode: swarm.ServiceMode{
					Replicated: &swarm.ReplicatedService{Replicas: uint64Ptr(2)},
				},
			},
			Meta:          swarm.Meta{Version: swarm.Version{Index: 7}},
			ServiceStatus: &swarm.ServiceStatus{RunningTasks: 2, DesiredTasks: 2},
		},
		{
			ID: "svc-2",
			Spec: swarm.ServiceSpec{
				Annotations: swarm.Annotations{
					Name: "other_app",
					Labels: map[string]string{
						"com.docker.stack.namespace": "other",
					},
				},
				TaskTemplate: swarm.TaskSpec{
					ContainerSpec: &swarm.ContainerSpec{
						Image: "redis:7",
					},
				},
				Mode: swarm.ServiceMode{
					Global: &swarm.GlobalService{},
				},
			},
			ServiceStatus: &swarm.ServiceStatus{RunningTasks: 3, DesiredTasks: 3},
		},
	}
	rc := realClientFromServer(t, m)

	svcs, err := rc.ServiceList(context.Background())
	if err != nil {
		t.Fatalf("ServiceList: %v", err)
	}
	if len(svcs) != 2 {
		t.Fatalf("want 2 services, got %d", len(svcs))
	}
	var web *runtime.Service
	for i := range svcs {
		if svcs[i].Name == "demo_web" {
			web = &svcs[i]
		}
	}
	if web == nil {
		t.Fatal("demo_web not found")
	}
	if web.Stack != "demo" || web.Image != "nginx:latest" || web.Mode != "replicated" {
		t.Errorf("unexpected demo_web: stack=%q image=%q mode=%q", web.Stack, web.Image, web.Mode)
	}
	if web.Desired != 2 {
		t.Errorf("demo_web desired replicas = %d, want 2", web.Desired)
	}
	// Global mode: desired = RunningTasks.
	var glob runtime.Service
	for i := range svcs {
		if svcs[i].Name == "other_app" {
			glob = svcs[i]
		}
	}
	if glob.Desired != 3 || glob.Mode != "global" {
		t.Errorf("other_app: desired=%d mode=%q, want 3/global", glob.Desired, glob.Mode)
	}

	names, err := rc.StackSecretNames(context.Background(), "demo")
	if err != nil {
		t.Fatalf("StackSecretNames: %v", err)
	}
	if len(names) != 1 || names[0] != "demo_db_pass" {
		t.Errorf("stack secrets = %v, want [demo_db_pass]", names)
	}
}

func TestRealClient_ServiceRestart(t *testing.T) {
	m := newMockDaemon()
	m.services = []swarm.Service{{
		ID: "svc-1",
		Spec: swarm.ServiceSpec{
			Annotations: swarm.Annotations{Name: "demo_web"},
			TaskTemplate: swarm.TaskSpec{
				ContainerSpec: &swarm.ContainerSpec{Image: "nginx:latest"},
			},
		},
		Meta: swarm.Meta{Version: swarm.Version{Index: 5}},
	}}
	rc := realClientFromServer(t, m)

	if err := rc.ServiceRestart(context.Background(), "demo_web"); err != nil {
		t.Fatalf("ServiceRestart: %v", err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.updateForce["svc-1"] != 1 {
		t.Errorf("update force calls = %d, want 1", m.updateForce["svc-1"])
	}
}

func TestRealClient_ServiceInspectNotFound(t *testing.T) {
	m := newMockDaemon()
	m.services = []swarm.Service{}
	rc := realClientFromServer(t, m)
	if _, err := rc.ServiceInspect(context.Background(), "ghost"); err == nil {
		t.Fatal("expected error for unknown service")
	}
}

func TestRealClient_ServiceTasks(t *testing.T) {
	m := newMockDaemon()
	m.services = []swarm.Service{{
		ID:   "svc-1",
		Spec: swarm.ServiceSpec{Annotations: swarm.Annotations{Name: "demo_web"}},
	}}
	m.nodes = []swarm.Node{
		{ID: "node-a", Description: swarm.NodeDescription{Hostname: "node-a"}},
	}
	finished := time.Unix(100, 0)
	m.tasks["svc-1"] = []swarm.Task{{
		ID:     "task-1",
		NodeID: "node-a",
		Slot:   2,
		Status: swarm.TaskStatus{
			State:     swarm.TaskStateRunning,
			Timestamp: finished,
		},
	}}
	rc := realClientFromServer(t, m)

	tasks, err := rc.ServiceTasks(context.Background(), "demo_web")
	if err != nil {
		t.Fatalf("ServiceTasks: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("want 1 task, got %d", len(tasks))
	}
	tt := tasks[0]
	if tt.TaskID != "task-1" || tt.Node != "node-a" || tt.Slot != 2 || tt.State != "running" {
		t.Errorf("unexpected task: %+v", tt)
	}
	if tt.StartedAt != 100 {
		t.Errorf("StartedAt = %d, want 100", tt.StartedAt)
	}
}

func TestRealClient_ServiceLogsDemux(t *testing.T) {
	m := newMockDaemon()
	m.services = []swarm.Service{{
		ID:   "svc-1",
		Spec: swarm.ServiceSpec{Annotations: swarm.Annotations{Name: "demo_web"}},
	}}
	// Two 8-byte-frame stdout lines: "hello\n" + "world\n".
	frame := func(stream byte, msg string) []byte {
		b := make([]byte, 8+len(msg))
		b[0] = stream
		binary.BigEndian.PutUint32(b[4:8], uint32(len(msg)))
		copy(b[8:], msg)
		return b
	}
	m.logBody = append(frame(1, "hello\n"), frame(1, "world\n")...)
	rc := realClientFromServer(t, m)

	lines, err := rc.ServiceLogs(context.Background(), "demo_web", 10)
	if err != nil {
		t.Fatalf("ServiceLogs: %v", err)
	}
	if len(lines) != 2 || lines[0].Line != "hello" || lines[1].Line != "world" {
		t.Errorf("demuxed lines = %+v, want [hello world]", lines)
	}
	for i := range lines {
		if lines[i].Stream != "stdout" {
			t.Errorf("line %d stream = %q, want stdout", i, lines[i].Stream)
		}
	}
}

func TestRealClient_ServiceExec(t *testing.T) {
	m := newMockDaemon()
	m.services = []swarm.Service{{
		ID:   "svc-1",
		Spec: swarm.ServiceSpec{Annotations: swarm.Annotations{Name: "demo_web"}},
	}}
	now := time.Now()
	m.tasks["svc-1"] = []swarm.Task{{
		ID: "task-1",
		Status: swarm.TaskStatus{
			State:           swarm.TaskStateRunning,
			Timestamp:       now,
			ContainerStatus: &swarm.ContainerStatus{ContainerID: "cont-1"},
		},
	}}
	m.execExit["exec-1"] = 0
	rc := realClientFromServer(t, m)

	res, err := rc.ServiceExec(context.Background(), "demo_web", []string{"whoami"})
	if err != nil {
		t.Fatalf("ServiceExec: %v", err)
	}
	if res.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", res.ExitCode)
	}
}

func TestIsNotFoundString(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"Error: No such service: ghost", true},
		{"Error response from daemon: service demo_web not found", true},
		{"some other error", false},
		{"", false},
	}
	for _, c := range cases {
		if got := isNotFoundString(fmt.Errorf("%s", c.in)); got != c.want {
			t.Errorf("isNotFoundString(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestIdempotentRemove(t *testing.T) {
	if err := idempotentRemove(nil, "secret", "x"); err != nil {
		t.Errorf("nil error should pass through as nil: %v", err)
	}
	if err := idempotentRemove(fmt.Errorf("Error: No such secret: x"), "secret", "x"); err != nil {
		t.Errorf("not-found error should map to nil: %v", err)
	}
	if err := idempotentRemove(fmt.Errorf("boom"), "secret", "x"); err == nil {
		t.Error("unrelated error should propagate")
	}
}

func uint64Ptr(v uint64) *uint64 { return &v }
