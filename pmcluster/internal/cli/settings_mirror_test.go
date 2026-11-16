package cli

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/docker"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
)

// TestEnforceScope_SettingsReadIsAnsweredOnAFollower: since ADR 0003's slice 3 a follower's
// daemon mirrors the settings table, so the read form is a legitimate answer there — with an
// age on it. The guard used to refuse the whole command, which is the "standing by" this
// slice deletes.
func TestEnforceScope_SettingsReadIsAnsweredOnAFollower(t *testing.T) {
	apiURL = ""
	stubSelf(t, docker.SelfNode{Hostname: "nxt-sw-2-m", Role: "manager", SwarmActive: true, IsManager: true}, nil)

	for _, args := range [][]string{nil, {"get", "domain"}} {
		if err := enforceScope(clusterSettingsCmd, args); err != nil {
			t.Errorf("`cluster settings %v` must be allowed on a follower (the mirror answers it): %v", args, err)
		}
	}
}

// TestEnforceScope_SettingsWriteStillNeedsTheLeader: there is exactly one writer. A follower
// that accepted `set` would persist a value nothing replicates upward, and the operator would
// believe it landed.
func TestEnforceScope_SettingsWriteStillNeedsTheLeader(t *testing.T) {
	apiURL = ""
	stubSelf(t, docker.SelfNode{Hostname: "nxt-sw-2-m", Role: "manager", SwarmActive: true, IsManager: true}, nil)

	err := enforceScope(clusterSettingsCmd, []string{"set", "domain=example.test"})
	if err == nil {
		t.Fatal("`cluster settings set` must still be refused on a follower")
	}
	if !strings.Contains(err.Error(), "mirror") {
		t.Errorf("the refusal should point at the readable half: %v", err)
	}
}

// TestEnforceScope_MirrorReadDoesNotOpenTheDoorForOtherCommands pins the scope of the
// exception: it is `cluster settings` reads, not every cluster command.
func TestEnforceScope_MirrorReadDoesNotOpenTheDoorForOtherCommands(t *testing.T) {
	apiURL = ""
	stubSelf(t, docker.SelfNode{Hostname: "nxt-sw-2-m", Role: "manager", SwarmActive: true, IsManager: true}, nil)

	for _, cmd := range []*cobra.Command{stackListCmd, clusterUpdateCmd} {
		if err := enforceScope(cmd, nil); err == nil {
			t.Errorf("%s must stay leader-only", cmd.Name())
		}
	}
}

func mirrorTestStore(t *testing.T, appliedAt int64, synced bool) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "mirror.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if !synced {
		return st
	}
	if err := st.ApplySnapshot(context.Background(), &store.Snapshot{
		Version:     5,
		GeneratedAt: appliedAt,
		Settings:    []store.SnapshotSetting{{Key: "domain", Value: "example.test"}},
		Secrets:     []store.SnapshotSecret{},
		Users:       []store.SnapshotUser{},
	}, "nxt-sw-5-m", appliedAt); err != nil {
		t.Fatalf("apply snapshot: %v", err)
	}
	return st
}

// TestMirrorLabelNamesTheLeaderAndTheAge is the label above every mirrored answer: without
// it, a value the leader stopped refreshing an hour ago reads exactly like a fresh one.
func TestMirrorLabelNamesTheLeaderAndTheAge(t *testing.T) {
	applied := time.Now().Add(-30 * time.Second).Unix()
	st := mirrorTestStore(t, applied, true)

	cmd := &cobra.Command{}
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	printMirrorLabel(cmd, st)

	out := buf.String()
	for _, want := range []string{"mirrored: from nxt-sw-5-m", "version 5", "age_seconds="} {
		if !strings.Contains(out, want) {
			t.Errorf("the label must carry %q: %s", want, out)
		}
	}
}

// TestMirrorLabelSaysWhenThereIsNoSnapshotYet: an unsynced node still has rows (its own),
// and printing them unlabelled is exactly the lie this whole migration removes.
func TestMirrorLabelSaysWhenThereIsNoSnapshotYet(t *testing.T) {
	st := mirrorTestStore(t, 0, false)

	cmd := &cobra.Command{}
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	printMirrorLabel(cmd, st)

	out := buf.String()
	if !strings.Contains(out, "no snapshot applied") || !strings.Contains(out, "own copy") {
		t.Errorf("an unsynced mirror must say the values are this node's own: %s", out)
	}
}
