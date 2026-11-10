package controllers

import (
	"bytes"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/ui/middleware"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/ui/store"
)

// The set/rotate form is operator work: it carries the App private key. These
// tests cover the two halves the daemon cannot see — what the page renders
// before an operator overwrites a key, and what the controller forwards (and
// refuses to forward).

// settingsAsOperator mounts the settings surface with an operator in the gin
// context, which is what Auth.Require does in the real router.
func settingsAsOperator(t *testing.T, role string) *ctrlHarness {
	t.Helper()
	h := newCtrlHarness(t)
	h.engine.Use(func(c *gin.Context) {
		middleware.WithUser(c, &store.User{ID: 1, Username: "op", PasswordSet: true, Role: role})
	})
	h.mountSettings()
	return h
}

// postGitHubApp submits the console form as multipart, the way the browser will.
func postGitHubApp(t *testing.T, h *ctrlHarness, fields map[string]string, fileName string, fileBody []byte, remoteAddr, proto string) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for k, v := range fields {
		if err := mw.WriteField(k, v); err != nil {
			t.Fatalf("write field %s: %v", k, err)
		}
	}
	if fileName != "" {
		fw, err := mw.CreateFormFile("private_key_file", fileName)
		if err != nil {
			t.Fatalf("create form file: %v", err)
		}
		if _, err := fw.Write(fileBody); err != nil {
			t.Fatalf("write form file: %v", err)
		}
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close multipart: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "http://x/web/settings/github/app", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	if remoteAddr != "" {
		req.RemoteAddr = remoteAddr
	}
	if proto != "" {
		req.Header.Set("X-Forwarded-Proto", proto)
	}
	rr := httptest.NewRecorder()
	h.engine.ServeHTTP(rr, req)
	return rr
}

func TestControllers_SettingsGitHubFormPrefillsCurrentRegistration(t *testing.T) {
	h := settingsAsOperator(t, store.RoleOperator)

	rr := h.get(t, "/web/settings")
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /web/settings = %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	// Rotating means editing one field, not retyping four.
	for _, want := range []string{
		`value="4242"`,                      // app id
		`value="7"`,                         // installation id
		`value="acme/app"`,                  // probe repo
		`value="acme/app:latest"`,           // probe package
		`action="/web/settings/github/app"`, //
		`enctype="multipart/form-data"`,     //
		`name="private_key_file"`,           // the upload input
		`name="private_key_pem"`,            // the paste input
		"Rotate key",                        // an app exists, so this rotates
	} {
		if !strings.Contains(body, want) {
			t.Errorf("settings page does not contain %q", want)
		}
	}
	if strings.Contains(body, `name="app_id" dir="ltr" value=""`) {
		t.Error("the app id field is not prefilled")
	}
}

func TestControllers_SettingsGitHubShowsFingerprintAndProbeAge(t *testing.T) {
	h := settingsAsOperator(t, store.RoleOperator)

	rr := h.get(t, "/web/settings")
	body := rr.Body.String()
	if !strings.Contains(body, "SHA256:11:22:33") {
		t.Error("the panel does not show the stored key's fingerprint")
	}
	if !strings.Contains(body, "Last probe") || !strings.Contains(body, "2 min ago") {
		t.Errorf("the panel does not show the last probe and its age: %s", body[:min(len(body), 400)])
	}
}

func TestControllers_SettingsGitHubViewerGetsNoForm(t *testing.T) {
	h := settingsAsOperator(t, store.RoleViewer)

	rr := h.get(t, "/web/settings")
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /web/settings = %d, want 200", rr.Code)
	}
	if body := rr.Body.String(); strings.Contains(body, "private_key_pem") {
		t.Error("a viewer was offered the private key form")
	}
}

func TestControllers_SettingsGitHubSetPastesKey(t *testing.T) {
	h := settingsAsOperator(t, store.RoleOperator)

	rr := postGitHubApp(t, h, map[string]string{
		"app_id":          "4242",
		"installation_id": "7",
		"probe_repo":      "acme/app",
		"probe_package":   "acme/app:latest",
		"private_key_pem": "-----BEGIN RSA PRIVATE KEY-----\nthrowaway\n-----END RSA PRIVATE KEY-----\n",
	}, "", nil, "127.0.0.1:5555", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("POST = %d, want 200; body: %s", rr.Code, rr.Body.String()[:min(len(rr.Body.String()), 300)])
	}

	body := rr.Body.String()
	for _, want := range []string{"SHA256:aa:bb:cc", "Saved", "previous key stopped working"} {
		if !strings.Contains(body, want) {
			t.Errorf("after a save the panel does not show %q", want)
		}
	}

	sent, _, posts := githubSetCalls.last()
	if posts != 1 {
		t.Fatalf("daemon received %d set calls, want 1", posts)
	}
	if got := sent["app_id"]; got != float64(4242) {
		t.Errorf("app_id forwarded = %v, want 4242", got)
	}
	if got, _ := sent["private_key_pem"].(string); !strings.Contains(got, "BEGIN RSA PRIVATE KEY") {
		t.Errorf("the pasted key was not forwarded: %v", sent["private_key_pem"])
	}
}

