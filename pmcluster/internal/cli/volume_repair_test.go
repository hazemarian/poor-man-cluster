package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/rs/zerolog"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/cluster"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/manifest"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runtime"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
)

type repairDocker struct {
	runtime.Client
	services []runtime.Service
	mounts   map[string][]runtime.Mount
	volumes  map[string]runtime.Volume
	listErr  error
}

func (r repairDocker) ServiceList(context.Context) ([]runtime.Service, error) {
	if r.listErr != nil {
		return nil, r.listErr
	}
	return r.services, nil
}

func (r repairDocker) ServiceInspect(_ context.Context, name string) (runtime.ServiceInspectResult, error) {
	return runtime.ServiceInspectResult{ID: name, Mounts: r.mounts[name]}, nil
}

func (r repairDocker) VolumeInspect(_ context.Context, name string) (runtime.Volume, error) {
	if v, ok := r.volumes[name]; ok {
		return v, nil
	}
	return runtime.Volume{}, errors.New("no such volume: " + name)
}

type repairForcer struct{ forced []string }

func (f *repairForcer) ForceUpdateService(_ context.Context, fullName string) error {
	f.forced = append(f.forced, fullName)
	return nil
}

func TestRepairLocalVolumeDirs_CreatesAndForces(t *testing.T) {
	host, err := os.Hostname()
	if err != nil {
		t.Fatalf("hostname: %v", err)
	}
	root := t.TempDir()
	source := filepath.Join(root, "demo", "db_data")
	extra := filepath.Join(root, "demo", "extra")
	dc := repairDocker{
		services: []runtime.Service{
			{ID: "s1", Name: "demo_db", Node: host},
			{ID: "s2", Name: "other_db", Node: "some-other-node"},
			{ID: "s3", Name: "platform_agent", Node: ""},
		},
		mounts: map[string][]runtime.Mount{
			// The realistic stateful shape: a named volume whose local-driver
			// device (the host directory) is missing, plus a raw bind and two
			// mounts the repair must ignore.
			"s1": {
				{Type: "volume", Source: "demo_db_data", Target: "/var/lib/postgresql/data"},
				{Type: "bind", Source: extra, Target: "/extra"},
				{Type: "bind", Source: "/var/run/docker.sock", Target: "/var/run/docker.sock"},
				{Type: "volume", Source: "gone_volume", Target: "/gone"},
			},
			"s2": {{Type: "bind", Source: filepath.Join(root, "other", "v"), Target: "/data"}},
			"s3": {{Type: "bind", Source: filepath.Join(root, "agent", "v"), Target: "/data"}},
		},
		volumes: map[string]runtime.Volume{
			"demo_db_data": {Name: "demo_db_data", Driver: "local", Device: source, Bind: true},
		},
	}
	forcer := &repairForcer{}
	created, updated, err := repairLocalVolumeDirs(context.Background(), dc, forcer, root, zerolog.Nop())
	if err != nil {
		t.Fatalf("repair: %v", err)
	}
	if created != 2 || updated != 1 {
		t.Fatalf("created=%d updated=%d, want 2/1", created, updated)
	}
	for _, dir := range []string{source, extra} {
		if _, statErr := os.Stat(dir); statErr != nil {
			t.Fatalf("volume directory not created: %s (%v)", dir, statErr)
		}
	}
	if len(forcer.forced) != 1 || forcer.forced[0] != "s1" {
		t.Fatalf("forced=%v, want [s1]", forcer.forced)
	}
	for _, other := range []string{filepath.Join(root, "other", "v"), filepath.Join(root, "agent", "v")} {
		if _, statErr := os.Stat(other); statErr == nil {
			t.Fatalf("created a directory for a service not pinned to this node: %s", other)
		}
	}
}

