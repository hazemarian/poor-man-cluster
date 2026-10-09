package manifest

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runtime"
	"github.com/hazemarian/poor-man-cluster/pmcluster/pkg/dsl"
)

// TestTranslate_VersionedSecretName verifies the BUG-007 fix: a rotated
// secret (swarm_rev > 1) renders as an external secret with a `name:`
// override pointing at the versioned swarm secret <name>_v<rev>, while the
// logical compose key and the container mount path stay unchanged.
func TestTranslate_VersionedSecretName(t *testing.T) {
	app := baseApp()
	app.Secrets = []string{"db_pass"}
	app.Services["api"].Secrets = []string{"db_pass"}
	ir, err := BuildIR(context.Background(), app, nil)
	if err != nil {
		t.Fatalf("BuildIR: %v", err)
	}

	// rev 1 → plain name (no override).
	out, err := (&ComposeWriter{}).Write(context.Background(), ir)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if strings.Contains(string(out), "name: db_pass_v2") {
		t.Errorf("rev-1 render unexpectedly versioned:\n%s", out)
	}

	// rev 3 → name: db_pass_v3 override, logical key untouched.
	w := &ComposeWriter{
		SecretNames: func(_ context.Context, name string) string {
			if name == "db_pass" {
				return "db_pass_v3"
			}
			return name
		},
	}
	out, err = w.Write(context.Background(), ir)
	if err != nil {
		t.Fatalf("Write versioned: %v", err)
	}
	if !strings.Contains(string(out), "name: db_pass_v3") {
		t.Errorf("versioned render missing name: override:\n%s", out)
	}
	// The logical key + service reference must survive for the mount path.
	if !strings.Contains(string(out), "external: true") {
		t.Errorf("versioned render lost external flag:\n%s", out)
	}
	if !strings.Contains(string(out), "- db_pass") {
		t.Errorf("versioned render lost service secret reference:\n%s", out)
	}
}

// TestTranslate_DependsOnEmitsComposeParity builds a migration service that
// depends on the db service and asserts the rendered compose carries the
// depends_on block (plain list form) for compose parity. Startup ordering is
// NOT rendered into the artifact — the deploy pipeline orders services into
// depends_on levels and deploys level by level (see ServiceLevels and the
// stacks deploy path). The translation stays clean and image-agnostic: no
// shell wrapper, no getent/nc probes, no image requirements.
func TestTranslate_DependsOnEmitsComposeParity(t *testing.T) {
	app := baseApp()
	app.Services = map[string]*dsl.Service{
		"db": {
			Image:       "postgres:14-alpine",
			Volumes:     []string{"db_data:/var/lib/postgresql/data"},
			Healthcheck: &dsl.Healthcheck{Type: "pg_isready"},
		},
		"migration": {
			Image:     "my-app:latest",
			Command:   []string{"./migrate"},
			RunOnce:   true,
			DependsOn: []string{"db"},
		},
	}

	out, err := TranslateIR(context.Background(), app, nil, nil)
	if err != nil {
		t.Fatalf("TranslateIR: %v", err)
	}
	s := string(out)

	// Compose-parity depends_on block present for the migration service
	// (list form — compose v3.9 / docker stack deploy rejects the map form).
	if !strings.Contains(s, "depends_on:") {
		t.Errorf("rendered compose should contain a depends_on block:\n%s", s)
	}
	if !strings.Contains(s, "depends_on:\n    - db") {
		t.Errorf("depends_on should reference db as a list item:\n%s", s)
	}

	// NO wait wrapper may be rendered: the artifact must stay clean (the
	// control plane orders the deploy). The old POSIX-sh getent/nc wrapper is
	// gone entirely.
	for _, leak := range []string{"sh -c", "getent", "nc -z", "exec \"$@\"", "sleep 2"} {
		if strings.Contains(s, leak) {
			t.Errorf("rendered compose must not contain a wait wrapper (%q):\n%s", leak, s)
		}
	}

	// A run-once job that waits on dependencies gets a bounded on-failure
	// restart policy (so a transient failure retries, then stops).
	if !strings.Contains(s, "condition: on-failure") {
		t.Errorf("run-once + depends_on should use on-failure restart:\n%s", s)
	}
	if !strings.Contains(s, "max_attempts: 3") {
		t.Errorf("run-once + depends_on should bound retries to 3:\n%s", s)
	}
}

// TestTranslate_DependsOnNoWrapperForBakedEntrypoint asserts a service with
// depends_on but NO explicit command/entrypoint keeps its image untouched:
// the rendered compose carries only the depends_on list, and no entrypoint
// wrapper is emitted (the old sh-loop wrapper is gone). The image's baked
// entrypoint/CMD run as shipped.
func TestTranslate_DependsOnNoWrapperForBakedEntrypoint(t *testing.T) {
	app := baseApp()
	app.Services = map[string]*dsl.Service{
		"db": {
			Image:   "postgres:14-alpine",
			Volumes: []string{"db_data:/var/lib/postgresql/data"},
		},
		"migration": {
			Image:     "my-app:latest", // image ships the migration in its baked CMD
			RunOnce:   true,
			DependsOn: []string{"db"},
		},
	}

	out, err := TranslateIR(context.Background(), app, nil, nil)
	if err != nil {
		t.Fatalf("TranslateIR: %v", err)
	}
	s := string(out)

	if !strings.Contains(s, "depends_on:\n    - db") {
		t.Errorf("depends_on list form should still be emitted:\n%s", s)
	}
	for _, leak := range []string{"sh -c", "getent", "nc -z", "exec \"$@\"", "entrypoint:", "sleep 2"} {
		if strings.Contains(s, leak) {
			t.Errorf("no-command service must not get a wrapper (%q):\n%s", leak, s)
		}
	}
}

