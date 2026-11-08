package controlplane

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// StoreSwapper is the control-plane store as adoption needs it: it closes the
// live SQLite handle, runs fn with no connection open, then reopens the
// database at the same path. *store.Store implements it (Store.Swap), and the
// interface lives here so the predicate and the extraction can be tested without
// a database.
type StoreSwapper interface {
	Swap(fn func() error) error
}

// Adoption reports what one leadership-gain check did.
type Adoption struct {
	// Adopted is true when this node replaced its control plane.
	Adopted bool
	// Config is the state config the check considered (the newest one).
	Config string
	// Revision is the snapshot time of that config.
	Revision time.Time
	// PreviousRevision is the marker this node carried before the check
	// (zero when it had never synced).
	PreviousRevision time.Time
	// KeyChanged reports that the adopted encryption key differs from the one
	// this node had — the caller must reload its Cipher, or every credential
	// ciphertext in the freshly adopted DB is undecryptable.
	KeyChanged bool
	// Files is how many kit files were extracted (0 when nothing was adopted).
	Files int
	// Reason names the predicate that fired.
	Reason string
}

// stagingPrefix names the scratch directory an adoption extracts into. It lives
// inside DataDir so the final rename stays on one filesystem (an atomic
// rename), and it is excluded from BOTH the kit tarball and the change walk for
// the same reason the revision marker is: a file this package writes must never
// make the kit look dirty or end up inside the next snapshot.
const stagingPrefix = ".adopt-"

