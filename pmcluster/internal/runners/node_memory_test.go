package runners

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/auth/swarmtls"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runtime"
)

// The two hosts from the live fleet, because the feature exists for exactly this
// pair: one cap that fits 3852 MB is permission to OOM 1839 MB.
const (
	smallHost = "nxt-sw-3-w" // 1839 MB
	bigHost   = "nxt-sw-6-w" // 3852 MB
)

func perNodeConfig() Config {
	return Config{
		Enabled:         true,
		Org:             "acme",
		Group:           "pmcluster",
		Nodes:           []string{smallHost, bigHost},
		Memory:          "2560m", // the fleet default: right for bigHost, fatal on smallHost
		NodeMemory:      map[string]string{smallHost: "1200m"},
		SocketProxy:     true,
		SocketPath:      "/run/pmcluster/docker.sock",
		IntervalSeconds: 60,
	}
}

// TestCapFor_PerNodeOverrideBeatsTheFleetDefault is the whole ask in one test:
// the same settings map must yield different caps for different nodes.
func TestCapFor_PerNodeOverrideBeatsTheFleetDefault(t *testing.T) {
	cfg := perNodeConfig()
	if got := cfg.CapFor(smallHost); got != "1200m" {
		t.Errorf("CapFor(%s) = %q, want the override 1200m", smallHost, got)
	}
	if got := cfg.CapFor(bigHost); got != "2560m" {
		t.Errorf("CapFor(%s) = %q, want the default 2560m (no override)", bigHost, got)
	}
	// An unlisted node falls back to the default, never to empty.
	if got := cfg.CapFor("some-other-node"); got != "2560m" {
		t.Errorf("CapFor(unknown) = %q, want the default", got)
	}
	// Swarm lowercases hostnames; an operator's value need not be lowercase.
	if got := cfg.CapFor("NXT-SW-3-W"); got != "1200m" {
		t.Errorf("CapFor is case-sensitive: %q", got)
	}
}

// A malformed override must degrade to the default. Returning it would produce a
// `docker run --memory=abc` that the daemon rejects at deploy time, i.e. a broken
// runner instead of a working one with a bigger cap than intended.
func TestCapFor_MalformedOverrideDegradesToTheDefault(t *testing.T) {
	cfg := perNodeConfig()
	cfg.NodeMemory = map[string]string{smallHost: "not-a-quantity"}
	if got := cfg.CapFor(smallHost); got != "2560m" {
		t.Fatalf("CapFor with a malformed override = %q, want the default", got)
	}
	cfg.NodeMemory = map[string]string{smallHost: "   "}
	if got := cfg.CapFor(smallHost); got != "2560m" {
		t.Fatalf("CapFor with a blank override = %q, want the default", got)
	}
}

// Removing the override is how an operator undoes it, and must fall back — not
// leave the node with no cap at all.
func TestCapFor_RemovingAnOverrideFallsBackToTheDefault(t *testing.T) {
	cfg := perNodeConfig()
	cfg.NodeMemory = nil
	if got := cfg.CapFor(smallHost); got != "2560m" {
		t.Fatalf("CapFor after removing the override = %q, want the default", got)
	}
}

func TestParseNodeMemory_FormatRoundTrip(t *testing.T) {
	raw := "nxt-sw-3-w=1200m, nxt-sw-6-w=2560m,nxt-sw-3-w=1g"
	got := ParseNodeMemory(raw)
	// last wins, keys lowercased, whitespace trimmed
	if got[smallHost] != "1g" {
		t.Errorf("duplicate host = %q, want the last value 1g", got[smallHost])
	}
	if got[bigHost] != "2560m" {
		t.Errorf("%s = %q", bigHost, got[bigHost])
	}

	formatted := FormatNodeMemory(got)
	again := ParseNodeMemory(formatted)
	if len(again) != len(got) || again[smallHost] != got[smallHost] || again[bigHost] != got[bigHost] {
		t.Fatalf("format/parse is not a round trip: %q → %v → %q → %v", raw, got, formatted, again)
	}
	if FormatNodeMemory(nil) != "" {
		t.Errorf("no overrides must format as the empty string, got %q", FormatNodeMemory(nil))
	}
}

