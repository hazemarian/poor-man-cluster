package doctor

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/storage"
)

// checkUnbackedData answers "what is on the volume that no backup covers?".
//
// The agent archives the stacks in its plan — the ones the swarm currently runs with a volume.
// A directory under the volume root that belongs to no such stack is archived by NOTHING: it is
// either the leftover of a removed stack or data mounted there by hand. Measured on production
// 2026-11-16: `dep-s2` and `dep-s3` on nxt-sw-1-m held 954 files each, warned about hourly in a
// journal nobody reads, and appeared in no check at all — `backup coverage` only asks whether a
// LIVE stack's volume sits on a storage node.
//
// Local by nature: it describes THIS node's volume root, because that is the only one this
// process can see. Run `pmcluster doctor` on each storage node to get the whole picture.
func checkUnbackedData(ctx context.Context, v backupView) Check {
	const name = "unbacked data"
	root := v.VolumeRoot
	if root == "" {
		// A node that never had the setting still has the stock root; and a node with no
		// volume at all skips on the ReadDir below rather than claiming an empty answer.
		root = storage.DefaultVolumeRoot
	}
	v.VolumeRoot = root
	if v.Runtime == nil {
		return Check{Name: name, Status: StatusSkip, Detail: "no swarm client on this host"}
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return Check{Name: name, Status: StatusSkip, Detail: "this node has no volume root (" + v.VolumeRoot + ")"}
		}
		return Check{Name: name, Status: StatusWarn, Detail: "could not read " + v.VolumeRoot + ": " + err.Error()}
	}

	// The live stacks, from the swarm: a service's Stack field is the stack it belongs to.
	svcs, err := v.Runtime.ServiceList(ctx)
	if err != nil {
		return Check{Name: name, Status: StatusSkip, Detail: "could not list services: " + err.Error()}
	}
	live := map[string]bool{}
	for _, s := range svcs {
		if s.Stack != "" {
			live[s.Stack] = true
		}
	}

	var withData, empty []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dirName := e.Name()
		// `lost+found` is the filesystem's, not anyone's data — and an ext4 volume always has one.
		if dirName == "lost+found" || live[dirName] {
			continue
		}
		files := countFiles(filepath.Join(v.VolumeRoot, dirName))
		if files > 0 {
			withData = append(withData, fmt.Sprintf("%s (%d file(s))", dirName, files))
		} else {
			empty = append(empty, dirName)
		}
	}
	sort.Strings(withData)
	sort.Strings(empty)

	if len(withData) > 0 {
		detail := "directories under " + v.VolumeRoot + " belong to no stack in this swarm, so no backup will ever archive them: " +
			strings.Join(withData, ", ") + " — either restore the stack they belong to (a deploy re-adopts the directory) or move the data out and delete it; `docs/backup.md` has the procedure"
		if len(empty) > 0 {
			detail += ". Empty and safe to delete: " + strings.Join(empty, ", ")
		}
		return Check{Name: name, Status: StatusFail, Detail: detail}
	}
	if len(empty) > 0 {
		return Check{Name: name, Status: StatusOK,
			Detail: fmt.Sprintf("every directory with data under %s belongs to a live stack; %d empty leftover(s) could be removed: %s",
				v.VolumeRoot, len(empty), strings.Join(empty, ", "))}
	}
	return Check{Name: name, Status: StatusOK, Detail: "every directory under " + v.VolumeRoot + " belongs to a live stack"}
}

// countFiles counts regular files under root, bounded: this is a health check, not an inventory,
// and a 90 GiB volume must not turn it into a walk of the whole tree.
func countFiles(root string) int {
	const limit = 20000
	n := 0
	_ = filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // unreadable subtree: nothing to count, and not this check's business
		}
		if d.IsDir() {
			return nil
		}
		n++
		if n >= limit {
			return fs.SkipAll
		}
		return nil
	})
	return n
}
