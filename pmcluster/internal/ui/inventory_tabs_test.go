package ui

import (
	"net/http"
	"strings"
	"testing"
)

// inventoryHarness logs an admin in against the shared fake daemon and returns
// a getter that renders a path through the real router and renderer.
func inventoryHarness(t *testing.T) (*App, func(path string) string) {
	t.Helper()
	daemon := fakeDaemon(t)
	t.Cleanup(daemon.Close)
	app := newTestApp(t, daemon)
	jar := map[string]*http.Cookie{}
	doRequest(t, app, http.MethodPost, "/web/setup", "username=admin&password=supersecret&confirm=supersecret", jar)
	doRequest(t, app, http.MethodPost, "/web/login", "username=admin&password=supersecret", jar)

	get := func(path string) string {
		t.Helper()
		resp := doRequest(t, app, http.MethodGet, path, "", jar)
		b := readBody(t, resp)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s = %d, want 200; body: %s", path, resp.StatusCode, b)
		}
		return b
	}
	return app, get
}

// TestInventoryTabsSeparateScopes proves the two tabs never render each
// other's rows, and that the tab rides in the query: ?tab=platform is a deep
// link to the platform rows, while the default is Stacks.
func TestInventoryTabsSeparateScopes(t *testing.T) {
	_, get := inventoryHarness(t)

	stacks := get("/web/inventory")
	for _, want := range []string{
		"Stacks", "Platform",
		"app_env", "demo_app_config", "demo_db_pass", // service-scope fixtures
		`hx-get="/web/settings/configs/for-stack"`, // create → service-scope form
		"2 configs", "1 secret", // counts from the fixtures
	} {
		if !strings.Contains(stacks, want) {
			t.Errorf("Stacks tab missing %q", want)
		}
	}
	for _, leaked := range []string{"traefik-dynamic", "site_cert", `hx-get="/web/settings/configs/new"`} {
		if strings.Contains(stacks, leaked) {
			t.Errorf("Stacks tab rendered the platform row/target %q", leaked)
		}
	}

	platform := get("/web/inventory?tab=platform")
	for _, want := range []string{
		"traefik-dynamic", "site_cert",
		`hx-get="/web/settings/configs/new"`,              // create → cluster-scope form
		`hx-get="/web/settings/rendered/traefik-dynamic"`, // rendered snapshot affordance
		"1 config", "1 secret",
	} {
		if !strings.Contains(platform, want) {
			t.Errorf("Platform tab missing %q", want)
		}
	}
	for _, leaked := range []string{"app_env", "demo_app_config", "demo_db_pass"} {
		if strings.Contains(platform, leaked) {
			t.Errorf("Platform tab rendered the stack row %q", leaked)
		}
	}
}

// TestInventorySearchFiltersEachTab drives the server-side q filter in both
// tabs. The same query must produce each tab's own count, prove the count is
// shown as "n of total", and leave no table at all when nothing matches.
func TestInventorySearchFiltersEachTab(t *testing.T) {
	_, get := inventoryHarness(t)

	// "env" matches only app_env among the two stack configs.
	stacks := get("/web/inventory?tab=stacks&q=env")
	for _, want := range []string{"app_env", "1 of 2", "0 of 1"} {
		if !strings.Contains(stacks, want) {
			t.Errorf("filtered Stacks tab missing %q", want)
		}
	}
	if strings.Contains(stacks, `hx-get="/web/inventory/retag/config/demo_app_config`) {
		t.Error("filtered Stacks tab still lists demo_app_config")
	}

	// The same query on Platform matches nothing and must say so, not render
	// an empty table.
	platform := get("/web/inventory?tab=platform&q=env")
	for _, want := range []string{"No matches for", "env", "0 of 1"} {
		if !strings.Contains(platform, want) {
			t.Errorf("unmatched Platform search missing %q", want)
		}
	}
	if strings.Contains(platform, "<table") {
		t.Error("unmatched search rendered a table instead of the empty state")
	}

	// And a platform-only term finds its row on Platform but not on Stacks.
	platform = get("/web/inventory?tab=platform&q=traefik")
	for _, want := range []string{"traefik-dynamic", "1 of 1"} {
		if !strings.Contains(platform, want) {
			t.Errorf("filtered Platform tab missing %q", want)
		}
	}
	stacks = get("/web/inventory?tab=stacks&q=traefik")
	if !strings.Contains(stacks, "No matches for") {
		t.Error("Stacks search for a platform-only name did not render the empty state")
	}
}