// TestComposeWriter_EscapesDollarInUserValues covers BUG-001 / H8: any literal
// `$` in a user/DSL-derived value must render as `$$` in the compose so docker
// stack deploy's interpolation cannot silently corrupt it. Covers env values,
// command/entrypoint elements, raw label values, and healthcheck test elements.
func TestComposeWriter_EscapesDollarInUserValues(t *testing.T) {
	ir := &IR{
		Name:    "demo",
		Env:     "prod",
		Version: "v1",
		Services: []IRService{
			{
				Name:       "api",
				Image:      "nginx:latest",
				Command:    []string{"./run", "--flag=$USER"},
				Entrypoint: []string{"/bin/sh", "-c", "echo $HOME"},
				Env: map[string]string{
					"PASSWORD": "p@ss$word",
					"PLAIN":    "no-dollar",
				},
				Labels: map[string]string{
					"traefik.http.routers.x.rule": "Host(`foo.$suffix`)",
				},
				Healthcheck: &IRHealthcheck{
					Test: []string{"CMD-SHELL", "check --token=$SECRET"},
				},
			},
		},
	}

	out, err := (&ComposeWriter{}).Write(context.Background(), ir)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	s := string(out)
	if !strings.Contains(s, "$$") {
		t.Fatalf("expected escaped `$$` in rendered compose:\n%s", s)
	}

	var cf composeFile
	if err := yaml.Unmarshal(out, &cf); err != nil {
		t.Fatalf("unmarshal rendered compose: %v\n%s", err, out)
	}
	svc := cf.Services["api"]
	if svc == nil {
		t.Fatalf("service api missing from render:\n%s", s)
	}

	if got := svc.Environment["PASSWORD"]; got != "p@ss$$word" {
		t.Errorf("env PASSWORD = %q, want escaped %q", got, "p@ss$$word")
	}
	if got := svc.Environment["PLAIN"]; got != "no-dollar" {
		t.Errorf("env PLAIN = %q, want unchanged %q", got, "no-dollar")
	}
	if got := svc.Command; len(got) != 2 || got[1] != "--flag=$$USER" {
		t.Errorf("command = %v, want [./run --flag=$$USER]", got)
	}
	if got := svc.Entrypoint; len(got) != 3 || got[2] != "echo $$HOME" {
		t.Errorf("entrypoint = %v, want [.. .. \"echo $$HOME\"]", got)
	}
	if got := svc.Deploy.Labels["traefik.http.routers.x.rule"]; got != "Host(`foo.$$suffix`)" {
		t.Errorf("raw label value = %q, want escaped Host(`foo.$$suffix`)", got)
	}
	if svc.Healthcheck == nil || len(svc.Healthcheck.Test) != 2 || svc.Healthcheck.Test[1] != "check --token=$$SECRET" {
		t.Errorf("healthcheck test = %v, want [CMD-SHELL \"check --token=$$SECRET\"]", svc.Healthcheck)
	}
}

// TestComposeWriter_PgIsreadyShorthandKeepsIntentionalDoubleDollar guards the
// hardcoded pg_isready healthcheck against over-escaping: its `$$POSTGRES_USER`
// is deliberate (compose collapses it to `$` so the container shell expands
// it), so it must render unchanged, not `$$$$`.
func TestComposeWriter_PgIsreadyShorthandKeepsIntentionalDoubleDollar(t *testing.T) {
	ir := &IR{
		Name:    "demo",
		Env:     "prod",
		Version: "v1",
		Services: []IRService{{
			Name:        "db",
			Image:       "postgres:14-alpine",
			Healthcheck: &IRHealthcheck{Type: "pg_isready"},
		}},
	}
	out, err := (&ComposeWriter{}).Write(context.Background(), ir)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	s := string(out)
	if !strings.Contains(s, "pg_isready -U $$POSTGRES_USER -d $$POSTGRES_DB") {
		t.Errorf("pg_isready shorthand must keep its intentional $$:\n%s", s)
	}
	if strings.Contains(s, "$$$$POSTGRES_USER") {
		t.Errorf("pg_isready shorthand was over-escaped ($$$$):\n%s", s)
	}
}

// TestEnsureVolumeDirs_CreatesBindTargets renders a compose with a named
// volume and asserts EnsureVolumeDirs mkdirs the driver_opts device paths.
func TestEnsureVolumeDirs_CreatesBindTargets(t *testing.T) {
	app := baseApp()
	app.Name = "vault-app"
	app.Services = map[string]*dsl.Service{
		"api": {
			Image:   "nginx:latest",
			Volumes: []string{"data_vol:/data"},
		},
	}

	out, err := TranslateIR(context.Background(), app, nil, &ComposeWriter{})
	if err != nil {
		t.Fatalf("TranslateIR: %v", err)
	}

	root := DefaultVolumeRoot
	var made []string
	mkdirAll := func(dir string, mode os.FileMode) error {
		made = append(made, dir)
		return nil
	}

	if err := EnsureVolumeDirs(out, mkdirAll); err != nil {
		t.Fatalf("EnsureVolumeDirs: %v", err)
	}

	want := filepath.Join(root, "vault-app", "data_vol")
	found := false
	for _, d := range made {
		if d == want {
			found = true
		}
	}
	if !found {
		t.Errorf("EnsureVolumeDirs should have created %q, got %v", want, made)
	}
}

