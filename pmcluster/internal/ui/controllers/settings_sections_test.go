package controllers

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/ui/middleware"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/ui/pmapi"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/ui/store"
)

// The settings page is six groups, and these tests hold the two promises that
// make the grouping worth anything: every group says what it needs before its
// fields, and a refusal lands on the group (and the field) the daemon named.
//
// The failures behind them were all real: a runner switch with no org, a node
// picker nobody could find, and a save that answered "The cluster settings could
// not be saved" with the reason behind a disclosure.

// settingsPageAs mounts the settings surface as a given role, with the cluster
// routes mounted too (they are the admin-only half of the same page).
func settingsPageAs(t *testing.T, role string) *ctrlHarness {
	t.Helper()
	h := newCtrlHarness(t)
	h.engine.Use(func(c *gin.Context) {
		middleware.WithUser(c, &store.User{ID: 1, Username: "op", PasswordSet: true, Role: role})
	})
	h.engine.GET("/web/settings", (Settings{Controller: h.ctrl}).Page)
	h.engine.GET("/web/settings/cluster", (Settings{Controller: h.ctrl}).ClusterSettingsPage)
	h.engine.POST("/web/settings/cluster", (Settings{Controller: h.ctrl}).ClusterSettingsSave)
	h.engine.POST("/web/settings", (Settings{Controller: h.ctrl}).Save)
	return h
}

// TestControllers_SettingsSectionsStateHeadingAndPurpose walks the page and
// checks each group renders its own heading, the one-line purpose under it, and
// its own id (the anchor a refusal links to).
func TestControllers_SettingsSectionsStateHeadingAndPurpose(t *testing.T) {
	h := settingsPageAs(t, store.RoleAdmin)
	rr := h.get(t, "/web/settings/cluster")
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /web/settings/cluster = %d, want 200", rr.Code)
	}
	body := rr.Body.String()

	for _, sect := range []struct{ id, heading, purpose string }{
		{"sect-connection", "Connection", "How this console reaches the daemon"},
		{"sect-github", "GitHub App", "installation tokens minted on demand"},
		{"sect-identity", "Identity and sign-in", "The domain the cluster answers on"},
		{"sect-runners", "Self-Hosted CI Runners", "one GitHub Actions runner container per selected worker node"},
		{"sect-backups", "Backups", "Needs a backup store"},
		{"sect-advanced", "Advanced", "Paths and levels that rarely change"},
	} {
		if !strings.Contains(body, `id="`+sect.id+`"`) {
			t.Errorf("the page has no section %q", sect.id)
		}
		if !strings.Contains(body, sect.heading) {
			t.Errorf("section %s has no heading %q", sect.id, sect.heading)
		}
		if !strings.Contains(body, sect.purpose) {
			t.Errorf("section %s has no purpose line containing %q", sect.id, sect.purpose)
		}
	}
}

// The switch is what an operator comes here to flip; every field below it only
// matters once it is on. Rendered the other way round, the org field looked
// like the entry point and the switch looked like a footnote.
func TestControllers_SettingsRunnerSwitchComesBeforeTheOrgField(t *testing.T) {
	h := settingsPageAs(t, store.RoleAdmin)
	body := h.get(t, "/web/settings/cluster").Body.String()

	on := strings.Index(body, `name="github_runners_enabled"`)
	org := strings.Index(body, `name="github_runner_org"`)
	switch {
	case on < 0:
		t.Fatal("the runners section has no enable switch")
	case org < 0:
		t.Fatal("the runners section has no org field")
	case on > org:
		t.Errorf("the enable switch is rendered after the org field (switch at %d, org at %d)", on, org)
	}
}

// Prerequisites are stated, not discovered. These are the three that failed.
func TestControllers_SettingsStatesItsPrerequisites(t *testing.T) {
	h := settingsPageAs(t, store.RoleAdmin)
	body := h.get(t, "/web/settings/cluster").Body.String()

	for _, want := range []string{
		// the runners section: the App, the org, and a selected worker.
		"Needs a configured GitHub App",
		"github_runner_org",
		"at least one worker selected on the Runners page",
		// the field-level condition, next to the field it constrains.
		"Required when runners are enabled",
		// the permission, by its GitHub path, and what it is NOT.
		"Organization → Self-hosted runners: Read &amp; write",
		"not Administration, which only covers repository-level endpoints",
		// and that a permission change needs a new token.
		"until the token is re-minted",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the page does not state %q", want)
		}
	}
}