func TestRepairLocalVolumeDirs_ExistingDirIsNoop(t *testing.T) {
	host, _ := os.Hostname()
	root := t.TempDir()
	source := filepath.Join(root, "demo", "v")
	if err := os.MkdirAll(source, 0o755); err != nil {
		t.Fatalf("seed: %v", err)
	}
	dc := repairDocker{
		services: []runtime.Service{{ID: "s1", Name: "demo_db", Node: host}},
		mounts:   map[string][]runtime.Mount{"s1": {{Type: "bind", Source: source, Target: "/data"}}},
	}
	forcer := &repairForcer{}
	created, updated, err := repairLocalVolumeDirs(context.Background(), dc, forcer, root, zerolog.Nop())
	if err != nil {
		t.Fatalf("repair: %v", err)
	}
	if created != 0 || updated != 0 || len(forcer.forced) != 0 {
		t.Fatalf("created=%d updated=%d forced=%v, want all zero", created, updated, forcer.forced)
	}
}

func TestRepairLocalVolumeDirs_WorkerIsSilent(t *testing.T) {
	dc := repairDocker{listErr: errors.New("docker service ls: This node is not a swarm manager. Worker nodes can't be used to view or modify cluster state")}
	created, updated, err := repairLocalVolumeDirs(context.Background(), dc, &repairForcer{}, "/var/stack/data", zerolog.Nop())
	if err != nil {
		t.Fatalf("a worker must not report an error: %v", err)
	}
	if created != 0 || updated != 0 {
		t.Fatalf("created=%d updated=%d, want 0/0", created, updated)
	}
}

func TestRepairLocalVolumeDirs_EmptyRootIsNoop(t *testing.T) {
	host, _ := os.Hostname()
	source := filepath.Join(t.TempDir(), "demo", "v")
	dc := repairDocker{
		services: []runtime.Service{{ID: "s1", Name: "demo_db", Node: host}},
		mounts:   map[string][]runtime.Mount{"s1": {{Type: "bind", Source: source, Target: "/data"}}},
	}
	forcer := &repairForcer{}
	created, updated, err := repairLocalVolumeDirs(context.Background(), dc, forcer, "", zerolog.Nop())
	if err != nil {
		t.Fatalf("repair: %v", err)
	}
	if created != 0 || updated != 0 {
		t.Fatalf("created=%d updated=%d, want 0/0", created, updated)
	}
	if _, statErr := os.Stat(source); statErr == nil {
		t.Fatal("created a directory with no volume root configured")
	}
}

func TestUnderVolumeRoot(t *testing.T) {
	cases := []struct {
		root, path string
		want       bool
	}{
		{"/var/stack/data", "/var/stack/data", true},
		{"/var/stack/data", "/var/stack/data/app/v", true},
		{"/var/stack/data", "/var/stack/datax/v", false},
		{"/var/stack/data", "/etc/passwd", false},
		{"/var/stack/data", "/var/stack", false},
		{"", "/var/stack/data/app/v", false},
	}
	for _, c := range cases {
		if got := underVolumeRoot(c.root, c.path); got != c.want {
			t.Errorf("underVolumeRoot(%q, %q) = %v, want %v", c.root, c.path, got, c.want)
		}
	}
}

func TestLocalVolumeRoot(t *testing.T) {
	ctx := context.Background()
	if got := localVolumeRoot(ctx, nil); got != manifest.DefaultVolumeRoot {
		t.Fatalf("nil store = %q, want %q", got, manifest.DefaultVolumeRoot)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = st.Close() }()
	if got := localVolumeRoot(ctx, st); got != manifest.DefaultVolumeRoot {
		t.Fatalf("unset setting = %q, want the default", got)
	}
	if err := st.SetSetting(ctx, cluster.SettingVolumeRoot(), ""); err != nil {
		t.Fatalf("set empty: %v", err)
	}
	if got := localVolumeRoot(ctx, st); got != manifest.DefaultVolumeRoot {
		t.Fatalf("empty setting = %q, want the default (an empty row counts as unset)", got)
	}
	if err := st.SetSetting(ctx, cluster.SettingVolumeRoot(), "/srv/stack/data"); err != nil {
		t.Fatalf("set value: %v", err)
	}
	if got := localVolumeRoot(ctx, st); got != "/srv/stack/data" {
		t.Fatalf("configured setting = %q, want /srv/stack/data", got)
	}
}