// TestEnsureVolumeDirs_SkipsNonBindVolumes asserts volumes without a
// driver_opts device (e.g. external or plain named volumes) are skipped.
func TestEnsureVolumeDirs_SkipsNonBindVolumes(t *testing.T) {
	// No driver_opts device — must not be created, must not error.
	err := EnsureVolumeDirs([]byte("volumes:\n  plain:\n    driver: local\n"), func(string, os.FileMode) error {
		t.Fatal("mkdirAll should not be called for a volume without device")
		return nil
	})
	if err != nil {
		t.Fatalf("EnsureVolumeDirs: %v", err)
	}
}

// TestTranslate_CertResolverConditional asserts the tls.certresolver label is
// emitted on exposed routers ONLY when the writer carries a resolver name
// (ACME clusters). BYO-cert clusters leave it empty and must get NO label —
// referencing a nonexistent letsencrypt resolver would break routing.
func TestTranslate_CertResolverConditional(t *testing.T) {
	app := baseApp()
	app.Services = map[string]*dsl.Service{
		"api": {
			Image: "nginx:latest",
			Expose: &dsl.Expose{
				Port: 8080,
				Host: "api.example.com",
			},
		},
	}

	// ACME cluster: resolver present.
	out, err := TranslateIR(context.Background(), app, nil, &ComposeWriter{CertResolver: "letsencrypt"})
	if err != nil {
		t.Fatalf("TranslateIR (acme): %v", err)
	}
	s := string(out)
	if !strings.Contains(s, "tls.certresolver: letsencrypt") {
		t.Errorf("ACME render should carry tls.certresolver: letsencrypt:\n%s", s)
	}

	// BYO-cert cluster: no resolver label at all.
	out, err = TranslateIR(context.Background(), app, nil, &ComposeWriter{})
	if err != nil {
		t.Fatalf("TranslateIR (byo): %v", err)
	}
	s = string(out)
	if strings.Contains(s, "tls.certresolver") {
		t.Errorf("BYO-cert render must NOT reference a letsencrypt resolver:\n%s", s)
	}
}

// TestTranslate_StatefulDefaults asserts that a service holding a volume gets
// stateful-aware defaults with NO manifest change: automatic stop-first update
// ordering (kills the postgres postmaster.pid shutdown race on redeploys) and
// auto-pinning to the platform node when no explicit placement is set.
func TestTranslate_StatefulDefaults(t *testing.T) {
	app := baseApp()
	app.Services = map[string]*dsl.Service{
		"db": {
			Image:   "postgres:14-alpine",
			Volumes: []string{"db_data:/var/lib/postgresql/data"},
		},
	}

	out, err := TranslateIR(context.Background(), app, nil, &ComposeWriter{PinNode: "node-01"})
	if err != nil {
		t.Fatalf("TranslateIR: %v", err)
	}
	s := string(out)

	// Stop-first: the old task shuts down fully before the replacement starts.
	if !strings.Contains(s, "order: stop-first") {
		t.Errorf("stateful service should default to stop-first update order:\n%s", s)
	}

	// Auto-pin: no explicit placement → pinned to the platform node.
	if !strings.Contains(s, "node.hostname == node-01") {
		t.Errorf("stateful service without placement should auto-pin to the platform node:\n%s", s)
	}

	// The resolved pin is stamped as a label so the UI can show the node.
	if !strings.Contains(s, runtime.NodeLabel+": node-01") {
		t.Errorf("stateful service should carry the %s node label:\n%s", runtime.NodeLabel, s)
	}
}

// TestTranslate_StatefulDefaultsDisabled asserts that without a PinNode the
// auto-pin is off (stateful services schedule anywhere) and stateless services
// keep start-first ordering even when a PinNode is configured.
func TestTranslate_StatefulDefaultsDisabled(t *testing.T) {
	app := baseApp()
	app.Services = map[string]*dsl.Service{
		"db": {
			Image:   "postgres:14-alpine",
			Volumes: []string{"db_data:/var/lib/postgresql/data"},
		},
		"web": {
			Image: "nginx:latest",
		},
	}

	out, err := TranslateIR(context.Background(), app, nil, nil)
	if err != nil {
		t.Fatalf("TranslateIR: %v", err)
	}
	s := string(out)

	if strings.Contains(s, "node.hostname") {
		t.Errorf("no PinNode configured — stateful services must not be pinned:\n%s", s)
	}
	if strings.Contains(s, runtime.NodeLabel) {
		t.Errorf("no PinNode configured — services must not carry the %s node label:\n%s", runtime.NodeLabel, s)
	}
	// Stateless web service keeps the start-first default.
	if !strings.Contains(s, "order: start-first") {
		t.Errorf("stateless service should keep start-first update order:\n%s", s)
	}
}

// ---------------------------------------------------------------------------
// platform-via-DSL surface: mode/restart, raw constraints, binds, ports,
// config mounts, extra_hosts, user, resources, raw labels, the platform
// label and app-level networks.
// ---------------------------------------------------------------------------

// renderCompose translates app through w (nil = default compose writer) and
// unmarshals the rendered YAML into a composeFile, returning BOTH the raw
// text (for substring assertions) and the parsed structure (for precise
// per-field assertions).
func renderCompose(t *testing.T, app *dsl.App, w Writer) (string, composeFile) {
	t.Helper()
	out, err := TranslateIR(context.Background(), app, nil, w)
	if err != nil {
		t.Fatalf("TranslateIR: %v", err)
	}
	var cf composeFile
	if err := yaml.Unmarshal(out, &cf); err != nil {
		t.Fatalf("unmarshal rendered compose: %v\n%s", err, out)
	}
	return string(out), cf
}