// TestInventoryTabAndQuerySurviveTogether checks the two pieces of state the
// page carries: the active tab is in a hidden field of the search form and in
// both tab links, so neither a debounced search nor a tab switch drops the
// other. It also checks the tab survives a full HTMX-style refresh URL.
func TestInventoryTabAndQuerySurviveTogether(t *testing.T) {
	_, get := inventoryHarness(t)

	body := get("/web/inventory?tab=platform&q=traefik")
	for _, want := range []string{
		`<input type="hidden" name="tab" value="platform">`,
		`hx-get="/web/inventory?tab=stacks&amp;q=traefik"`,
		`hx-get="/web/inventory?tab=platform&amp;q=traefik"`,
		`role="tab" class="tab is-active"`,
		`aria-selected="true"`,
		`name="q" value="traefik"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("tab+query state missing %q", want)
		}
	}
}

// TestStackConfigPageShowsOnlyServiceScope is the other direction of the
// scope split: the fake daemon ignores the scope query and answers every call
// with all rows, so a cluster-scope config or secret appearing here means the
// controller merged scopes.
func TestStackConfigPageShowsOnlyServiceScope(t *testing.T) {
	_, get := inventoryHarness(t)

	body := get("/web/stacks/demo/config")
	for _, want := range []string{"app_env", "demo_app_config", "demo_db_pass"} {
		if !strings.Contains(body, want) {
			t.Errorf("stack config page missing its service-scope row %q", want)
		}
	}
	for _, leaked := range []string{"traefik-dynamic", "site_cert"} {
		if strings.Contains(body, leaked) {
			t.Errorf("stack config page rendered the cluster-scope row %q", leaked)
		}
	}
}

// TestSettingsLinksInventoryAndKeepsForms covers the settings half of the
// change: the cluster-scope lists are gone (the Platform tab owns them), the
// page links there, and the create/edit form routes still render their forms.
func TestSettingsLinksInventoryAndKeepsForms(t *testing.T) {
	_, get := inventoryHarness(t)

	body := get("/web/settings")
	if !strings.Contains(body, `href="/web/inventory?tab=platform"`) {
		t.Error("settings page has no link to Inventory → Platform")
	}
	for _, leaked := range []string{"site_cert", "traefik-dynamic", "demo_db_pass"} {
		if strings.Contains(body, leaked) {
			t.Errorf("settings page still lists the cluster-scope row %q", leaked)
		}
	}

	// The create/edit forms the Platform tab targets keep working.
	forms := map[string][]string{
		"/web/settings/configs/new":             {"Add Config", "/web/settings/configs/add"},
		"/web/settings/secrets/new":             {"Add Secret", "/web/settings/secrets/add"},
		"/web/settings/configs/edit/nginx_conf": {`Edit config`, "/web/settings/configs/edit"},
		"/web/settings/secrets/edit/db_pass":    {`Edit secret`, "/web/settings/secrets/edit"},
	}
	for path, wants := range forms {
		b := get(path)
		for _, want := range wants {
			if !strings.Contains(b, want) {
				t.Errorf("GET %s missing %q", path, want)
			}
		}
	}
}

// TestInventoryActionsStayOnTab covers the row actions the page keeps: retag
// carries the tab through the modal, and delete re-renders the same tab with a
// result banner instead of jumping to another page.
func TestInventoryActionsStayOnTab(t *testing.T) {
	app, _ := inventoryHarness(t)
	jar := map[string]*http.Cookie{}
	doRequest(t, app, http.MethodPost, "/web/login", "username=admin&password=supersecret", jar)

	// The retag modal carries the originating tab.
	resp := doRequest(t, app, http.MethodGet, "/web/inventory/retag/config/app_env?scope=service&tab=platform", "", jar)
	modal := readBody(t, resp)
	if !strings.Contains(modal, `<input type="hidden" name="tab" value="platform">`) {
		t.Errorf("retag form does not carry the tab: %s", modal)
	}

	// A successful retag returns to that tab.
	resp = doRequest(t, app, http.MethodPost, "/web/inventory/retag",
		"kind=config&name=app_env&scope=service&stack=demo&tab=platform", jar)
	retagged := readBody(t, resp)
	if !strings.Contains(retagged, `<input type="hidden" name="tab" value="platform">`) {
		t.Error("successful retag did not return to the Platform tab")
	}

	// Deleting keeps the operator on the same tab with a confirmation.
	resp = doRequest(t, app, http.MethodPost, "/web/inventory/remove/config/app_env",
		"tab=platform&q=traefik", jar)
	deleted := readBody(t, resp)
	for _, want := range []string{"Config app_env deleted.", `<input type="hidden" name="tab" value="platform">`, `name="q" value="traefik"`} {
		if !strings.Contains(deleted, want) {
			t.Errorf("inventory delete missing %q", want)
		}
	}
}
