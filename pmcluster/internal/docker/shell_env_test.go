package docker

import (
	"strings"
	"testing"
)

// TestShellEnv_HasNoDefaultDatabaseHost pins a decision, not an implementation detail: the shell used
// to export PGHOST=<stack>_db, which assumed the database service was called `db` and silently
// pointed psql at whatever answered under that name. There is no default now.
//
// With no PGHOST, psql has no host and no local socket, so it fails immediately and says so. That is
// the correct answer to "which database?": the operator names it — `psql -h aarma_db`.
func TestShellEnv_HasNoDefaultDatabaseHost(t *testing.T) {
	for _, kv := range ShellEnv() {
		upper := strings.ToUpper(kv)
		for _, banned := range []string{"PGHOST=", "PGDATABASE=", "PGUSER=", "PGPASSWORD="} {
			if strings.HasPrefix(upper, banned) {
				t.Errorf("ShellEnv() exports %s — a guessed default connects an operator to the "+
					"wrong database quietly, which is worse than failing to connect at all: %q", banned, kv)
			}
		}
	}
}

// TestShellEnv_KeepsWhatTheClientsNeed — dropping the guessed defaults must not drop the settings that
// make the clients usable in an interactive session.
func TestShellEnv_KeepsWhatTheClientsNeed(t *testing.T) {
	env := ShellEnv()
	joined := strings.Join(env, "\n")
	if !strings.Contains(joined, "HOME=/root") {
		t.Errorf("no HOME: psql's history and libpq defaults degrade. env=%v", env)
	}
	if !strings.Contains(joined, "PS1=") {
		t.Errorf("no PS1: the prompt shows nothing about where the operator is. env=%v", env)
	}
}