// deployOf returns the deploy block of a rendered service or fails the test.
func deployOf(t *testing.T, cf composeFile, svc string) *composeDeploy {
	t.Helper()
	cs, ok := cf.Services[svc]
	if !ok || cs.Deploy == nil {
		t.Fatalf("service %q missing from rendered compose (or has no deploy block)", svc)
	}
	return cs.Deploy
}

// TestTranslate_GlobalMode verifies mode: global renders a Swarm global
// service — deploy mode global, restart_policy condition defaulting to
// "any", NO replicas key, NO update_config block — plus the validation rules
// (mode: global is mutually exclusive with replicas and run_once) and the
// stateful auto-pin exemption (global services run on every node, never pin).
func TestTranslate_GlobalMode(t *testing.T) {
	app := baseApp()
	app.Services["api"].Mode = "global"

	s, cf := renderCompose(t, app, nil)
	d := deployOf(t, cf, "api")

	if !strings.Contains(s, "mode: global") {
		t.Errorf("global service should render `mode: global`:\n%s", s)
	}
	if !strings.Contains(s, "condition: any") {
		t.Errorf("global service should default restart_policy condition to any:\n%s", s)
	}
	if strings.Contains(s, "replicas:") {
		t.Errorf("global service must NOT render a replicas key:\n%s", s)
	}
	if strings.Contains(s, "update_config") {
		t.Errorf("global service must NOT render an update_config block:\n%s", s)
	}
	if d.Mode != "global" {
		t.Errorf("deploy.mode = %q, want global", d.Mode)
	}
	if d.Replicas != nil {
		t.Errorf("deploy.replicas = %d, want nil for a global service", *d.Replicas)
	}
	if d.UpdateConfig != nil {
		t.Errorf("deploy.update_config = %+v, want nil for a global service", d.UpdateConfig)
	}
	if d.RestartPolicy == nil || d.RestartPolicy.Condition != "any" {
		t.Errorf("restart_policy = %+v, want condition any", d.RestartPolicy)
	}

	// An explicit restart override is honored on the global branch.
	app = baseApp()
	app.Services["api"].Mode = "global"
	app.Services["api"].Restart = "on-failure"
	s, _ = renderCompose(t, app, nil)
	if !strings.Contains(s, "condition: on-failure") {
		t.Errorf("global + restart: on-failure should render that condition:\n%s", s)
	}

	// Stateful auto-pin exemption: a global service holding a volume is NOT
	// pinned to the platform node — it must run on every node.
	app = baseApp()
	app.Services["api"].Mode = "global"
	app.Services["api"].Volumes = []string{"data:/data"}
	s, _ = renderCompose(t, app, &ComposeWriter{PinNode: "node-01"})
	if strings.Contains(s, "node.hostname == node-01") {
		t.Errorf("global service must be exempt from the stateful auto-pin:\n%s", s)
	}
	if strings.Contains(s, runtime.NodeLabel) {
		t.Errorf("global service must not carry the %s pin label:\n%s", runtime.NodeLabel, s)
	}

	// Validation: mode: global is meaningless with replicas / run_once.
	bad := baseApp()
	bad.Services["api"].Mode = "global"
	bad.Services["api"].Replicas = ptr(2)
	mustFail(t, bad, "replicas and mode: global are mutually exclusive")

	bad = baseApp()
	bad.Services["api"].Mode = "global"
	bad.Services["api"].RunOnce = true
	mustFail(t, bad, "run_once and mode: global are mutually exclusive")

	// Anything other than "global" (or empty) is rejected.
	bad = baseApp()
	bad.Services["api"].Mode = "daemon"
	mustFail(t, bad, "services.api.mode:")

	// Empty mode keeps the replicated default (replicas rendered).
	app = baseApp()
	s, cf = renderCompose(t, app, nil)
	if d := deployOf(t, cf, "api"); d.Mode != "" || d.Replicas == nil || *d.Replicas != 1 {
		t.Errorf("default mode should stay replicated with replicas=1, got mode=%q replicas=%v", d.Mode, d.Replicas)
	}
	if strings.Contains(s, "mode: global") {
		t.Errorf("a service without mode must not render mode: global:\n%s", s)
	}
}

// TestTranslate_RawConstraints verifies raw placement constraints render
// verbatim alongside (AFTER) the auto-derived role/hostname constraint.
func TestTranslate_RawConstraints(t *testing.T) {
	const raw = "node.labels.pmcluster.storage == true"

	// No Placement → the raw constraint is the only entry.
	app := baseApp()
	app.Services["api"].Constraints = []string{raw}
	s, cf := renderCompose(t, app, nil)
	if !strings.Contains(s, raw) {
		t.Errorf("raw constraint should render verbatim:\n%s", s)
	}
	if p := deployOf(t, cf, "api").Placement; p == nil || len(p.Constraints) != 1 || p.Constraints[0] != raw {
		t.Errorf("placement constraints = %+v, want exactly [%q]", p, raw)
	}

	// Placement "worker" + raw constraint → BOTH, role constraint first.
	app = baseApp()
	app.Services["api"].Placement = "worker"
	app.Services["api"].Constraints = []string{raw}
	s, cf = renderCompose(t, app, nil)
	iRole := strings.Index(s, "node.role == worker")
	iRaw := strings.Index(s, raw)
	if iRole < 0 {
		t.Errorf("auto role constraint missing:\n%s", s)
	}
	if iRaw < 0 {
		t.Errorf("raw constraint missing:\n%s", s)
	}
	if iRole >= 0 && iRaw >= 0 && iRole > iRaw {
		t.Errorf("role constraint must render BEFORE the raw constraint:\n%s", s)
	}
	if p := deployOf(t, cf, "api").Placement; p == nil ||
		len(p.Constraints) != 2 ||
		p.Constraints[0] != "node.role == worker" || p.Constraints[1] != raw {
		t.Errorf("placement constraints = %+v, want [role worker, raw]", p)
	}

	// Hostname placement + raw constraint → BOTH, hostname pin first.
	app = baseApp()
	app.Services["api"].Placement = "node-01"
	app.Services["api"].Constraints = []string{raw}
	_, cf = renderCompose(t, app, nil)
	if p := deployOf(t, cf, "api").Placement; p == nil ||
		len(p.Constraints) != 2 ||
		p.Constraints[0] != "node.hostname == node-01" || p.Constraints[1] != raw {
		t.Errorf("placement constraints = %+v, want [hostname pin, raw]", p)
	}

	// Validation: empty / multi-line constraints are rejected.
	bad := baseApp()
	bad.Services["api"].Constraints = []string{"   "}
	mustFail(t, bad, "constraints[0]: empty constraint")

	bad = baseApp()
	bad.Services["api"].Constraints = []string{"node.role == manager\nnode.role == worker"}
	mustFail(t, bad, "constraints[0]: constraint must be single-line")
}