func TestValidateNodeMemory_RefusesWhatTheDaemonWould(t *testing.T) {
	if err := ValidateNodeMemory("nxt-sw-3-w=1200m,nxt-sw-6-w=2560m"); err != nil {
		t.Fatalf("a valid list was refused: %v", err)
	}
	if err := ValidateNodeMemory(""); err != nil {
		t.Fatalf("empty must be allowed (no overrides): %v", err)
	}
	for _, bad := range []string{
		"nxt-sw-3-w",     // no cap
		"nxt-sw-3-w=",    // empty cap
		"=1200m",         // no host
		"nxt-sw-3-w=abc", // not a quantity
		// NOTE: "nxt-sw-3-w=1200" is deliberately NOT here. A bare number is bytes
		// in Docker's grammar and ValidateMemory has always accepted it (pinned by
		// TestValidation); refusing it here would silently change that contract.
		"nxt-sw-3-w=1200x", // unknown unit
		"nxt-sw-3-w=0m",    // a zero cap is not a cap
	} {
		if err := ValidateNodeMemory(bad); err == nil {
			t.Errorf("ValidateNodeMemory(%q) was accepted", bad)
		}
	}
}

// The cap has to TRAVEL: the leader resolves it, the worker must not have to know
// the settings (a worker cannot even read them — configs are manager-only).
func TestAssignmentCarriesEachNodesResolvedCap(t *testing.T) {
	nodes := []runtime.Node{
		{ID: "id-w1", Hostname: smallHost, Role: "worker"},
		{ID: "id-w2", Hostname: bigHost, Role: "worker"},
	}
	svc := newAssignmentService(newFakeGitHub(), nodes, perNodeConfig(), nil)

	caps := map[string]string{}
	for _, n := range nodes {
		rec := httptest.NewRecorder()
		svc.ServeHTTP(rec, requestFor(swarmtls.Node{ID: n.ID, Hostname: n.Hostname}, AssignmentPath))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status = %d: %s", n.Hostname, rec.Code, rec.Body.String())
		}
		var payload struct {
			Memory string `json:"memory"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
			t.Fatalf("%s: decode: %v", n.Hostname, err)
		}
		caps[n.Hostname] = payload.Memory
	}
	if caps[smallHost] != "1200m" {
		t.Errorf("the answer for %s carries memory=%q, want the override 1200m", smallHost, caps[smallHost])
	}
	if caps[bigHost] != "2560m" {
		t.Errorf("the answer for %s carries memory=%q, want the default 2560m", bigHost, caps[bigHost])
	}
	if caps[smallHost] == caps[bigHost] {
		t.Fatal("both nodes were given the same cap: the override did not travel")
	}
}

// A node whose override makes the cap fit must stop being refused; the same
// settings must still refuse the node with no override if its RAM is small.
func TestMemoryFitIsJudgedWithTheNodesCap(t *testing.T) {
	cfg := perNodeConfig()
	// 2560m is 139 % of 1839 MB: refused.
	if err := CheckMemoryFit(smallHost, 1839, true, cfg.CapFor(bigHost)); err == nil {
		t.Error("the fleet default was allowed on the 1839 MB node")
	}
	// The override is 65 % of the same host: allowed.
	if err := CheckMemoryFit(smallHost, 1839, true, cfg.CapFor(smallHost)); err != nil {
		t.Errorf("the node's own override was refused: %v", err)
	}
	// Unknown RAM never refuses: the rule is arithmetic, not a guess.
	if err := CheckMemoryFit(smallHost, 1839, false, cfg.CapFor(bigHost)); err != nil {
		t.Errorf("an unknown host size refused the cap: %v", err)
	}
}

var _ = context.Background
var _ = strings.TrimSpace
