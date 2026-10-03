package docker

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/client"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runtime"
)

// miscMock serves the small stateful endpoints (info, networks, secrets,
// configs, volumes, swarm) that the shared mockDaemon does not cover. It keeps
// an in-memory map per resource kind and answers the SDK's exact request
// shapes (JSON bodies + 404 for missing).
type miscMock struct {
	// resources are keyed as kind/name → raw JSON payload. A DELETE removes
	// the entry; a GET returns it (404 when absent); a POST/PUT stores the
	// body echoed as the SDK's expected response type.
	resources map[string][]byte
	// infoBody is returned verbatim by GET /info.
	infoBody []byte
	// swarmBody is returned verbatim by GET /swarm.
	swarmBody []byte
}

func newMiscMock() *miscMock {
	return &miscMock{
		resources: map[string][]byte{},
		infoBody:  []byte(`{"Name":"mock","ServerVersion":"29.8.0","OSType":"linux","Architecture":"x86_64","NCPU":4,"MemTotal":8589934592,"Swarm":{"LocalNodeState":"active","ControlAvailable":true,"Managers":1,"Nodes":2}}`),
		swarmBody: []byte(`{"JoinTokens":{"Worker":"SWMTKN-w","Manager":"SWMTKN-m"}}`),
	}
}

func (m *miscMock) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if strings.HasPrefix(p, "/v1.") {
			rest := strings.TrimPrefix(p, "/v1.")
			if i := strings.IndexByte(rest, '/'); i >= 0 {
				p = rest[i:]
			}
		}
		r2 := r.Clone(r.Context())
		r2.URL.Path = p
		inner := http.NewServeMux()

		inner.HandleFunc("/_ping", func(w http.ResponseWriter, r *http.Request) {
			// Advertise a modern API version so version negotiation does not
			// pin the SDK to its floor (1.24) — secrets/configs need ≥1.25/1.30.
			w.Header().Set("Api-Version", "1.47")
			w.Header().Set("OSType", "linux")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("OK"))
		})

		inner.HandleFunc("/info", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(m.infoBody)
		})
		inner.HandleFunc("/swarm", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(m.swarmBody)
		})

		// /networks[/{id}] — GET inspect, DELETE. Create lives at
		// /networks/create (the SDK posts there, not to /networks).
		inner.HandleFunc("/networks", func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, `{"message":"not found"}`, 404)
		})
		inner.HandleFunc("/networks/create", func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				http.Error(w, `{"message":"method not allowed"}`, http.StatusMethodNotAllowed)
				return
			}
			var in network.CreateRequest
			_ = json.NewDecoder(r.Body).Decode(&in)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(network.CreateResponse{ID: "net-" + in.Name})
		})
		inner.HandleFunc("/networks/", func(w http.ResponseWriter, r *http.Request) {
			name := strings.TrimPrefix(r.URL.Path, "/networks/")
			if r.Method == http.MethodGet {
				body, ok := m.resources["network/"+name]
				if !ok {
					http.Error(w, `{"message":"network not found"}`, 404)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(body)
				return
			}
			if r.Method == http.MethodDelete {
				delete(m.resources, "network/"+name)
				w.WriteHeader(http.StatusNoContent)
				return
			}
			http.Error(w, `{"message":"not found"}`, 404)
		})

		// /secrets[/{id}] and /configs[/{id}] — list (GET on collection),
		// create (POST on /<kind>/create), inspect (GET on item), remove
		// (DELETE on item).
		for _, kind := range []string{"secrets", "configs"} {
			collection := kind
			itemPrefix := "/" + kind + "/"
			inner.HandleFunc("/"+collection, func(w http.ResponseWriter, r *http.Request) {
				switch r.Method {
				case http.MethodGet:
					var out []map[string]any
					for k, v := range m.resources {
						if strings.HasPrefix(k, collection+"/") {
							var one map[string]any
							_ = json.Unmarshal(v, &one)
							out = append(out, one)
						}
					}
					if out == nil {
						out = []map[string]any{}
					}
					w.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(w).Encode(out)
				default:
					http.Error(w, `{"message":"method not allowed"}`, http.StatusMethodNotAllowed)
				}
			})
			inner.HandleFunc("/"+collection+"/create", func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost {
					http.Error(w, `{"message":"method not allowed"}`, http.StatusMethodNotAllowed)
					return
				}
				var in struct {
					Name string
				}
				_ = json.NewDecoder(r.Body).Decode(&in)
				payload := []byte(`{"ID":"` + collection + `-` + in.Name + `"}`)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(payload)
			})
			inner.HandleFunc(itemPrefix, func(w http.ResponseWriter, r *http.Request) {
				name := strings.TrimPrefix(r.URL.Path, itemPrefix)
				switch r.Method {
				case http.MethodGet:
					body, ok := m.resources[collection+"/"+name]
					if !ok {
						http.Error(w, `{"message":"not found"}`, 404)
						return
					}
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write(body)
				case http.MethodDelete:
					delete(m.resources, collection+"/"+name)
					w.WriteHeader(http.StatusNoContent)
				default:
					http.Error(w, `{"message":"method not allowed"}`, http.StatusMethodNotAllowed)
				}
			})
		}

		// /volumes[/{name}] — list (GET), remove (DELETE).
		inner.HandleFunc("/volumes", func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet {
				http.Error(w, `{"message":"method not allowed"}`, http.StatusMethodNotAllowed)
				return
			}
			var vols []*volume.Volume
			for k, v := range m.resources {
				if strings.HasPrefix(k, "volume/") {
					var one volume.Volume
					_ = json.Unmarshal(v, &one)
					vols = append(vols, &one)
				}
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(volume.ListResponse{Volumes: vols})
		})
		inner.HandleFunc("/volumes/", func(w http.ResponseWriter, r *http.Request) {
			name := strings.TrimPrefix(r.URL.Path, "/volumes/")
			if r.Method != http.MethodDelete {
				http.Error(w, `{"message":"method not allowed"}`, http.StatusMethodNotAllowed)
				return
			}
			delete(m.resources, "volume/"+name)
			w.WriteHeader(http.StatusNoContent)
		})

		inner.ServeHTTP(w, r2)
	})
	return mux
}