// TestTranslate_BindsVerbatim asserts raw binds land in the service volumes
// list byte-for-byte — they are host mounts (docker.sock, the volume root,
// …) and must NOT be relocated under <volume_root>/<app>/ like ordinary
// host-path volumes.
func TestTranslate_BindsVerbatim(t *testing.T) {
	binds := []string{
		"/var/run/docker.sock:/var/run/docker.sock:ro",
		"/srv/stack/data:/backup/data:ro",
	}
	app := baseApp()
	app.Services["api"].Binds = binds
	app.Services["api"].Volumes = []string{"data_vol:/data"}

	s, cf := renderCompose(t, app, &ComposeWriter{})
	got := cf.Services["api"].Volumes

	contains := func(hay []string, needle string) bool {
		for _, h := range hay {
			if h == needle {
				return true
			}
		}
		return false
	}
	for _, b := range binds {
		if !contains(got, b) {
			t.Errorf("bind %q must appear VERBATIM in volumes %v:\n%s", b, got, s)
		}
	}

	// NOT relocated: no <root>/<app>/<basename> rewriting of the raw binds.
	for _, leak := range []string{"my-app/docker.sock", "my-app/data:/backup"} {
		if strings.Contains(s, leak) {
			t.Errorf("raw bind must not be relocated (found %q):\n%s", leak, s)
		}
	}
	// A regular named volume still renders untouched next to the binds.
	if !contains(got, "data_vol:/data") {
		t.Errorf("named volume should still render as data_vol:/data, got %v:\n%s", got, s)
	}

	// Validation: a bind without a colon is rejected.
	bad := baseApp()
	bad.Services["api"].Binds = []string{"/var/run/docker.sock"}
	mustFail(t, bad, "binds[0]: must be '/host:/container[:ro]'")
}

// TestTranslate_Ports verifies published ports render with Published
// defaulting to Target (all TCP) and that an explicit host-mode port survives
// translation. (The Protocol field was dropped from the DSL — all swarm port
// publishing is TCP.)
func TestTranslate_Ports(t *testing.T) {
	app := baseApp()
	app.Services["api"].Ports = []dsl.PortSpec{
		{Target: 80, Published: 80},
		{Target: 4318, Published: 4318, Mode: "host"},
		{Target: 9000}, // published defaults to target
	}
	if err := Validate(app); err != nil {
		t.Fatalf("Validate: %v", err)
	}

	s, cf := renderCompose(t, app, nil)
	ports := cf.Services["api"].Ports
	if len(ports) != 3 {
		t.Fatalf("rendered ports = %+v, want 3 entries\n%s", ports, s)
	}

	want := []composePort{
		{Target: 80, Published: 80, Protocol: "tcp"},
		{Target: 4318, Published: 4318, Protocol: "tcp", Mode: "host"},
		{Target: 9000, Published: 9000, Protocol: "tcp"}, // published defaulted
	}
	for i, w := range want {
		if ports[i] != w {
			t.Errorf("ports[%d] = %+v, want %+v", i, ports[i], w)
		}
	}

	// String-level checks on the rendered YAML (published defaulting + host
	// mode; protocol always tcp).
	for _, substr := range []string{
		"target: 80", "published: 80",
		"protocol: tcp",
		"target: 4318", "published: 4318",
		"mode: host",
		"target: 9000", "published: 9000",
	} {
		if !strings.Contains(s, substr) {
			t.Errorf("rendered ports missing %q:\n%s", substr, s)
		}
	}

	// Validation: range / mode checks.
	bad := baseApp()
	bad.Services["api"].Ports = []dsl.PortSpec{{Target: 0}}
	mustFail(t, bad, "ports[0].target:")

	bad = baseApp()
	bad.Services["api"].Ports = []dsl.PortSpec{{Target: 80, Published: 70000}}
	mustFail(t, bad, "ports[0].published:")

	bad = baseApp()
	bad.Services["api"].Ports = []dsl.PortSpec{{Target: 80, Mode: "bridge"}}
	mustFail(t, bad, "ports[0].mode:")
}