// The node picker is one click from the section heading, and the section says
// where the choice is made — the operator who enabled runners and then could not
// find the picker is the reason.
func TestControllers_SettingsRunnersLinkTheNodePicker(t *testing.T) {
	h := settingsPageAs(t, store.RoleAdmin)
	body := h.get(t, "/web/settings/cluster").Body.String()

	if !strings.Contains(body, `href="/web/runners"`) {
		t.Fatal("the runners section does not link the Runners page")
	}
	if n := strings.Count(body, `href="/web/runners"`); n < 2 {
		t.Errorf("the Runners link appears %d time(s); it belongs at the section heading and in the summary", n)
	}
	if !strings.Contains(body, "Pick worker nodes") {
		t.Error("the link to the node picker does not say what it opens")
	}
	if !strings.Contains(body, "chosen on the Runners panel") {
		t.Error("the section does not say where the worker selection is made")
	}
}

// A daemon refusal is rendered as the daemon wrote it, in the banner body, and
// it marks the section and the field it names.
func TestControllers_SettingsRefusalNamesTheOrgField(t *testing.T) {
	prev := clusterSaveRefusal
	clusterSaveRefusal = "github_runners_enabled is enabled but github_runner_org is empty: " +
		"an org-level runner registers into an organisation, so set github_runner_org (for example acme) or turn the feature off"
	t.Cleanup(func() { clusterSaveRefusal = prev })

	h := settingsPageAs(t, store.RoleAdmin)
	rr := h.post(t, "/web/settings/cluster", "github_runners_enabled=true&github_runner_org=")
	if rr.Code != http.StatusOK {
		t.Fatalf("POST /web/settings/cluster = %d, want 200 (the page re-renders with the refusal)", rr.Code)
	}
	body := rr.Body.String()

	// The reason is the banner BODY — the b-raw paragraph, not the title, and
	// not behind a disclosure: that was the reported failure.
	at := strings.Index(body, `class="b-raw`)
	if at < 0 {
		t.Fatal("the refusal has no banner body to hold the daemon's message")
	}
	chunk := body[at:]
	if end := strings.Index(chunk, "</p>"); end > 0 {
		chunk = chunk[:end]
	}
	if !strings.Contains(chunk, "github_runner_org is empty") {
		t.Errorf("the daemon's message is not the banner body: %s", chunk)
	}
	if strings.Contains(body, "Show details") {
		t.Error("the reason is hidden behind a details disclosure")
	}

	// The section it names is marked…
	if !strings.Contains(body, `class="panel is-flagged" id="sect-runners"`) {
		t.Error("the CI runners section is not flagged")
	}
	// …and so is the field.
	if !strings.Contains(body, `id="cs-runner-org" class="finput mono is-invalid"`) &&
		!strings.Contains(body, `class="finput mono is-invalid"`) {
		t.Error("the org field is not marked invalid")
	}
	if !strings.Contains(body, `aria-invalid="true"`) {
		t.Error("the org field does not announce its invalid state")
	}
	if !strings.Contains(body, "github_runner_org is empty while runners are enabled") {
		t.Error("the field carries no sentence of its own")
	}
	// The refusal keeps the operator's own input: they submitted an empty org,
	// so the field comes back empty — not silently re-filled from the store,
	// which would look like the daemon had accepted a value it refused.
	fieldAt := strings.Index(body, `id="cs-runner-org"`)
	if fieldAt < 0 {
		t.Fatal("the org field is not on the page after a refusal")
	}
	org := body[fieldAt:]
	if end := strings.Index(org, ">"); end > 0 {
		if !strings.Contains(org[:end], `value=""`) {
			t.Errorf("the refused org field was not re-rendered with the submitted value: %s", org[:end])
		}
	}
}

// A refusal that names no setting but reads as a permission problem marks the
// GitHub App section and says what to change there.
func TestControllers_SettingsPermissionRefusalMarksTheAppSection(t *testing.T) {
	prev := clusterSaveRefusal
	clusterSaveRefusal = "the GitHub App installation token is not permitted to register a runner in this organization (403)"
	t.Cleanup(func() { clusterSaveRefusal = prev })

	h := settingsPageAs(t, store.RoleAdmin)
	body := h.post(t, "/web/settings/cluster", "github_runner_org=acme").Body.String()

	if !strings.Contains(body, `class="panel is-flagged" id="sect-github"`) {
		t.Error("a permission refusal does not mark the GitHub App section")
	}
	if !strings.Contains(body, "Self-hosted runners: Read") {
		t.Error("a permission refusal does not name the permission to grant")
	}
}

