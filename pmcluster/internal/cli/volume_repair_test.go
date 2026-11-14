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
	tasks    map[string][]runtime.ServiceTask
	listErr  error
}

func (r repairDocker) ServiceTasks(_ context.Context, id string) ([]runtime.ServiceTask, error) {
	return r.tasks[id], nil
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
	created, updated, _, err := repairLocalVolumeDirs(context.Background(), dc, forcer, root, nil, zerolog.Nop())
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
	created, updated, _, err := repairLocalVolumeDirs(context.Background(), dc, forcer, root, nil, zerolog.Nop())
	if err != nil {
		t.Fatalf("repair: %v", err)
	}
	if created != 0 || updated != 0 || len(forcer.forced) != 0 {
		t.Fatalf("created=%d updated=%d forced=%v, want all zero", created, updated, forcer.forced)
	}
}

func TestRepairLocalVolumeDirs_WorkerIsSilent(t *testing.T) {
	dc := repairDocker{listErr: errors.New("docker service ls: This node is not a swarm manager. Worker nodes can't be used to view or modify cluster state")}
	created, updated, _, err := repairLocalVolumeDirs(context.Background(), dc, &repairForcer{}, "/var/stack/data", nil, zerolog.Nop())
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
	created, updated, _, err := repairLocalVolumeDirs(context.Background(), dc, forcer, "", nil, zerolog.Nop())
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

// fakeUserResolver answers image users from a map and counts how often it was
// asked, so the cache can be tested without a daemon.
type fakeUserResolver struct {
	users map[string][2]int
	calls int
}

func (f *fakeUserResolver) ImageUserIDs(ctx context.Context, ref string) (int, int, bool, error) {
	f.calls++
	if u, ok := f.users[ref]; ok {
		return u[0], u[1], true, nil
	}
	return 0, 0, false, nil
}

func TestRepairStateCachesImageOwner(t *testing.T) {
	res := &fakeUserResolver{users: map[string][2]int{"img:1": {10001, 10001}}}
	st := newRepairState()
	for i := 0; i < 5; i++ {
		uid, gid, ok := st.owner(context.Background(), res, "img:1")
		if !ok || uid != 10001 || gid != 10001 {
			t.Fatalf("owner = %d:%d ok=%v", uid, gid, ok)
		}
	}
	if res.calls != 1 {
		t.Errorf("resolved %d times, want 1 — a 30s loop must not create a container per pass", res.calls)
	}
	// Unresolvable images are cached too (as not-ok), or every pass would retry.
	st.owner(context.Background(), res, "img:missing")
	st.owner(context.Background(), res, "img:missing")
	if res.calls != 2 {
		t.Errorf("calls = %d, want 2 (one per image, cached)", res.calls)
	}
}

func TestEnforceOwnershipLeavesRootImagesAlone(t *testing.T) {
	dir := t.TempDir()
	st := newRepairState()
	// An empty map means "runs as root or unknown": nothing to repair.
	changed, err := enforceOwnership(context.Background(), &fakeUserResolver{}, st, "img:root", dir)
	if err != nil || changed != 0 {
		t.Fatalf("changed=%d err=%v, want 0/nil", changed, err)
	}
}

// TestEnforceOwnershipTakesOverForeignDirectory is the 4-m case: the tree is
// owned by someone else (root, after an adoption copy) and the image runs as a
// different user. Needs root to hand the directory to a foreign uid.
func TestEnforceOwnershipTakesOverForeignDirectory(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root to create a directory owned by another uid")
	}
	dir := t.TempDir()
	sub := filepath.Join(dir, "nested")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "app.db"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	const foreign = 10001
	for _, p := range []string{dir, sub, filepath.Join(sub, "app.db")} {
		if err := os.Chown(p, foreign, foreign); err != nil {
			t.Fatal(err)
		}
	}
	st := newRepairState()
	res := &fakeUserResolver{users: map[string][2]int{"img:app": {int(os.Getuid()), int(os.Getgid())}}}
	changed, err := enforceOwnership(context.Background(), res, st, "img:app", dir)
	if err != nil {
		t.Fatalf("enforce: %v", err)
	}
	if changed < 3 {
		t.Errorf("changed %d entries, want the directory, the nested dir and the file", changed)
	}
	for _, p := range []string{dir, sub, filepath.Join(sub, "app.db")} {
		uid, _, oerr := pathOwner(p)
		if oerr != nil || uid != os.Getuid() {
			t.Errorf("%s still owned by %d (err=%v)", p, uid, oerr)
		}
	}
}

// TestRepairLocalVolumeDirs_RefusesToRecreateVanishedData is the guard against
// the silent-empty-start: a node that has run the service before has lost the
// data, and recreating the directory would boot the app on an empty database
// with nothing in the logs to say so.
func TestRepairLocalVolumeDirs_RefusesToRecreateVanishedData(t *testing.T) {
	host, _ := os.Hostname()
	root := t.TempDir()
	missing := filepath.Join(root, "shop", "db_data")
	dc := repairDocker{
		services: []runtime.Service{{ID: "s1", Name: "shop_db", Node: host}},
		mounts:   map[string][]runtime.Mount{"s1": {{Type: "bind", Source: missing, Target: "/data"}}},
		tasks:    map[string][]runtime.ServiceTask{"s1": {{TaskID: "t1", State: "shutdown"}}},
	}
	forcer := &repairForcer{}
	created, updated, _, err := repairLocalVolumeDirs(context.Background(), dc, forcer, root, nil, zerolog.Nop())
	if err != nil {
		t.Fatalf("repair: %v", err)
	}
	if created != 0 || updated != 0 {
		t.Fatalf("created=%d updated=%d, want 0/0 — the task must fail rather than the app boot empty", created, updated)
	}
	if _, statErr := os.Stat(missing); statErr == nil {
		t.Fatal("the directory was recreated")
	}
}

// A first-ever deployment has no task history: the volume repair is what makes
// its directory exist, so the guard must not block it.
func TestRepairLocalVolumeDirs_CreatesForANewService(t *testing.T) {
	host, _ := os.Hostname()
	root := t.TempDir()
	fresh := filepath.Join(root, "newapp", "data")
	dc := repairDocker{
		services: []runtime.Service{{ID: "s9", Name: "newapp_web", Node: host}},
		mounts:   map[string][]runtime.Mount{"s9": {{Type: "bind", Source: fresh, Target: "/data"}}},
		tasks:    map[string][]runtime.ServiceTask{"s9": {{TaskID: "t9", State: "rejected", Error: "no such file or directory"}}},
	}
	created, _, _, err := repairLocalVolumeDirs(context.Background(), dc, &repairForcer{}, root, nil, zerolog.Nop())
	if err != nil {
		t.Fatalf("repair: %v", err)
	}
	if created != 1 {
		t.Fatalf("created=%d, want 1", created)
	}
	if _, statErr := os.Stat(fresh); statErr != nil {
		t.Fatal("directory not created for a service that has never run")
	}
}