// TestTranslate_ConfigMounts verifies a config_path(<name>) service config
// mount declares the logical name as an EXTERNAL top-level config (with an
// optional versioned name: override from ComposeWriter.ConfigNames) while the
// service keeps mounting the LOGICAL source so the container path stays
// stable. Without a ConfigPathResolver the file mounts at the default
// /etc/<name>; a resolver overrides the target.
func TestTranslate_ConfigMounts(t *testing.T) {
	app := baseApp()
	app.Services["api"].Configs = []string{"config_path(pmcluster_traefik_dynamic)"}
	if err := Validate(app); err != nil {
		t.Fatalf("Validate: %v", err)
	}

	// No ConfigNames resolver → plain external declaration.
	s, cf := renderCompose(t, app, &ComposeWriter{})
	decl, ok := cf.Configs["pmcluster_traefik_dynamic"]
	if !ok {
		t.Fatalf("top-level configs block missing pmcluster_traefik_dynamic:\n%s", s)
	}
	if !decl.External {
		t.Errorf("config must be declared external, got %+v", decl)
	}
	if decl.Name != "" {
		t.Errorf("without ConfigNames there must be no name override, got %q", decl.Name)
	}
	if !strings.Contains(s, "configs:") || !strings.Contains(s, "external: true") {
		t.Errorf("rendered compose should declare an external config:\n%s", s)
	}
	mounts := cf.Services["api"].Configs
	if len(mounts) != 1 || mounts[0].Source != "pmcluster_traefik_dynamic" || mounts[0].Target != "/etc/pmcluster_traefik_dynamic" {
		t.Errorf("service config mounts = %+v, want source + default target /etc/<name>", mounts)
	}

	// With ConfigNames: top-level gets the versioned name: override; the
	// logical compose key AND the service mount source stay unchanged.
	w := &ComposeWriter{
		ConfigNames: func(_ context.Context, name string) string {
			return name + "_v3"
		},
	}
	s, cf = renderCompose(t, app, w)
	decl, ok = cf.Configs["pmcluster_traefik_dynamic"]
	if !ok {
		t.Fatalf("top-level configs block missing logical key:\n%s", s)
	}
	if !decl.External {
		t.Errorf("versioned config must stay external, got %+v", decl)
	}
	if decl.Name != "pmcluster_traefik_dynamic_v3" {
		t.Errorf("config name override = %q, want pmcluster_traefik_dynamic_v3", decl.Name)
	}
	if !strings.Contains(s, "name: pmcluster_traefik_dynamic_v3") {
		t.Errorf("rendered configs block missing the versioned name override:\n%s", s)
	}
	mounts = cf.Services["api"].Configs
	if len(mounts) != 1 || mounts[0].Source != "pmcluster_traefik_dynamic" {
		t.Errorf("service mount source must stay the LOGICAL name, got %+v", mounts)
	}

	// With a ConfigPathResolver: the target honors the resolver path.
	app2 := baseApp()
	app2.Services["api"].Configs = []string{"config_path(pmcluster_traefik_dynamic)"}
	res := &configPathResolverStub{paths: map[string]string{
		"pmcluster_traefik_dynamic": "/etc/traefik/dynamic/conf.yml",
	}}
	out, err := TranslateIR(context.Background(), app2, res, &ComposeWriter{})
	if err != nil {
		t.Fatalf("TranslateIR (resolver): %v", err)
	}
	var cf2 composeFile
	if err := yaml.Unmarshal(out, &cf2); err != nil {
		t.Fatalf("unmarshal rendered compose: %v\n%s", err, out)
	}
	mounts = cf2.Services["api"].Configs
	if len(mounts) != 1 || mounts[0].Target != "/etc/traefik/dynamic/conf.yml" {
		t.Errorf("service mount target = %+v, want resolver path /etc/traefik/dynamic/conf.yml", mounts)
	}

	// Validation: every configs entry must be a valid config_path(<name>).
	bad := baseApp()
	bad.Services["api"].Configs = []string{"raw-name-without-helper"}
	mustFail(t, bad, "configs[0]: expected config_path(<name>)")

	bad = baseApp()
	bad.Services["api"].Configs = []string{"config_path( )"}
	mustFail(t, bad, "config_path(<name>) requires a non-empty name")
}

// TestTranslate_ExtraHostsAndUser verifies /etc/hosts entries and the
// container user render through to the service block.
func TestTranslate_ExtraHostsAndUser(t *testing.T) {
	app := baseApp()
	app.Services["api"].ExtraHosts = []string{"host.docker.internal:host-gateway"}
	app.Services["api"].User = "0:0"
	if err := Validate(app); err != nil {
		t.Fatalf("Validate: %v", err)
	}

	s, cf := renderCompose(t, app, nil)
	if !strings.Contains(s, "extra_hosts:") {
		t.Errorf("rendered compose missing extra_hosts:\n%s", s)
	}
	if !strings.Contains(s, "- host.docker.internal:host-gateway") {
		t.Errorf("rendered compose missing the extra host entry:\n%s", s)
	}
	if !strings.Contains(s, `user: "0:0"`) {
		t.Errorf(`rendered compose missing user: "0:0"`+"\n%s", s)
	}
	svc := cf.Services["api"]
	if len(svc.ExtraHosts) != 1 || svc.ExtraHosts[0] != "host.docker.internal:host-gateway" {
		t.Errorf("extra_hosts = %v", svc.ExtraHosts)
	}
	if svc.User != "0:0" {
		t.Errorf("user = %q, want 0:0", svc.User)
	}

	// No user → the key stays omitted (image default).
	app = baseApp()
	_, cf = renderCompose(t, app, nil)
	if u := cf.Services["api"].User; u != "" {
		t.Errorf("user should be empty when unset, got %q", u)
	}

	// Validation: an extra host without a colon is rejected.
	bad := baseApp()
	bad.Services["api"].ExtraHosts = []string{"host.docker.internal"}
	mustFail(t, bad, "extra_hosts[0]: must be 'host:ip'")
}