// The read-only summary: what is enabled, which workers are selected, and one
// pill per runner. It is a summary, not the picker — the picker stays on its own
// page, linked from the heading.
func TestControllers_SettingsShowsTheRunnersSummary(t *testing.T) {
	h := settingsPageAs(t, store.RoleAdmin)
	body := h.get(t, "/web/settings/cluster").Body.String()

	if !strings.Contains(body, "What Is Registered Now") {
		t.Fatal("the runners section has no summary")
	}
	if !strings.Contains(body, "worker-1, worker-2") {
		t.Error("the summary does not list the selected workers")
	}
	if !strings.Contains(body, `data-runner="pmc-worker-1-org"`) || !strings.Contains(body, `data-runner="pmc-worker-2-org"`) {
		t.Error("the summary has no per-runner pills")
	}
	// The summary is read-only: no node checkbox may appear on the settings page.
	if strings.Contains(body, `name="node"`) {
		t.Error("the settings page grew the node picker; it belongs on /web/runners")
	}
}

// A save that works names what changed, in a confirmation that can be dismissed,
// and leaves the bar clean.
func TestControllers_SettingsSaveNamesTheChangedSections(t *testing.T) {
	h := settingsPageAs(t, store.RoleAdmin)
	body := h.post(t, "/web/settings/cluster", "github_runner_org=acme-two&github_runner_nodes=worker-1").Body.String()

	if !strings.Contains(body, "Settings saved. Changed:") {
		t.Fatalf("a successful save says nothing about what changed")
	}
	if !strings.Contains(body, `<span class="pill good">Self-Hosted CI Runners</span>`) {
		t.Error("the confirmation does not name the CI runners section")
	}
	if !strings.Contains(body, `data-dismiss`) {
		t.Error("the confirmation cannot be dismissed")
	}
	if !strings.Contains(body, `data-save-state="clean"`) {
		t.Error("a successful save left the bar dirty")
	}
}

// The one save bar states both conditions in the markup (the script only flips
// between them) and says what a save covers.
func TestControllers_SettingsSaveBarReportsDirtyAndClean(t *testing.T) {
	h := settingsPageAs(t, store.RoleAdmin)
	body := h.get(t, "/web/settings/cluster").Body.String()

	for _, want := range []string{
		`data-savebar`,
		`data-save-form`,
		`data-save-state="clean"`,
		`data-state-clean`,
		`data-state-dirty`,
		"No unsaved changes",
		"Unsaved changes",
		"Saves Identity and sign-in, CI runners, Backups and Advanced",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the save bar does not carry %q", want)
		}
	}
}

// A viewer or an operator cannot save cluster settings, so they are not shown a
// form that would 403: the sections are absent, not disabled.
func TestControllers_SettingsHidesTheClusterFormFromNonAdmins(t *testing.T) {
	h := settingsPageAs(t, store.RoleOperator)
	rr := h.get(t, "/web/settings")
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /web/settings = %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	for _, unwanted := range []string{`name="github_runner_org"`, `data-savebar`, `id="sect-runners"`} {
		if strings.Contains(body, unwanted) {
			t.Errorf("an operator was shown %q, a form only an admin can save", unwanted)
		}
	}
}

// With no App stored, the page says so, still names the permission runners need,
// and does not pretend a saved runner configuration exists.
func TestControllers_SettingsRendersWithoutTheApp(t *testing.T) {
	githubAppUnconfigured.Store(true)
	t.Cleanup(func() { githubAppUnconfigured.Store(false) })

	h := settingsPageAs(t, store.RoleAdmin)
	body := h.get(t, "/web/settings/cluster").Body.String()

	if !strings.Contains(body, "No GitHub App is configured") {
		t.Error("the page does not say the App is missing")
	}
	if strings.Contains(body, "Rotate key") {
		t.Error("the page offers to rotate a key that does not exist")
	}
	if !strings.Contains(body, "Org → Self-hosted") && !strings.Contains(body, "Organization → Self-hosted runners") {
		t.Error("the runners section stopped naming the permission its prerequisite needs")
	}
	if strings.Contains(body, "Settings saved") {
		t.Error("an unconfigured App rendered a success message")
	}
}