func TestControllers_SettingsGitHubSetPrefersTheUploadedFile(t *testing.T) {
	h := settingsAsOperator(t, store.RoleOperator)

	const uploaded = "-----BEGIN PRIVATE KEY-----\nfrom-file\n-----END PRIVATE KEY-----\n"
	rr := postGitHubApp(t, h, map[string]string{
		"app_id":          "4242",
		"installation_id": "7",
		"probe_repo":      "acme/app",
		"probe_package":   "",
		"private_key_pem": "pasted-but-ignored",
	}, "app.pem", []byte(uploaded), "127.0.0.1:5555", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("POST = %d, want 200; body: %s", rr.Code, rr.Body.String()[:min(len(rr.Body.String()), 300)])
	}
	sent, _, _ := githubSetCalls.last()
	if got, _ := sent["private_key_pem"].(string); got != uploaded {
		t.Errorf("forwarded key = %q, want the uploaded file", got)
	}
}

func TestControllers_SettingsGitHubSetInstallationOnlyKeepsStoredKey(t *testing.T) {
	h := settingsAsOperator(t, store.RoleOperator)

	rr := postGitHubApp(t, h, map[string]string{
		"app_id":          "4242",
		"installation_id": "8",
		"probe_repo":      "acme/app",
		"probe_package":   "acme/app:latest",
	}, "", nil, "127.0.0.1:5555", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("POST = %d, want 200; body: %s", rr.Code, rr.Body.String()[:min(len(rr.Body.String()), 300)])
	}
	sent, _, _ := githubSetCalls.last()
	if _, present := sent["private_key_pem"]; present {
		t.Error("an installation-only change forwarded a key field")
	}
	if got := sent["installation_id"]; got != float64(8) {
		t.Errorf("installation_id forwarded = %v, want 8", got)
	}
}

func TestControllers_SettingsGitHubSetRefusesOversizedUpload(t *testing.T) {
	h := settingsAsOperator(t, store.RoleOperator)

	big := bytes.Repeat([]byte("A"), maxGitHubKeyUpload+1024)
	rr := postGitHubApp(t, h, map[string]string{
		"app_id":          "4242",
		"installation_id": "7",
	}, "big.pem", big, "127.0.0.1:5555", "")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("POST with a %d-byte upload = %d, want 400", len(big), rr.Code)
	}
	if _, _, posts := githubSetCalls.last(); posts != 0 {
		t.Errorf("an oversized upload reached the daemon (%d calls)", posts)
	}
}

func TestControllers_SettingsGitHubSetRefusesCleartextBrowserHop(t *testing.T) {
	h := settingsAsOperator(t, store.RoleOperator)
	fields := map[string]string{"app_id": "4242", "installation_id": "7"}

	rr := postGitHubApp(t, h, fields, "", nil, "203.0.113.9:5555", "")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("POST from a cleartext non-loopback caller = %d, want 400", rr.Code)
	}
	if _, _, posts := githubSetCalls.last(); posts != 0 {
		t.Errorf("a private key was forwarded over cleartext (%d calls)", posts)
	}
	if body := rr.Body.String(); !strings.Contains(body, "cleartext") {
		t.Error("the refusal does not say why")
	}

	// The same submit over the edge's HTTPS hop is forwarded, and the daemon is
	// told the browser used TLS.
	rr = postGitHubApp(t, h, fields, "", nil, "203.0.113.9:5555", "https")
	if rr.Code != http.StatusOK {
		t.Fatalf("POST with X-Forwarded-Proto: https = %d, want 200", rr.Code)
	}
	if _, proto, _ := githubSetCalls.last(); proto != "https" {
		t.Errorf("forwarded X-Forwarded-Proto = %q, want https", proto)
	}
}

func TestControllers_SettingsGitHubSetRejectsANonNumericID(t *testing.T) {
	h := settingsAsOperator(t, store.RoleOperator)

	rr := postGitHubApp(t, h, map[string]string{
		"app_id":          "not-a-number",
		"installation_id": "7",
	}, "", nil, "127.0.0.1:5555", "")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("POST with a bad app id = %d, want 400", rr.Code)
	}
	if _, _, posts := githubSetCalls.last(); posts != 0 {
		t.Errorf("a malformed id reached the daemon (%d calls)", posts)
	}
}
