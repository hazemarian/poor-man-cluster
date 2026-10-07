// Package stackdrift is the shared drift detector for pmcluster stacks. It
// compares the live Swarm against a freshly rendered compose so a Swarm-side
// change that the stored rendered hash cannot see is still caught and
// re-applied: every service carries a content-hash label, and the control
// loop always checks the label on the live services against the fresh render.
package stackdrift

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"sigs.k8s.io/yaml"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runtime"
)

// ContentHash returns the lowercase hex sha256 (64 chars) of data. It MUST
// stay byte-identical to store.ConfigHash so the hashes the store records and
// the labels stamped on services remain directly comparable (the store is not
// imported here to avoid an import cycle; check_test.go asserts the equality).
func ContentHash(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// InSync reports whether the live Swarm matches the freshly rendered compose
// (platform or app stack). It is the drift detector the stored rendered hash
// cannot provide: the DB hash records what pmcluster last INTENDED to deploy,
// so a Swarm-side change (a manual `docker service update`, a half-applied
// deploy, an upgrade that skipped one service) leaves the store reporting
// "nothing to redeploy" forever (BUG-017 — the wafaa control-plane backup ran
// for weeks without its offsite/WebDAV destination for exactly this reason).
//
// Two independent signals are checked:
//   - every service the render expects is present in the stack (detects a
//     missing or renamed service), and
//   - a service that carries a RenderedHashLabel whose value differs from the
//     freshly rendered compose's label was deployed from a DIFFERENT render
//     (detects a half-applied deploy, a skipped service, an older binary) —
//     the exact class of drift that let the wafaa control-plane backup run
//     without its offsite destination.
//
// Label drift is reported under two rules:
//   - PARTIAL: some (but not all) services carry the label. The deploy was
//     cut off mid-flight (e.g. the daemon restarted during an install), so it
//     is drift and must be re-applied.
//   - ALL-UNLABELLED: when the fresh compose carries a label (wantHash != "")
//     but NO live service carries one. At these call sites InSync is only
//     consulted after the stored rendered hash already matches the fresh
//     render — meaning a labelled render was supposedly applied. Zero labelled
//     live services proves it was NOT actually applied (a failed deploy that
//     left the store's hash stale — BUG-018), so it is drift and must be
//     re-applied. This intentionally supersedes the old rationale ("the stack
//     predates the label"), which only held when the label was first
//     introduced, not when a labelled render was the recorded truth.
//
// A render with no label (wantHash == "") predates the feature, so only the
// service set is compared and no label rule applies.
//
// Extra live services the render no longer produces are deliberately ignored —
// `docker stack deploy` cannot remove them, so flagging them would redeploy
// forever. This also cannot see a hand-edited env on a live service (a manual
// service update does not change labels); it detects SWARM-vs-RENDER drift,
// not render-internal tampering.
//
// A nil client is treated as "in sync" so store-only callers never
// force-redeploy. Any error is returned so the caller can decide to
// assume-in-sync (avoiding a redeploy loop).
func InSync(ctx context.Context, d runtime.Client, stack string, freshCompose []byte) (bool, string, error) {
	if d == nil {
		return true, "", nil
	}
	want, wantHash, err := composeServiceNames(freshCompose)
	if err != nil {
		return false, "", err
	}
	services, err := d.ServiceList(ctx)
	if err != nil {
		return false, "", err
	}
	live := map[string]string{}
	for _, svc := range services {
		if svc.Stack == stack {
			live[strings.TrimPrefix(svc.Name, stack+"_")] = svc.Labels[runtime.RenderedHashLabel]
		}
	}
	if len(live) == 0 {
		return false, "absent from the swarm", nil
	}
	// Only services the render WANTS are checked. A live service the render no
	// longer produces is ignored on purpose: `docker stack deploy` cannot
	// remove a service absent from the compose (no --prune), so flagging it
	// would re-deploy on every update forever.
	labeled, unlabeled := 0, 0
	for name := range want {
		got, ok := live[name]
		if !ok {
			return false, "a required service is missing", nil
		}
		if wantHash == "" {
			continue // render predates the label: only the service set is compared
		}
		switch {
		case got == "":
			unlabeled++
		case got != wantHash:
			return false, "drifted from the rendered compose", nil
		default:
			labeled++
		}
	}
	// A stack where only SOME services carry the label is a PARTIALLY APPLIED
	// deploy — the deploy was cut off (interrupted mid `stack deploy`, e.g. the
	// daemon restarted during an install) or a service was created by hand — so
	// treat it as drift and re-apply.
	if labeled > 0 && unlabeled > 0 {
		return false, "some services are missing the rendered-hash label (partial deploy)", nil
	}
	// A stack where NONE carry the label while the fresh render REQUIRES one
	// proves the labelled render was never actually applied (a failed deploy
	// that left the stored hash matching). This only runs when wantHash != ""
	// — a render with no label predates the feature and is left in sync.
	if labeled == 0 && wantHash != "" {
		return false, "no live service carries the rendered-hash label (the labelled render was not applied)", nil
	}
	return true, "", nil
}

// composeServiceNames parses a rendered compose file and returns its service
// names plus the rendered-hash label stamped on each service ("" when the
// render predates the label, in which case only the service set is compared).
func composeServiceNames(composeYAML []byte) (map[string]struct{}, string, error) {
	var doc struct {
		Services map[string]struct {
			Deploy struct {
				Labels map[string]string `json:"labels"`
			} `json:"deploy"`
		} `json:"services"`
	}
	if err := yaml.Unmarshal(composeYAML, &doc); err != nil {
		return nil, "", fmt.Errorf("parse rendered compose: %w", err)
	}
	names := make(map[string]struct{}, len(doc.Services))
	hash := ""
	for n, svc := range doc.Services {
		names[n] = struct{}{}
		if hash == "" {
			hash = svc.Deploy.Labels[runtime.RenderedHashLabel]
		}
	}
	return names, hash, nil
}