// TestTranslate_Resources verifies cpu/memory reservations + limits render
// into the deploy resources block, and that the quantity regexes gate
// Validate.
func TestTranslate_Resources(t *testing.T) {
	app := baseApp()
	app.Services["api"].Resources = &dsl.Resources{
		Reservations: &dsl.ResourceSpec{CPUs: "0.1", Memory: "128M"},
		Limits:       &dsl.ResourceSpec{CPUs: "0.25", Memory: "256M"},
	}
	if err := Validate(app); err != nil {
		t.Fatalf("Validate: %v", err)
	}

	s, cf := renderCompose(t, app, nil)
	r := deployOf(t, cf, "api").Resources
	if r == nil {
		t.Fatalf("deploy.resources missing:\n%s", s)
		return
	}
	if r.Reservations == nil || r.Reservations.CPUs != "0.1" || r.Reservations.Memory != "128M" {
		t.Errorf("reservations = %+v, want cpus 0.1 / memory 128M", r.Reservations)
	}
	if r.Limits == nil || r.Limits.CPUs != "0.25" || r.Limits.Memory != "256M" {
		t.Errorf("limits = %+v, want cpus 0.25 / memory 256M", r.Limits)
	}
	for _, substr := range []string{
		"reservations:", `cpus: "0.1"`, "memory: 128M",
		"limits:", `cpus: "0.25"`, "memory: 256M",
	} {
		if !strings.Contains(s, substr) {
			t.Errorf("rendered resources missing %q:\n%s", substr, s)
		}
	}

	// No resources → the block stays omitted.
	app = baseApp()
	_, cf = renderCompose(t, app, nil)
	if r := deployOf(t, cf, "api").Resources; r != nil {
		t.Errorf("deploy.resources should be nil when unset, got %+v", r)
	}

	// Validation: cpus is a decimal string, memory a byte quantity.
	bad := baseApp()
	bad.Services["api"].Resources = &dsl.Resources{Limits: &dsl.ResourceSpec{CPUs: "fast"}}
	mustFail(t, bad, "resources.limits.cpus:")

	bad = baseApp()
	bad.Services["api"].Resources = &dsl.Resources{Reservations: &dsl.ResourceSpec{Memory: "plenty"}}
	mustFail(t, bad, "resources.reservations.memory:")

	bad = baseApp()
	bad.Services["api"].Resources = &dsl.Resources{Limits: &dsl.ResourceSpec{Memory: "256"}}
	mustFail(t, bad, "resources.limits.memory:")

	ok := baseApp()
	ok.Services["api"].Resources = &dsl.Resources{
		Reservations: &dsl.ResourceSpec{CPUs: "2", Memory: "512m"},
		Limits:       &dsl.ResourceSpec{CPUs: "1.5", Memory: "1g"},
	}
	mustPass(t, ok)
}

// TestTranslate_RawLabels verifies raw deploy labels merge onto the service,
// that the standard auto-injected labels are still present, and — per the
// documented contract in pkg/dsl — that the STANDARD label wins on a key
// collision.
func TestTranslate_RawLabels(t *testing.T) {
	app := baseApp()
	app.Services["api"].Labels = map[string]string{
		"traefik.http.routers.x.rule": "Host(`foo.test`)",
	}
	s, cf := renderCompose(t, app, nil)
	labels := deployOf(t, cf, "api").Labels

	if labels["traefik.http.routers.x.rule"] != "Host(`foo.test`)" {
		t.Errorf("raw label missing from deploy labels: %v", labels)
	}
	if !strings.Contains(s, "traefik.http.routers.x.rule: Host(`foo.test`)") {
		t.Errorf("raw label should render verbatim:\n%s", s)
	}
	for k, v := range map[string]string{
		"service":     "api",
		"application": "my-app",
		"environment": "production",
	} {
		if labels[k] != v {
			t.Errorf("standard label %q = %q, want %q (raw labels must not remove it):\n%s", k, labels[k], v, s)
		}
	}

	// Collision: the standard label must WIN over a raw label with the same
	// key — pkg/dsl documents "Standard auto-injected labels
	// (service/application/environment/version, io.pmcluster.*) always win
	// on key collisions", and composeDeployFromIR's own comment says the
	// same.
	app = baseApp()
	app.Services["api"].Labels = map[string]string{
		"service":     "raw-hijack",
		"application": "raw-hijack",
	}
	s, cf = renderCompose(t, app, nil)
	labels = deployOf(t, cf, "api").Labels
	if labels["service"] != "api" {
		t.Errorf("standard label `service` must WIN on collision:\n  rendered: service: %q\n  expected: service: \"api\"\nrendered compose:\n%s", labels["service"], s)
	}
	if labels["application"] != "my-app" {
		t.Errorf("standard label `application` must WIN on collision:\n  rendered: application: %q\n  expected: application: \"my-app\"\nrendered compose:\n%s", labels["application"], s)
	}
}