// AdoptOnLeadership runs the startup freshness predicate once, on the
// follower→leader transition, and adopts the newest Raft-replicated kit when it
// is newer than this node's.
//
// The startup path (serve, before store.Open) and this one decide with the same
// predicates — no key, a missing or older `.state_revision`, or a different
// encryption key — so the two can never disagree. What differs is the store:
// here the daemon is already serving, so a restore must close its SQLite
// connection, replace `data.db` and `.encryption_key`, and reopen. That is what
// StoreSwapper.Swap provides, and it is why this is NOT wired to a tick: a node
// that stays leader must not keep closing its store.
//
// Call it on every leadership GAIN (and only then). It is safe to call when
// nothing needs doing: it checks, finds the node current, and returns without
// touching a file.
//
// Failure is contained by design. An unreadable kit, a state config whose key
// config is missing, or a payload that does not extract leaves the current
// database in place and serving; the error is logged at error level and
// returned so the caller can decide, but nothing is half-written: the kit is
// extracted into a staging directory and renamed into place, with `data.db`
// renamed last.
func (k *Kit) AdoptOnLeadership(ctx context.Context, swap StoreSwapper) (Adoption, error) {
	if k.Docker == nil {
		return Adoption{}, nil // standalone/local: no Raft configs to adopt from
	}
	log := k.Log.With().Str("component", "controlplane").Logger()

	snap, err := k.newestSnapshot(ctx)
	switch {
	case errors.Is(err, ErrNoSnapshots):
		log.Debug().Msg("no control-plane state config; nothing to adopt on leadership gain")
		return Adoption{}, nil
	case err != nil:
		if IsSwarmUnavailable(err) {
			// A worker, or a swarm without a Raft leader right now. Nothing to
			// read, nothing to adopt, nothing to crash: the next leadership
			// transition retries.
			log.Warn().Err(err).Msg("swarm unavailable; skipping control-plane adoption on leadership gain")
			return Adoption{}, nil
		}
		log.Error().Err(err).Msg("control-plane adoption check failed on leadership gain; still serving the CURRENT database")
		return Adoption{}, err
	}

	// Never adopt a snapshot this node published itself. The revision marker
	// written by Snapshot already makes the predicate a no-op; this covers the
	// case where that marker write failed, which would otherwise close and
	// re-extract a store that is already the source of the snapshot.
	if own := k.publishedConfig(); own != "" && snap.stateConfig == own {
		log.Debug().Str("config", snap.stateConfig).
			Msg("newest control-plane snapshot is this node's own publication; nothing to adopt")
		return Adoption{Config: snap.stateConfig, Revision: snap.at}, nil
	}

	// A cluster whose newest state config has no key config is broken even when
	// this node happens to be current: say so, once per transition.
	if snap.keyErr != nil {
		log.Warn().Err(snap.keyErr).Str("config", snap.stateConfig).Str("key-config", snap.keyConfig).
			Msg("newest control-plane state config has no readable key config (security split broken); an adoption would be refused")
	}

	f := k.freshnessOf(snap)
	if !f.need {
		log.Info().Str("config", snap.stateConfig).
			Str("revision", snap.at.Format(time.RFC3339Nano)).
			Str("local-revision", formatRevision(f.localRev)).
			Msg("control plane is already at the newest snapshot; no adoption on leadership gain")
		return Adoption{Config: snap.stateConfig, Revision: snap.at, PreviousRevision: f.localRev}, nil
	}

	// A data kit without its key is a control plane that cannot decrypt its own
	// credentials. Refuse before the store is touched at all.
	if snap.keyErr != nil {
		err := fmt.Errorf("state config %s exists but its key config %s is unreadable (%w) — refusing to adopt a ciphertext without its key (security split); keeping the CURRENT database", snap.stateConfig, snap.keyConfig, snap.keyErr)
		log.Error().Err(err).Str("config", snap.stateConfig).Str("key-config", snap.keyConfig).
			Msg("control-plane adoption REFUSED on leadership gain")
		return Adoption{Config: snap.stateConfig, Revision: snap.at, PreviousRevision: f.localRev}, err
	}
	if swap == nil {
		err := errors.New("cannot adopt control-plane state: no store handle supplied")
		log.Error().Err(err).Msg("control-plane adoption refused on leadership gain")
		return Adoption{}, err
	}

	// Extract alongside the live data dir first. Nothing under DataDir changes
	// until the kit is fully extracted and validated, so an unreadable payload
	// cannot leave a truncated data.db behind.
	files, staged, err := k.stageKit(snap.kit, snap.at)
	if err != nil {
		log.Error().Err(err).Str("config", snap.stateConfig).
			Msg("control-plane adoption REFUSED on leadership gain: kit not usable; still serving the CURRENT database")
		return Adoption{Config: snap.stateConfig, Revision: snap.at, PreviousRevision: f.localRev}, err
	}
	defer func() { _ = os.RemoveAll(staged) }()

	// Close the store, rename the staged kit into place (data.db last, so it is
	// one atomic rename), reopen. If promoteStaged fails the handle is still
	// reopened over the untouched database and the daemon keeps serving it.
	if err := swap.Swap(func() error { return k.promoteStaged(staged, snap) }); err != nil {
		log.Error().Err(err).Str("config", snap.stateConfig).
			Msg("control-plane adoption failed on leadership gain; still serving the CURRENT database")
		return Adoption{Config: snap.stateConfig, Revision: snap.at, PreviousRevision: f.localRev}, err
	}

	adoption := Adoption{
		Adopted:          true,
		Config:           snap.stateConfig,
		Revision:         snap.at,
		PreviousRevision: f.localRev,
		KeyChanged:       f.keyChanged,
		Files:            files,
		Reason:           f.reason,
	}
	log.Info().
		Str("config", adoption.Config).
		Str("revision", adoption.Revision.Format(time.RFC3339Nano)).
		Str("previous-revision", formatRevision(adoption.PreviousRevision)).
		Str("reason", adoption.Reason).
		Bool("key-adopted", adoption.KeyChanged).
		Int("files", adoption.Files).
		Msg("control-plane data ADOPTED on leadership gain: store closed, data.db + .encryption_key replaced, store reopened, revision advanced")
	return adoption, nil
}

// formatRevision renders a revision marker for logs ("never" for the zero time).
func formatRevision(at time.Time) string {
	if at.IsZero() {
		return "never"
	}
	return at.Format(time.RFC3339Nano)
}

// publishedConfig returns the name of the newest state config this Kit created,
// or "" when it has not published one in this process.
func (k *Kit) publishedConfig() string {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.published
}

// setPublishedConfig records the state config this Kit just created.
func (k *Kit) setPublishedConfig(name string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.published = name
}

