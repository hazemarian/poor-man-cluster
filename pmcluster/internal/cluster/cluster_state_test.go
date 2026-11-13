package cluster

import (
	"context"
	"strings"
	"testing"
)

func TestRequestedTLSMode(t *testing.T) {
	cases := []struct {
		in   UpInput
		want string
	}{
		{UpInput{ACMEEmail: "a@b.c"}, "acme"},
		{UpInput{CertPath: "/c.pem"}, "cert"},
		{UpInput{KeyPath: "/k.pem"}, "cert"},
		{UpInput{}, ""},
	}
	for i, c := range cases {
		if got := requestedTLSMode(c.in); got != c.want {
			t.Errorf("case %d: requestedTLSMode = %q, want %q", i, got, c.want)
		}
	}
}

func TestMergeTLSState_NoStored(t *testing.T) {
	in := UpInput{ACMEEmail: "a@b.c"}
	merged, err := mergeTLSState(in, tlsState{})
	if err != nil {
		t.Fatalf("mergeTLSState: %v", err)
	}
	if merged != in {
		t.Error("input should be unchanged when no stored state")
	}
}

func TestMergeTLSState_ReusesStoredPathsWhenFlagsOmitted(t *testing.T) {
	in := UpInput{}
	state := tlsState{Mode: "cert", CertPath: "/c.pem", KeyPath: "/k.pem"}
	merged, err := mergeTLSState(in, state)
	if err != nil {
		t.Fatalf("mergeTLSState: %v", err)
	}
	if merged.CertPath != "/c.pem" || merged.KeyPath != "/k.pem" {
		t.Errorf("stored cert/key not filled in: %+v", merged)
	}
}

func TestMergeTLSState_SameModeAllowsFlags(t *testing.T) {
	in := UpInput{CertPath: "/new.pem"}
	state := tlsState{Mode: "cert", CertPath: "/old.pem", KeyPath: "/k.pem"}
	merged, err := mergeTLSState(in, state)
	if err != nil {
		t.Fatalf("mergeTLSState: %v", err)
	}
	if merged.CertPath != "/new.pem" {
		t.Errorf("supplied cert should win, got %q", merged.CertPath)
	}
	if merged.KeyPath != "/k.pem" {
		t.Errorf("missing key should be filled from store, got %q", merged.KeyPath)
	}
}

func TestMergeTLSState_RefusesModeFlipUnlessForced(t *testing.T) {

	in := UpInput{CertPath: "/c.pem", KeyPath: "/k.pem"}
	state := tlsState{Mode: "acme", ACMEEmail: "a@b.c"}
	if _, err := mergeTLSState(in, state); err == nil {
		t.Fatal("expected mode-flip error")
	}
	in.ForceTLSMode = true
	if _, err := mergeTLSState(in, state); err != nil {
		t.Fatalf("force should allow the flip: %v", err)
	}
}

// TestLoadNodeDockerRestartKnown is the regression for a node that disarmed its own
// docker watchdog. `node_docker_restart` is leader-owned, so a worker's store has no
// row for it and the plain loader answers "false" — fine as a value, fatal as a value
// to WRITE over the decision the leader already pushed to the node's policy file
// (nxt-sw-6-w, 2026-11-13: dockerd dead, daemon restarted, policy rewritten as false,
// watchdog sat through three failed probes doing nothing).
func TestLoadNodeDockerRestartKnown(t *testing.T) {
	s, _ := newTestDeps(t)
	ctx := context.Background()

	// A store that cannot answer must say so. "false, true" here would be a decision
	// this node never made.
	if armed, known := LoadNodeDockerRestartKnown(ctx, s); known || armed {
		t.Fatalf("empty store: armed=%v known=%v, want false/false", armed, known)
	}

	if err := s.SetSetting(ctx, settingNodeDockerRestart, "true"); err != nil {
		t.Fatal(err)
	}
	if armed, known := LoadNodeDockerRestartKnown(ctx, s); !known || !armed {
		t.Fatalf("after set true: armed=%v known=%v, want true/true", armed, known)
	}

	// The value the console and the CLI write is whatever case the operator typed.
	if err := s.SetSetting(ctx, settingNodeDockerRestart, " FALSE "); err != nil {
		t.Fatal(err)
	}
	if armed, known := LoadNodeDockerRestartKnown(ctx, s); !known || armed {
		t.Fatalf("after set FALSE: armed=%v known=%v, want false/true", armed, known)
	}
}

// TestLoadObservabilityRetentionDefaults: an untouched store returns the
// per-stream defaults (30 logs / 14 metrics / 10 traces), matching the
// provision script's own fallback.
func TestLoadObservabilityRetentionDefaults(t *testing.T) {
	s, _ := newTestDeps(t)
	logs, metrics, traces := loadObservabilityRetention(context.Background(), s)
	if logs != defaultLogsRetentionDays || metrics != defaultMetricsRetentionDays || traces != defaultTracesRetentionDays {
		t.Fatalf("defaults = (%d, %d, %d), want (%d, %d, %d)",
			logs, metrics, traces, defaultLogsRetentionDays, defaultMetricsRetentionDays, defaultTracesRetentionDays)
	}
}

// TestLoadObservabilityRetentionConfigured: configured values win; an
// unparseable or non-positive value falls back to its own default.
func TestLoadObservabilityRetentionConfigured(t *testing.T) {
	s, _ := newTestDeps(t)
	ctx := context.Background()

	if err := s.SetSetting(ctx, settingObservabilityLogsRetentionDays, "21"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSetting(ctx, settingObservabilityMetricsRetentionDays, "abc"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSetting(ctx, settingObservabilityTracesRetentionDays, "0"); err != nil {
		t.Fatal(err)
	}

	logs, metrics, traces := loadObservabilityRetention(ctx, s)
	if logs != 21 {
		t.Errorf("logs = %d, want 21", logs)
	}
	if metrics != defaultMetricsRetentionDays {
		t.Errorf("metrics = %d, want default %d", metrics, defaultMetricsRetentionDays)
	}
	if traces != defaultTracesRetentionDays {
		t.Errorf("traces = %d, want default %d", traces, defaultTracesRetentionDays)
	}
}

// TestObservabilityProvisionScriptRetentionFallbacks: a missing env var keeps
// today's behaviour — the script falls back to the 30/14/10 defaults rather
// than leaving the streams unbounded.
func TestObservabilityProvisionScriptRetentionFallbacks(t *testing.T) {
	body, err := RenderObservabilityProvisionScript(RenderInput{})
	if err != nil {
		t.Fatalf("render provision script: %v", err)
	}
	for _, want := range []string{
		`${LOGS_RETENTION_DAYS:-30}`,
		`${METRICS_RETENTION_DAYS:-14}`,
		`${TRACES_RETENTION_DAYS:-10}`,
	} {
		if !strings.Contains(string(body), want) {
			t.Errorf("provision script missing fallback %q", want)
		}
	}
}
