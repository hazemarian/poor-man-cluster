package middleware

import (
	"net/http"
	"testing"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/ui/store"
)

// TestPermissionForMapsTheConsoleSurface pins the table that decides what each
// console route needs. It is the one place "who may do this" is written down, so a
// change here is a change of policy and should have to be made deliberately.
func TestPermissionForMapsTheConsoleSurface(t *testing.T) {
	cases := []struct {
		method, path, want string
	}{
		{http.MethodGet, "/web/", "console.view"},
		{http.MethodGet, "/web/stacks/demo", "console.view"},
		{http.MethodGet, "/web/services/aarma/web/logs", "console.view"},

		// Access management.
		{http.MethodGet, "/web/users", "users.manage"},
		{http.MethodGet, "/web/roles", "roles.manage"},
		{http.MethodPost, "/web/roles/add", "roles.manage"},
		{http.MethodGet, "/web/apikeys", "keys.manage"},

		// The settings page is readable; a RENDERED config is not, because it
		// embeds credentials. Saving settings is admin work; TLS/webhooks are
		// operator work, which is where they were before roles were data.
		{http.MethodGet, "/web/settings", "console.view"},
		{http.MethodPost, "/web/settings", "settings.write"},
		{http.MethodGet, "/web/settings/rendered/traefik-dynamic", "secrets.write"},
		{http.MethodGet, "/web/tls", "console.view"},
		{http.MethodPost, "/web/tls", "platform.configure"},
		{http.MethodPost, "/web/webhooks", "platform.configure"},

		// A shell runs commands with the stack's secrets mounted.
		{http.MethodGet, "/web/shells/abc/ws", "shells.open"},

		// Stack mutations, backups, deploy.
		{http.MethodPost, "/web/stacks/demo/sync", "stacks.operate"},
		{http.MethodPost, "/web/deploy", "stacks.deploy"},
		{http.MethodPost, "/web/backups/7/restore", "backups.manage"},
		{http.MethodGet, "/web/backups", "console.view"},
	}
	for _, c := range cases {
		if got := PermissionFor(c.method, c.path); got != c.want {
			t.Errorf("PermissionFor(%s %s) = %q, want %q", c.method, c.path, got, c.want)
		}
	}
}

// TestEveryCataloguePermissionIsMapped — a permission nobody can trigger is a
// checkbox that lies: the role editor would offer it and no route would read it.
func TestEveryCataloguePermissionIsMapped(t *testing.T) {
	mapped := map[string]bool{}
	for _, r := range routeRules {
		mapped[r.Perm] = true
	}
	// The defaults are reached without a rule.
	mapped["console.view"] = true
	mapped["stacks.operate"] = true

	for _, perm := range store.Permissions() {
		if !mapped[perm.Name] {
			t.Errorf("permission %q is in the catalogue but no route requires it", perm.Name)
		}
	}
}
