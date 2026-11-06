package backups

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Retention did not apply to the copies that cost money.
//
// The node-local archive was pruned on every run, but nothing ever deleted from
// the in-cluster store or the offsite bucket: an upload loop that adds an artifact
// per stack per hour and removes none grows without limit, which is a bill and,
// eventually, a full bucket that silently fails every upload. The same policy now
// governs all three legs.

// planPrune decides which artifacts may be deleted. It is the single implementation
// of the retention rules; the local, store and S3 pruners all call it so they can
// never disagree about what "keep 15 days" means.
func planPrune(byStack map[string][]Artifact, policy Retention, now time.Time) (del []Artifact, kept int) {
	if policy.KeepLast < 1 {
		policy.KeepLast = 1
	}
	for _, arts := range byStack {
		sort.Slice(arts, func(i, j int) bool { return arts[i].At.After(arts[j].At) })
		newestPerDay := map[string]Artifact{}
		for _, a := range arts {
			day := a.At.Format("2006-01-02")
			if _, seen := newestPerDay[day]; !seen {
				newestPerDay[day] = a
			}
		}
		for i, a := range arts {
			if i < policy.KeepLast {
				kept++
				continue
			}
			if !shouldDelete(a.At, policy, now, newestPerDay[a.At.Format("2006-01-02")].Name == a.Name) {
				kept++
				continue
			}
			del = append(del, a)
		}
	}
	return del, kept
}

// groupArtifacts parses names into per-node, per-stack buckets.
func groupArtifacts(names []string, policy Retention) (map[string][]Artifact, int) {
	byStack := map[string][]Artifact{}
	skipped := 0
	for _, n := range names {
		art, ok := ParseArtifact(filepath.Base(n))
		if !ok {
			continue
		}
		if art.Stack == "" && !policy.IncludeLegacy {
			skipped++
			continue
		}
		key := art.Node + "/" + art.Stack
		byStack[key] = append(byStack[key], art)
	}
	return byStack, skipped
}

// PruneS3 applies the retention policy to the offsite bucket.
func PruneS3(ctx context.Context, cfg S3Config, policy Retention, now time.Time, dryRun bool) (PruneResult, error) {
	res := PruneResult{DryRun: dryRun}
	if !cfg.Configured() {
		return res, nil
	}
	objs, err := listS3Objects(ctx, cfg, "backup-")
	if err != nil {
		return res, fmt.Errorf("list offsite objects: %w", err)
	}
	names := make([]string, 0, len(objs))
	for _, o := range objs {
		names = append(names, o.Key)
	}
	byStack, skipped := groupArtifacts(names, policy)
	res.SkippedOld = skipped
	del, kept := planPrune(byStack, policy, now)
	res.Kept = kept
	for _, a := range del {
		if dryRun {
			res.Deleted = append(res.Deleted, a.Name+" (would delete)")
			continue
		}
		if err := deleteS3Object(ctx, cfg, a.Name); err != nil {
			res.Failed = append(res.Failed, a.Name+": "+err.Error())
			res.Kept++
			continue
		}
		res.Deleted = append(res.Deleted, a.Name)
	}
	return res, nil
}

// deleteS3Object removes one object. A DELETE is signed over the empty-payload
// hash, not the object's own hash — that is the whole difference from the PUT.
func deleteS3Object(ctx context.Context, cfg S3Config, key string) error {
	endpoint := strings.TrimSuffix(cfg.Endpoint, "/")
	if endpoint == "" {
		return fmt.Errorf("s3: no endpoint")
	}
	if cfg.Bucket == "" {
		return fmt.Errorf("s3: no bucket")
	}
	u := endpoint + "/" + cfg.Bucket + "/" + escapePath(key)
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, u, nil)
	if err != nil {
		return err
	}
	for k, v := range signV4Request(cfg, http.MethodDelete, key, nil, emptyPayloadSHA, time.Now().UTC()) {
		req.Header[k] = v
	}
	resp, err := s3UploadClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("s3 delete %s: %s", key, resp.Status)
	}
	return nil
}

// ControlPlaneKeep is how many control-plane archives a remote target keeps.
// Two is enough: the newest is what a promoted leader restores, the one behind
// it is the "the newest is corrupt" fallback — and the kit itself lives in Raft
// configs, so these tarballs are the pre-L2 fallback, not the primary copy.
const ControlPlaneKeep = 2

// pruneControlPlaneArchives ages out pmcluster-ctlplane-* objects.
//
// They are written by the control-plane archiver, a different producer with a
// different name shape than the stack artifacts, and nothing ever pruned them:
// PruneS3 lists the "backup-" prefix only. On the live store that had grown to
// 40 GB on a host with 6 GB free, all of it invented by a family no retention
// rule could see.
// PruneControlPlaneArchives is pruneControlPlaneArchives for callers outside
// this package (the hourly agent, and a manual reclaim).
func PruneControlPlaneArchives(ctx context.Context, cfg S3Config, now time.Time, dryRun bool) (int, int, error) {
	return pruneControlPlaneArchives(ctx, cfg, now, dryRun)
}

func pruneControlPlaneArchives(ctx context.Context, cfg S3Config, now time.Time, dryRun bool) (deleted, kept int, err error) {
	objs, err := listS3Objects(ctx, cfg, "pmcluster-ctlplane-")
	if err != nil {
		return 0, 0, err
	}
	names := make([]string, 0, len(objs))
	for _, o := range objs {
		names = append(names, o.Key)
	}
	// Names carry an RFC3339 timestamp, so newest-last sort is lexical.
	sort.Strings(names)
	if len(names) <= ControlPlaneKeep {
		return 0, len(names), nil
	}
	del := names[:len(names)-ControlPlaneKeep]
	for _, key := range del {
		if dryRun {
			deleted++
			continue
		}
		if derr := DeleteS3Object(ctx, cfg, key); derr != nil {
			return deleted, len(names) - deleted, derr
		}
		deleted++
	}
	return deleted, len(names) - deleted, nil
}