// A daemon that does not answer is its own state: the sections that read the
// cluster say so, no save bar is offered, and nothing reads as success.
func TestControllers_SettingsDaemonUnreachable(t *testing.T) {
	h := settingsPageAs(t, store.RoleAdmin)
	// Port 1 on loopback: refused immediately, which is what "the daemon is
	// unreachable" looks like to the console.
	h.ctrl.API = pmapi.New("http://127.0.0.1:1", "pmc_test", 200*1e6)
	h.ctrl.EnvAPI = "http://127.0.0.1:1"

	rr := h.get(t, "/web/settings/cluster")
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /web/settings/cluster = %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "The cluster settings could not be read") {
		t.Error("an unreachable daemon is not stated")
	}
	if strings.Contains(body, `data-savebar`) {
		t.Error("the page offers a save bar with nothing read to save")
	}
	if strings.Contains(body, "Settings saved") {
		t.Error("an unreachable daemon rendered a success message")
	}
}

// The mapping itself: the messages the daemon actually sends, and the fallback
// that must not invent a target.
func TestClusterFailureTarget(t *testing.T) {
	for _, tc := range []struct {
		name           string
		raw            string
		section, field string
		permission     bool
	}{
		{
			name:    "org",
			raw:     "pmcluster API 400: github_runners_enabled is enabled but github_runner_org is empty: an org-level runner registers into an organisation",
			section: "runners", field: "cs-runner-org",
		},
		{
			name:    "nodes",
			raw:     "pmcluster API 400: github_runners_enabled is enabled but github_runner_nodes is empty: a runner is a local container on a worker",
			section: "runners", field: "cs-runner-nodes",
		},
		{
			name:    "retention",
			raw:     "pmcluster API 400: backup_retention_days must be a positive integer",
			section: "backups", field: "cs-retention-days",
		},
		{
			name:    "permission",
			raw:     "pmcluster API 403: the installation token lacks permission to register a runner",
			section: "github", permission: true,
		},
		{
			// A message that names a setting AND a permission keeps the field:
			// the field is the actionable anchor.
			name:    "setting wins over permission",
			raw:     "github_runner_org is empty: permission denied when listing org runners",
			section: "runners", field: "cs-runner-org",
		},
		{name: "unknown message maps nowhere", raw: "something else went wrong"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			section, field, _, permission := clusterFailureTarget(tc.raw)
			if section != tc.section || field != tc.field || permission != tc.permission {
				t.Fatalf("clusterFailureTarget(%q) = (%q, %q, permission=%v), want (%q, %q, %v)",
					tc.raw, section, field, permission, tc.section, tc.field, tc.permission)
			}
		})
	}
}

// The confirmation names sections that actually differ — and a save that changed
// nothing says exactly that, rather than reporting a default as an edit.
func TestChangedClusterSections(t *testing.T) {
	before := map[string]string{
		"domain":                 "example.test",
		"github_runner_org":      "acme",
		"github_runners_enabled": "true",
		"backup_retention_days":  "3",
	}
	// Nothing typed: every key is echoed back as the daemon holds it, and the
	// two defaulted flags that were absent from the snapshot stay at default.
	same := map[string]string{
		"domain":                     "example.test",
		"github_runner_org":          "acme",
		"github_runners_enabled":     "true",
		"backup_retention_days":      "3",
		"github_runner_socket_proxy": "true",
		"backup_enabled":             "true",
		"node_docker_restart":        "false",
	}
	if got := changedClusterSections(before, same); len(got) != 0 {
		t.Fatalf("a no-op save reported changes: %v", got)
	}

	edited := map[string]string{}
	for k, v := range same {
		edited[k] = v
	}
	edited["github_runner_org"] = "acme-two"
	edited["backup_retention_days"] = "7"
	got := changedClusterSections(before, edited)
	want := []string{"clustersettings.sect_runners", "clustersettings.sect_backup"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("changed sections = %v, want %v (page order, section heading keys)", got, want)
	}

	if got := changedClusterSections(nil, same); got != nil {
		t.Fatalf("with no snapshot to compare, changed = %v, want none", got)
	}
}

// The section a key belongs to is what a refusal and a confirmation both use, so
// it must not drift from the page: each key lands in exactly one section.
func TestClusterSectionOfCoversEveryKey(t *testing.T) {
	for _, k := range clusterSettingKeys {
		section := clusterSectionOf(k)
		if _, ok := clusterSectionTitles[section]; !ok {
			t.Errorf("setting %s maps to section %q, which has no heading", k, section)
		}
	}
	if section := clusterSectionOf("github_runner_nodes"); section != "runners" {
		t.Errorf("github_runner_nodes maps to %q, want runners", section)
	}
}