// stageKit extracts a kit payload into a fresh staging directory inside DataDir
// and validates that it carries a data.db. It returns the file count and the
// staging path; the caller owns the directory.
func (k *Kit) stageKit(payload []byte, at time.Time) (int, string, error) {
	if err := os.MkdirAll(k.DataDir, 0o755); err != nil {
		return 0, "", fmt.Errorf("ensure data dir: %w", err)
	}
	k.clearStaging()
	staged := filepath.Join(k.DataDir, stagingPrefix+strconv.FormatInt(time.Now().UnixNano(), 10))
	if err := os.MkdirAll(staged, 0o700); err != nil {
		return 0, "", fmt.Errorf("create staging dir: %w", err)
	}
	n, err := k.extractKitTo(staged, payload, at)
	if err != nil {
		_ = os.RemoveAll(staged)
		return 0, "", fmt.Errorf("extract control-plane kit: %w", err)
	}
	if _, err := os.Stat(filepath.Join(staged, "data.db")); err != nil {
		_ = os.RemoveAll(staged)
		return 0, "", fmt.Errorf("control-plane kit carries no data.db: %w", err)
	}
	return n, staged, nil
}

// clearStaging removes staging directories a previous adoption left behind
// (interrupted after extraction, before the rename).
func (k *Kit) clearStaging() {
	entries, err := os.ReadDir(k.DataDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), stagingPrefix) {
			_ = os.RemoveAll(filepath.Join(k.DataDir, e.Name()))
		}
	}
}

// promoteStaged moves a fully extracted kit from the staging directory into
// DataDir. Every entry is a rename on the same filesystem; `data.db` is renamed
// LAST so that a crash or a failure part-way through leaves the previous
// database in place and intact rather than a truncated one.
//
// The encryption key is written from the key config and renamed into place the
// same way. It is always adopted with the DB: a cluster DB beside a foreign key
// (or the reverse) leaves every credential ciphertext unreadable.
func (k *Kit) promoteStaged(staged string, snap *snapshot) error {
	base, err := filepath.Abs(k.DataDir)
	if err != nil {
		return err
	}
	var dbSrc string
	err = filepath.Walk(staged, func(path string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(staged, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		dst := filepath.Join(base, rel)
		if fi.IsDir() {
			return os.MkdirAll(dst, 0o755)
		}
		if !fi.Mode().IsRegular() {
			return nil
		}
		switch rel {
		case "data.db":
			// Last: the single atomic step that swaps the live database.
			dbSrc = path
			return nil
		case ".encryption_key", ".state_revision":
			// Never taken from the kit: the key has its own config (security
			// split) and the revision is this node's own bookkeeping, written
			// after the swap succeeds.
			return nil
		}
		return os.Rename(path, dst)
	})
	if err != nil {
		return fmt.Errorf("promote control-plane kit files: %w", err)
	}
	if dbSrc == "" {
		return errors.New("promote control-plane kit: staged kit has no data.db")
	}

	// Stage the key beside the kit and rename it in before data.db, so the pair
	// a crash could expose is (new key, new DB) or (old key, old DB) — both
	// consistent — never a new key beside the old DB.
	keyTmp := filepath.Join(staged, ".encryption_key")
	if err := os.WriteFile(keyTmp, snap.key, 0o600); err != nil {
		return fmt.Errorf("stage adopted encryption key: %w", err)
	}
	if err := os.Rename(keyTmp, k.KeyPath()); err != nil {
		return fmt.Errorf("adopt encryption key: %w", err)
	}
	if err := os.Chtimes(k.KeyPath(), snap.at, snap.at); err != nil {
		return fmt.Errorf("timestamp adopted encryption key: %w", err)
	}
	if err := os.Rename(dbSrc, k.DBPath()); err != nil {
		return fmt.Errorf("adopt data.db: %w", err)
	}
	if err := os.Chtimes(k.DBPath(), snap.at, snap.at); err != nil {
		return fmt.Errorf("timestamp adopted data.db: %w", err)
	}
	if err := k.writeRevision(snap.at); err != nil {
		return fmt.Errorf("record adopted control-plane revision: %w", err)
	}
	return nil
}