// miscClient wires a realClient to the misc mock.
func miscClient(t *testing.T, m *miscMock) *realClient {
	t.Helper()
	srv := httptest.NewServer(m.handler())
	t.Cleanup(srv.Close)
	addr := srv.Listener.Addr().String()
	cli, err := client.NewClientWithOpts(
		client.WithHost("tcp://"+addr),
		client.WithAPIVersionNegotiation(),
		client.WithDialContext(func(ctx context.Context, _, _ string) (net.Conn, error) {
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

func TestRealClient_PingAndInfo(t *testing.T) {
	m := newMiscMock()
	rc := miscClient(t, m)
	ctx := context.Background()

	p, err := rc.Ping(ctx)
	if err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if p.OSType != "linux" {
		t.Errorf("Ping OSType = %q, want linux", p.OSType)
	}

	info, err := rc.Info(ctx)
	if err != nil {
		t.Fatalf("Info: %v", err)
	}
	if info.Name != "mock" || info.ServerVersion != "29.8.0" || info.NCPU != 4 {
		t.Errorf("Info = %+v, want mock/29.8.0/4", info)
	}
	if info.SwarmNodes != 2 || info.SwarmManagers != 1 || !info.SwarmControlAvailable {
		t.Errorf("Info swarm = %+v, want nodes 2/managers 1/control", info)
	}
}

func TestRealClient_NetworkExistsCreateRemove(t *testing.T) {
	m := newMiscMock()
	rc := miscClient(t, m)
	ctx := context.Background()

	// Not found first.
	ok, err := rc.NetworkExists(ctx, "nope")
	if err != nil {
		t.Fatalf("NetworkExists(missing): %v", err)
	}
	if ok {
		t.Error("NetworkExists(missing) = true, want false")
	}

	// Create (driver default overlay).
	if err := rc.NetworkCreate(ctx, runtime.NetworkSpec{Name: "pmcluster_net", Attachable: true}); err != nil {
		t.Fatalf("NetworkCreate: %v", err)
	}
	// Custom driver.
	if err := rc.NetworkCreate(ctx, runtime.NetworkSpec{Name: "pmcluster_net2", Driver: "bridge"}); err != nil {
		t.Fatalf("NetworkCreate(bridge): %v", err)
	}

	// Exists now.
	m.resources["network/pmcluster_net"] = []byte(`{"Name":"pmcluster_net","Driver":"overlay"}`)
	ok, err = rc.NetworkExists(ctx, "pmcluster_net")
	if err != nil {
		t.Fatalf("NetworkExists(existing): %v", err)
	}
	if !ok {
		t.Error("NetworkExists(existing) = false, want true")
	}

	// Remove idempotent (missing → nil).
	if err := rc.NetworkRemove(ctx, "pmcluster_net"); err != nil {
		t.Fatalf("NetworkRemove: %v", err)
	}
	if err := rc.NetworkRemove(ctx, "gone"); err != nil {
		t.Errorf("NetworkRemove(missing) = %v, want nil (idempotent)", err)
	}
}

func TestRealClient_SecretLifecycle(t *testing.T) {
	m := newMiscMock()
	rc := miscClient(t, m)
	ctx := context.Background()

	if ok, _ := rc.SecretExists(ctx, "db_pass"); ok {
		t.Error("SecretExists(missing) = true, want false")
	}
	m.resources["secrets/db_pass"] = []byte(`{"ID":"sec-1","Spec":{"Name":"db_pass","Labels":{"app":"demo"},"Data":"c2VjcmV0Cg=="}}`)
	if ok, _ := rc.SecretExists(ctx, "db_pass"); !ok {
		t.Error("SecretExists(existing) = false, want true")
	}

	// Inspect surfaces labels + decoded data.
	insp, err := rc.SecretInspect(ctx, "db_pass")
	if err != nil {
		t.Fatalf("SecretInspect: %v", err)
	}
	if insp.Labels["app"] != "demo" {
		t.Errorf("SecretInspect labels = %v, want app=demo", insp.Labels)
	}
	if string(insp.Data) != "secret\n" {
		t.Errorf("SecretInspect data = %q, want secret\\n", insp.Data)
	}

	// Create.
	if err := rc.SecretCreate(ctx, runtime.SecretSpec{Name: "new_secret", Data: []byte("x")}); err != nil {
		t.Fatalf("SecretCreate: %v", err)
	}

	// List filtered by label.
	names, err := rc.SecretList(ctx, "app", "demo")
	if err != nil {
		t.Fatalf("SecretList: %v", err)
	}
	if len(names) != 1 || names[0] != "db_pass" {
		t.Errorf("SecretList = %v, want [db_pass]", names)
	}

	// Remove + idempotent remove.
	if err := rc.SecretRemove(ctx, "db_pass"); err != nil {
		t.Fatalf("SecretRemove: %v", err)
	}
	if err := rc.SecretRemove(ctx, "gone"); err != nil {
		t.Errorf("SecretRemove(missing) = %v, want nil", err)
	}
}

func TestRealClient_ConfigLifecycle(t *testing.T) {
	m := newMiscMock()
	rc := miscClient(t, m)
	ctx := context.Background()

	if ok, _ := rc.ConfigExists(ctx, "pmcluster_otel_config"); ok {
		t.Error("ConfigExists(missing) = true, want false")
	}
	m.resources["configs/pmcluster_otel_config"] = []byte(`{"ID":"cfg-1","Spec":{"Name":"pmcluster_otel_config","Labels":{"io.pmcluster.managed":"true"},"Data":"b3RlbAo="}}`)
	if ok, _ := rc.ConfigExists(ctx, "pmcluster_otel_config"); !ok {
		t.Error("ConfigExists(existing) = false, want true")
	}

	insp, err := rc.ConfigInspect(ctx, "pmcluster_otel_config")
	if err != nil {
		t.Fatalf("ConfigInspect: %v", err)
	}
	if insp.Labels["io.pmcluster.managed"] != "true" {
		t.Errorf("ConfigInspect labels = %v, want managed", insp.Labels)
	}
	if string(insp.Data) != "otel\n" {
		t.Errorf("ConfigInspect data = %q, want otel\\n", insp.Data)
	}

	if err := rc.ConfigCreate(ctx, runtime.ConfigSpec{Name: "pmcluster_otel_config_v9", Data: []byte("y")}); err != nil {
		t.Fatalf("ConfigCreate: %v", err)
	}

	names, err := rc.ConfigList(ctx, "io.pmcluster.managed", "true")
	if err != nil {
		t.Fatalf("ConfigList: %v", err)
	}
	if len(names) != 1 || names[0] != "pmcluster_otel_config" {
		t.Errorf("ConfigList = %v, want [pmcluster_otel_config]", names)
	}
	// Unfiltered list.
	all, err := rc.ConfigList(ctx, "", "")
	if err != nil {
		t.Fatalf("ConfigList(unfiltered): %v", err)
	}
	if len(all) != 1 {
		t.Errorf("ConfigList(unfiltered) = %v, want 1", all)
	}

	if err := rc.ConfigRemove(ctx, "pmcluster_otel_config"); err != nil {
		t.Fatalf("ConfigRemove: %v", err)
	}
	if err := rc.ConfigRemove(ctx, "gone"); err != nil {
		t.Errorf("ConfigRemove(missing) = %v, want nil", err)
	}
}

func TestRealClient_VolumeListAndRemove(t *testing.T) {
	m := newMiscMock()
	rc := miscClient(t, m)
	ctx := context.Background()

	// Empty list.
	names, err := rc.VolumeList(ctx, runtime.StackNamespaceLabel, "demo")
	if err != nil {
		t.Fatalf("VolumeList(empty): %v", err)
	}
	if len(names) != 0 {
		t.Errorf("VolumeList(empty) = %v, want []", names)
	}

	// One volume with the stack label.
	m.resources["volume/demo_db"] = []byte(`{"Name":"demo_db"}`)
	m.resources["volume/other"] = []byte(`{"Name":"other"}`)
	names, err = rc.VolumeList(ctx, runtime.StackNamespaceLabel, "demo")
	if err != nil {
		t.Fatalf("VolumeList: %v", err)
	}
	if len(names) != 2 {
		t.Errorf("VolumeList = %v, want 2 (mock ignores the label filter)", names)
	}

	if err := rc.VolumeRemove(ctx, "demo_db"); err != nil {
		t.Fatalf("VolumeRemove: %v", err)
	}
	if err := rc.VolumeRemove(ctx, "gone"); err != nil {
		t.Errorf("VolumeRemove(missing) = %v, want nil", err)
	}
}

func TestRealClient_JoinTokens(t *testing.T) {
	m := newMiscMock()
	rc := miscClient(t, m)
	ctx := context.Background()

	toks, err := rc.JoinTokens(ctx)
	if err != nil {
		t.Fatalf("JoinTokens: %v", err)
	}
	if toks.Worker != "SWMTKN-w" || toks.Manager != "SWMTKN-m" {
		t.Errorf("JoinTokens = %+v, want worker/manager tokens", toks)
	}
}

func TestRealClient_NewFromEnv(t *testing.T) {
	// New() reads DOCKER_HOST from env. Point it at a real TCP listener so the
	// underlying SDK client construction succeeds (the adapter itself doesn't
	// dial until a method is called).
	t.Setenv("DOCKER_HOST", "tcp://127.0.0.1:1")
	rc, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer rc.Close()
	if rc == nil {
		t.Fatal("New returned nil client")
	}
}
