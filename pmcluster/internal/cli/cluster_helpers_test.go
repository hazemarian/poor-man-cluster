package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runtime"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
)

func TestChangedMarker(t *testing.T) {
	if got := changedMarker(true); got != "[rotated]" {
		t.Errorf("changedMarker(true) = %q, want [rotated]", got)
	}
	if got := changedMarker(false); got != "[unchanged]" {
		t.Errorf("changedMarker(false) = %q, want [unchanged]", got)
	}
}

func TestTernary(t *testing.T) {
	if got := ternary(true, "a", "b"); got != "a" {
		t.Errorf("ternary(true) = %q, want a", got)
	}
	if got := ternary(false, "a", "b"); got != "b" {
		t.Errorf("ternary(false) = %q, want b", got)
	}
	if got := ternary(1 < 2, 10, 20); got != 10 {
		t.Errorf("ternary typed = %d, want 10", got)
	}
}

func TestPrintNodePins(t *testing.T) {
	var out bytes.Buffer
	dc := &leaderFake{nodes: []runtime.Node{
		{Hostname: "nextrum-sy-1", Role: "manager", IsLeader: true},
		{Hostname: "nextrum-sy-2", Role: "worker"},
	}}
	printNodePins(&out, context.Background(), dc)
	s := out.String()
	for _, want := range []string{"Swarm nodes", "nextrum-sy-1", "manager (leader)", "nextrum-sy-2", "[worker]"} {
		if !strings.Contains(s, want) {
			t.Errorf("printNodePins output missing %q:\n%s", want, s)
		}
	}
}

func TestPrintNodePinsSilentOnError(t *testing.T) {
	var out bytes.Buffer
	dc := &leaderFake{nodeErr: errors.New("node list unavailable")}
	printNodePins(&out, context.Background(), dc)
	if out.Len() != 0 {
		t.Errorf("expected silent on error, got: %s", out.String())
	}
}

func TestClusterUpHasInputFlags(t *testing.T) {
	cmd := &cobra.Command{}
	cmd.Flags().String("domain", "", "")
	cmd.Flags().String("acme-email", "", "")
	cmd.Flags().String("cert", "", "")
	cmd.Flags().String("key", "", "")
	st, err := store.Open(t.TempDir() + "/data.db")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	defer st.Close()

	if clusterUpHasInput(cmd, st, context.Background()) {
		t.Error("no flags + empty store should be false")
	}
	if err := cmd.Flags().Set("domain", "example.com"); err != nil {
		t.Fatal(err)
	}
	if !clusterUpHasInput(cmd, st, context.Background()) {
		t.Error("--domain set should be true")
	}
}