// The alias route renders the same page, so a bookmark or a link from the Runners
// empty state lands on the grouped layout rather than the old wall of panels.
func TestControllers_ClusterSettingsAliasRendersTheSamePage(t *testing.T) {
	h := settingsPageAs(t, store.RoleAdmin)
	alias := h.get(t, "/web/settings/cluster").Body.String()
	page := h.get(t, "/web/settings").Body.String()
	for _, want := range []string{`id="sect-identity"`, `id="sect-runners"`, `data-savebar`} {
		if !strings.Contains(alias, want) || !strings.Contains(page, want) {
			t.Errorf("%q is not on both settings routes", want)
		}
	}
}

// An httptest recorder check that the page is a fragment (no shell) when htmx
// asks for it: the shell would nest a document inside #view.
func TestControllers_SettingsFragmentIsNotAShell(t *testing.T) {
	h := settingsPageAs(t, store.RoleAdmin)
	req := httptest.NewRequest(http.MethodGet, "http://x/web/settings/cluster", nil)
	req.Header.Set("HX-Request", "true")
	rr := httptest.NewRecorder()
	h.engine.ServeHTTP(rr, req)
	if body := rr.Body.String(); strings.Contains(body, "<!doctype html>") {
		t.Error("an htmx request was answered with a full document")
	}
}

// The Arabic page renders all six groups with Arabic headings and no unresolved
// key: a translated string that is present in the dictionary but not wired to the
// template would look identical to a missing one here.
func TestControllers_SettingsRendersInArabic(t *testing.T) {
	h := settingsPageAs(t, store.RoleAdmin)
	req := httptest.NewRequest(http.MethodGet, "http://x/web/settings/cluster?lang=ar", nil)
	rr := httptest.NewRecorder()
	h.engine.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /web/settings/cluster?lang=ar = %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, `dir="rtl"`) {
		t.Error("the Arabic page is not RTL")
	}
	for _, want := range []string{
		"الهوية والدخول",        // Identity and sign-in
		"مشغّلات CI",            // CI runners
		"لا تغييرات غير محفوظة", // the clean label on the save bar
		"اختيار العقد العاملة",  // Pick worker nodes
		"Read &amp; write",      // the permission, kept Latin and isolated
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the Arabic page is missing %q", want)
		}
	}
	for _, prefix := range []string{">clustersettings.", ">settings.sec_", ">settings.bar_", ">settings.adv_"} {
		if i := strings.Index(body, prefix); i >= 0 {
			t.Errorf("an unresolved translation key reached the Arabic page: %s", body[i+1:i+50])
		}
	}
}

// TestNodeDockerRestartIsAConsoleSwitch pins the watchdog switch as part of the
// console form. It was CLI-only for a release: the daemon allowlisted the key,
// the service validated it, and nothing in the UI carried it, so an operator had
// to know the key name to arm the only mechanism that recovers a node whose
// docker died.
func TestNodeDockerRestartIsAConsoleSwitch(t *testing.T) {
	const key = "node_docker_restart"
	found := false
	for _, k := range clusterSettingKeys {
		if k == key {
			found = true
		}
	}
	if !found {
		t.Fatalf("%s is not in the console form; the daemon would accept it and the operator could not set it", key)
	}
	if !clusterBoolSettings[key] {
		t.Errorf("%s must be a flag: a cleared checkbox sends nothing and the form would store \"\"", key)
	}
	if got := clusterSectionOf(key); got != "advanced" {
		t.Errorf("section = %q, want advanced — it sits with the other node-level knobs", got)
	}
	// A cleared checkbox and an absent row must read the same, or every save
	// reports the advanced section as changed.
	if got := clusterSettingDefault(key); got != "false" {
		t.Errorf("default = %q, want false", got)
	}
}

// TestControllers_DockerRestartSwitchRendersInAdvanced reads the page an operator
// actually sees. The unit assertions above pin the key lists; this one catches the
// template half, which is the half that was missing for a release.
func TestControllers_DockerRestartSwitchRendersInAdvanced(t *testing.T) {
	h := settingsPageAs(t, store.RoleAdmin)
	rr := h.get(t, "/web/settings/cluster")
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /web/settings/cluster = %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, `name="node_docker_restart"`) {
		t.Fatal("the Advanced section renders no node_docker_restart input")
	}
	if !strings.Contains(body, "Restart docker automatically") {
		t.Error("the switch has no label")
	}
	// The hint is the only place the operator is told what the cure costs, so it
	// has to be on the page next to the switch.
	if !strings.Contains(body, "three failed probes") {
		t.Error("the switch renders without its consequence")
	}
	if !strings.Contains(body, `id="sect-advanced"`) {
		t.Fatal("no advanced section")
	}
}
