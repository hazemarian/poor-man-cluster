package controllers

import (
	"net/http"
	"strings"
	"testing"
)

// githubRunnersFixture is the /api/github/runners body the controller tests
// serve. The default is one running runner and one stopped one on the second
// selected worker, so a single render exercises both rows and both kill paths.
var githubRunnersFixture = `{"enabled":true,"runners":[
 {"name":"pmc-worker-1-org","org":"acme","node":"worker-1","container_id":"abc123","mode":"ephemeral",
  "state":"idle","registered_at":1700000000,"last_seen":1700000600,"last_job_url":"https://github.com/acme/api/actions/runs/42"},
 {"name":"pmc-worker-2-org","org":"acme","node":"worker-2","container_id":"","mode":"ephemeral",
  "state":"offline","registered_at":0,"last_seen":1700000600,"last_job_url":""}]}`

// clusterSettingsFixture is the /api/cluster/settings body: the runner settings
// plus the selection the picker has to render as checked.
var clusterSettingsFixture = `{"settings":{
 "github_runner_org":"acme","github_runner_group":"pmcluster","github_runner_nodes":"worker-1,worker-2",
 "github_runner_memory":"2560m","github_runner_socket_proxy":"true"}}`

func (h *ctrlHarness) mountRunners() {
	h.engine.GET("/web/runners", (Runners{Controller: h.ctrl}).Page)
	h.engine.POST("/web/runners/nodes", (Runners{Controller: h.ctrl}).SaveNodes)
	h.engine.POST("/web/runners/restart-all", (Runners{Controller: h.ctrl}).RestartAll)
	h.engine.POST("/web/runners/:name/kill", (Runners{Controller: h.ctrl}).Kill)
	h.engine.POST("/web/runners/:name/restart", (Runners{Controller: h.ctrl}).Restart)
}

func TestControllers_RunnersPanel(t *testing.T) {
	h := newCtrlHarness(t)
	h.mountRunners()
	rr := h.get(t, "/web/runners")
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /web/runners = %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	for _, want := range []string{
		"pmc-worker-1-org",                   // the row's name
		"acme",                               // the scope column
		">idle<",                             // the state pill, translated
		">offline<",                          // the second row's state
		"/web/runners/pmc-worker-1-org/kill", // the kill affordance
		"acme/api#42",                        // the last-job label
		"worker-2",                           // the second selected node
	} {
		if !strings.Contains(body, want) {
			t.Errorf("panel does not contain %q", want)
		}
	}
}

// A busy runner's kill is the one action that fails a job: the row must carry the
// confirmation, and it must say so in words.
func TestControllers_RunnersBusyKillNeedsConfirmation(t *testing.T) {
	h := newCtrlHarness(t)
	h.mountRunners()
	prev := githubRunnersFixture
	githubRunnersFixture = strings.Replace(githubRunnersFixture, `"state":"idle"`, `"state":"busy"`, 1)
	t.Cleanup(func() { githubRunnersFixture = prev })

	body := h.get(t, "/web/runners").Body.String()
	if !strings.Contains(body, "hx-confirm=") {
		t.Fatal("a busy runner's kill has no confirmation")
	}
	if !strings.Contains(body, "FAILS that job") {
		t.Fatalf("the confirmation does not state the consequence: %s", firstLine(body))
	}
}

func TestControllers_RunnersDisabledEmptyState(t *testing.T) {
	h := newCtrlHarness(t)
	h.mountRunners()
	prev := githubRunnersFixture
	githubRunnersFixture = `{"enabled":false,"runners":[]}`
	t.Cleanup(func() { githubRunnersFixture = prev })

	body := h.get(t, "/web/runners").Body.String()
	if !strings.Contains(body, "github_runners_enabled is false") {
		t.Fatalf("the disabled state does not explain the setting: %s", firstLine(body))
	}
	if !strings.Contains(body, "/web/settings/cluster") {
		t.Fatal("the disabled state does not point at the setting that turns it on")
	}
}

// Managers are shown, disabled, with the reason — hiding them turns "why is my
// manager not in the list" into a support question.
func TestControllers_RunnersNodePickerDisablesManagers(t *testing.T) {
	h := newCtrlHarness(t)
	h.mountRunners()
	body := h.get(t, "/web/runners").Body.String()
	if !strings.Contains(body, "manager-1") {
		t.Fatal("the picker hides the manager node instead of disabling it")
	}
	if !strings.Contains(body, "marks the node Down") {
		t.Fatal("the picker does not say why a manager cannot run a runner")
	}
	if !strings.Contains(body, `value="worker-1" checked`) {
		t.Fatal("the stored selection is not checked")
	}
	if strings.Contains(body, `value="manager-1" checked`) || strings.Contains(body, `name="node" value="manager-1"`) {
		t.Fatal("a manager is selectable in the picker")
	}
}

// A forged save that includes a manager is refused by the controller, so the
// setting never sees a hostname the picker would not offer.
func TestControllers_RunnersSaveRefusesAManager(t *testing.T) {
	h := newCtrlHarness(t)
	h.mountRunners()
	rr := h.post(t, "/web/runners/nodes", "node=worker-1&node=manager-1")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "cannot run a runner") {
		t.Fatalf("the refusal is not rendered: %s", firstLine(body))
	}
}

func TestControllers_RunnersKillReportsTheOutcome(t *testing.T) {
	h := newCtrlHarness(t)
	h.mountRunners()
	rr := h.post(t, "/web/runners/pmc-worker-1-org/kill", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	if body := rr.Body.String(); !strings.Contains(body, "Killed runner pmc-worker-1-org") {
		t.Fatalf("the kill outcome is not reported: %s", firstLine(body))
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
