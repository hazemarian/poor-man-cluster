package cli

import (
	"fmt"
	"os"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/config"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
)

// The never-create rule, in one place.
//
// store.Open CREATES a database file when the path does not exist. That is right
// exactly once — `init`, and `join` on a node with no local database yet — and
// wrong everywhere else: an empty data.db on a node that was never initialised
// turns every later check into a plausible empty answer, because the "run
// pmcluster init" guard passes, doctor finds a healthy store, and a manager with
// no stacks looks exactly like a manager with no stacks.
//
// Before this file the rule was hand-rolled in six places with four different
// sentences, next to six raw store.Open calls that said nothing about intent, so a
// reader could not tell a deliberate create from an oversight. Both halves are one
// call now: requireInitialised states the refusal, openExistingStore opens without
// creating, and openOrCreateStore is the single named exception.
//
// requireInitialised and openExistingStore are separate on purpose. A command wants
// the refusal BEFORE it does any other work (init a logger, sweep logs, prompt),
// and the two statements are not always adjacent; a caller that already knows the
// file is there only wants the no-create guarantee.

// requireInitialised refuses when the data directory holds no database, appending
// the caller's own hint — the advice differs ("run `pmcluster init` first", "or
// pass --restore <archive>", "or `pmcluster join` on a joining node") even though
// the rule does not.
func requireInitialised(cfg *config.Config, hint string) error {
	if _, err := os.Stat(cfg.DBPath()); os.IsNotExist(err) {
		return fmt.Errorf("data directory not initialised at %s — %s", cfg.DataDir, hint)
	}
	return nil
}

// openExistingStore opens the control-plane database ONLY if it already exists. The
// snapshot mirror and the CLI's mirror-labelled reads both use it, so neither can
// seed the database the other refuses to create.
//
// A follower that would create one is a lie: an empty data.db is exactly what the
// uninitialised-node guard exists to catch, and creating it here would make the
// guard pass on a node that was never initialised. A follower mid-join simply
// retries next interval.
//
// A caller must also expect the one race this can lose — the migration runner is
// not concurrency-safe across processes (0001 is a plain CREATE TABLE), so an
// opener that starts while another process is mid-migration fails once and then
// finds the work already done.
func openExistingStore(dbPath string) (*store.Store, error) {
	if _, err := os.Stat(dbPath); err != nil {
		return nil, fmt.Errorf("no control-plane database at %s yet: %w", dbPath, err)
	}
	return store.Open(dbPath)
}

// openOrCreateStore is the ONLY place a missing database is created, and it names
// its two callers — `init` and `join` — in the comment because a third one is a
// decision, not a convenience.
func openOrCreateStore(cfg *config.Config) (*store.Store, error) {
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		return nil, fmt.Errorf("open store: %w", err)
	}
	return st, nil
}

// openStore loads config, refuses if the data dir isn't initialised, and opens the
// store. Returns config too — callers need EncryptionKeyPath().
func openStore() (*store.Store, *config.Config, error) {
	cfg, err := config.Load(configPath)
	if err != nil {
		return nil, nil, fmt.Errorf("load config: %w", err)
	}
	if err := requireInitialised(cfg, "run `pmcluster init` first"); err != nil {
		return nil, nil, err
	}
	st, err := openExistingStore(cfg.DBPath())
	if err != nil {
		return nil, nil, err
	}
	return st, cfg, nil
}
