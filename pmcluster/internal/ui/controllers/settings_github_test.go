package controllers

import (
	"net/http"
	"strings"
	"testing"
)

// The GitHub App card is fed by the daemon, never by the console's own store:
// the key lives there and must not be copied here. This exercises the read path
// (ids + granted permissions) and the Test button's answer.
func TestControllers_SettingsGitHubAppCard(t *testing.T) {
	h := newCtrlHarness(t)
	h.mountSettings()

	rr := h.get(t, "/web/settings")
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /web/settings = %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	for _, want := range []string{"GitHub App", "4242", "acme/app", "packages"} {
		if !strings.Contains(body, want) {
			t.Errorf("settings page does not show %q", want)
		}
	}
	// The private key is not part of the daemon's response, and the card must
	// not invent placeholder material that looks like it.
	if strings.Contains(body, "PRIVATE KEY") || strings.Contains(body, "BEGIN") {
		t.Error("the settings page rendered key material")
	}
}

func TestControllers_SettingsGitHubAppTest(t *testing.T) {
	h := newCtrlHarness(t)
	h.mountSettings()

	rr := h.post(t, "/web/settings/github/test", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("POST /web/settings/github/test = %d, want 200", rr.Code)
	}
	if body := rr.Body.String(); !strings.Contains(body, "read acme/app OK") {
		t.Errorf("the daemon's test sentence is not rendered: %s", body[:min(len(body), 300)])
	}
}