// TestTranslate_PlatformLabel verifies app.platform: true stamps
// io.pmcluster.platform=true on EVERY service, and that the label is absent
// for ordinary app stacks.
func TestTranslate_PlatformLabel(t *testing.T) {
	app := baseApp()
	app.Platform = true
	app.Services["agent"] = &dsl.Service{Image: "otel/opentelemetry-collector:latest", Mode: "global"}

	s, cf := renderCompose(t, app, nil)
	if len(cf.Services) != 2 {
		t.Fatalf("expected 2 services, got %d:\n%s", len(cf.Services), s)
	}
	for name := range cf.Services {
		if got := deployOf(t, cf, name).Labels[labelPlatform]; got != "true" {
			t.Errorf("service %q label %s = %q, want \"true\"\n%s", name, labelPlatform, got, s)
		}
	}
	if !strings.Contains(s, `io.pmcluster.platform: "true"`) {
		t.Errorf("platform label should render as io.pmcluster.platform: \"true\":\n%s", s)
	}
	if got := strings.Count(s, labelPlatform+":"); got != 2 {
		t.Errorf("platform label stamped %d times, want once per service (2):\n%s", got, s)
	}

	// Ordinary app stack → no platform label at all.
	app = baseApp()
	app.Platform = false
	s, cf = renderCompose(t, app, nil)
	if strings.Contains(s, labelPlatform) {
		t.Errorf("non-platform app must not carry %s:\n%s", labelPlatform, s)
	}
	if got := deployOf(t, cf, "api").Labels[labelPlatform]; got != "" {
		t.Errorf("label %s = %q, want empty", labelPlatform, got)
	}
}

// TestTranslate_AppNetworks verifies app-level networks are declared
// top-level as external and joined by EVERY service.
func TestTranslate_AppNetworks(t *testing.T) {
	app := baseApp()
	app.Networks = []string{"traefik-net", "monitoring-net"}
	app.Services = map[string]*dsl.Service{
		"api":   {Image: "nginx:latest"},
		"agent": {Image: "otel/opentelemetry-collector:latest", Mode: "global"},
	}
	if err := Validate(app); err != nil {
		t.Fatalf("Validate: %v", err)
	}

	s, cf := renderCompose(t, app, nil)
	for _, n := range []string{"traefik-net", "monitoring-net"} {
		net, ok := cf.Networks[n]
		if !ok {
			t.Fatalf("top-level networks missing %q:\n%s", n, s)
		}
		if !net.External {
			t.Errorf("network %q must be external, got %+v", n, net)
		}
		if !strings.Contains(s, n+":\n    external: true") {
			t.Errorf("rendered networks block missing external %q:\n%s", n, s)
		}
	}
	// Explicit app networks REPLACE the private overlay (platform stacks join
	// shared external networks only — no throwaway per-stack overlay net).
	if net := cf.Networks["net"]; net != nil {
		t.Errorf("private overlay net must NOT exist when app networks are set: %+v", net)
	}
	for name, cs := range cf.Services {
		joined := map[string]bool{}
		for _, n := range cs.Networks {
			joined[n] = true
		}
		if joined["net"] {
			t.Errorf("service %q must NOT join the private net when app networks are set (networks=%v):\n%s", name, cs.Networks, s)
		}
		for _, n := range []string{"traefik-net", "monitoring-net"} {
			if !joined[n] {
				t.Errorf("service %q must join %q (networks=%v):\n%s", name, n, cs.Networks, s)
			}
		}
	}

	// No app networks → only the private overlay is declared/joined.
	app = baseApp()
	s, cf = renderCompose(t, app, nil)
	for _, n := range []string{"traefik-net", "monitoring-net"} {
		if _, ok := cf.Networks[n]; ok {
			t.Errorf("network %q must not be declared without app networks/expose:\n%s", n, s)
		}
	}
}

// TestTranslate_ExposeExternal verifies expose.external publishes a raw swarm
// port (TCP) without any Traefik router when Host is empty, honors the mode
// (host for the node-local OTel collector), and that a host-ful expose with
// external keeps BOTH the Traefik routing and the publish.
func TestTranslate_ExposeExternal(t *testing.T) {
	// Bare external publish (no host, host mode) — the otel-collector shape.
	app := baseApp()
	app.Services["api"].Expose = &dsl.Expose{Port: 4318, External: 4318, Mode: "host"}
	if err := Validate(app); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	s, cf := renderCompose(t, app, nil)
	ports := cf.Services["api"].Ports
	if len(ports) != 1 || ports[0].Target != 4318 || ports[0].Published != 4318 || ports[0].Mode != "host" {
		t.Fatalf("expose.external ports = %+v, want target/published 4318 host mode\n%s", ports, s)
	}
	for k := range cf.Services["api"].Deploy.Labels {
		if strings.HasPrefix(k, "traefik.") {
			t.Errorf("bare expose.external must not inject Traefik labels, got %q", k)
		}
	}
	// No traefik-net/monitoring-net membership for a bare publish.
	for _, n := range []string{"traefik-net", "monitoring-net"} {
		if _, ok := cf.Networks[n]; ok {
			t.Errorf("bare expose.external must not declare routing network %q:\n%s", n, s)
		}
	}
	if !strings.Contains(s, "published: 4318") || !strings.Contains(s, "mode: host") {
		t.Errorf("rendered expose.external missing publish fields:\n%s", s)
	}

	// Host-ful expose with external — Traefik routing AND raw publish.
	app = baseApp()
	app.Services["api"].Expose = &dsl.Expose{Port: 8080, Host: "api.example.com", External: 8080}
	if err := Validate(app); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	s, cf = renderCompose(t, app, nil)
	ports = cf.Services["api"].Ports
	if len(ports) != 1 || ports[0].Target != 8080 || ports[0].Published != 8080 || ports[0].Mode != "" {
		t.Fatalf("expose.external ports = %+v, want target/published 8080 ingress\n%s", ports, s)
	}
	if !strings.Contains(s, "traefik.http.routers.my-app-api.rule") {
		t.Errorf("host-ful expose must still emit Traefik router labels:\n%s", s)
	}

	// Validation: bad mode rejected.
	bad := baseApp()
	bad.Services["api"].Expose = &dsl.Expose{Port: 8080, Host: "api.example.com", External: 8080, Mode: "weird"}
	mustFail(t, bad, "expose.mode:")
}
