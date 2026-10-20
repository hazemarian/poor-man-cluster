package cluster

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runtime"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
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

// An upgraded cluster that predates edge_login_disabled must keep the Basic
// Auth gate it already has. Only an explicit "false" switches to native sessions.
func TestLoadEdgeLoginDisabledDefaultsToBasicAndPreservesSSOGate(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	if got := loadEdgeLoginDisabled(ctx, st); !got {
		t.Fatal("unset edge_login_disabled must keep Basic Auth (legacy default)")
	}
	if err := st.SetSetting(ctx, SettingEdgeLoginDisabled(), "true"); err != nil {
		t.Fatal(err)
	}
	if got := loadEdgeLoginDisabled(ctx, st); !got {
		t.Fatal("explicit edge_login_disabled=true should keep Basic mode")
	}
	if err := st.SetSetting(ctx, SettingEdgeLoginDisabled(), "false"); err != nil {
		t.Fatal(err)
	}
	if got := loadEdgeLoginDisabled(ctx, st); got {
		t.Fatal("explicit edge_login_disabled=false should select native sessions")
	}
	if err := st.SetSetting(ctx, SettingSSOEnabled(), "true"); err != nil {
		t.Fatal(err)
	}
	if got := loadEdgeLoginDisabled(ctx, st); !got {
		t.Fatal("SSO must keep the edge's own session login disabled")
	}
	if got := loadEdgeLoginDisabled(ctx, nil); !got {
		t.Fatal("a missing store must keep the legacy Basic Auth gate")
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

func TestLoadObservabilityParseableHostname_CapturesExistingPin(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	d := newFakeDocker()
	d.serviceInspect = runtime.ServiceInspectResult{Name: "observability_parseable"}
	d.runningHostname = "b15ab0ff62e5"

	got, err := loadObservabilityParseableHostname(context.Background(), st, d, false)
	if err != nil {
		t.Fatalf("loadObservabilityParseableHostname: %v", err)
	}
	if got != "b15ab0ff62e5" {
		t.Fatalf("hostname = %q, want existing service pin", got)
	}
	if stored := st.SettingDefault(context.Background(), SettingObservabilityParseableHostname(), ""); stored != got {
		t.Fatalf("stored hostname = %q, want %q", stored, got)
	}
}

func TestLoadObservabilityParseableHostname_ExistingEmptyTemplateRequiresRunningInspection(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	d := newFakeDocker()
	d.serviceInspect = runtime.ServiceInspectResult{Name: "observability_parseable"}
	d.runningHostErr = errors.New("container inspect failed")

	_, err = loadObservabilityParseableHostname(context.Background(), st, d, false)
	if err == nil {
		t.Fatal("expected legacy empty-template inspection failure")
	}
	if !errors.Is(err, d.runningHostErr) {
		t.Fatalf("error must wrap the inspection failure, got %v", err)
	}
	if !strings.Contains(err.Error(), "docker service ps observability_parseable") {
		t.Fatalf("error must say how to fix the legacy service, got %v", err)
	}
	if got := st.SettingDefault(context.Background(), SettingObservabilityParseableHostname(), ""); got != "" {
		t.Fatalf("failed inspection must not persist a default hostname: %q", got)
	}
}

func TestLoadObservabilityParseableHostname_InspectFailureDoesNotDefault(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	d := newFakeDocker()
	d.serviceInspectErr = errors.New("daemon unavailable")

	if _, err := loadObservabilityParseableHostname(context.Background(), st, d, false); err == nil {
		t.Fatal("expected service inspect failure")
	}
	if got := st.SettingDefault(context.Background(), SettingObservabilityParseableHostname(), ""); got != "" {
		t.Fatalf("inspect failure must not persist a default hostname: %q", got)
	}
}

func TestLoadObservabilityParseableHostname_DefaultsStableFreshValue(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	got, err := loadObservabilityParseableHostname(context.Background(), st, nil, true)
	if err != nil {
		t.Fatalf("loadObservabilityParseableHostname: %v", err)
	}
	if got != defaultObservabilityParseableHostname {
		t.Fatalf("hostname = %q, want %q", got, defaultObservabilityParseableHostname)
	}
	if stored := st.SettingDefault(context.Background(), SettingObservabilityParseableHostname(), ""); stored != got {
		t.Fatalf("stored hostname = %q, want %q", stored, got)
	}
}

func TestLoadObservabilityParseableHostname_InstalledMissingServiceFailsClosed(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	d := newFakeDocker()
	if _, err := loadObservabilityParseableHostname(context.Background(), st, d, false); err == nil {
		t.Fatal("expected installed missing-service failure")
	}
	if got := st.SettingDefault(context.Background(), SettingObservabilityParseableHostname(), ""); got != "" {
		t.Fatalf("installed missing service must not persist a fresh default: %q", got)
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
