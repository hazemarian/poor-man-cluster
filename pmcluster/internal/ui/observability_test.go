package ui

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/gin-gonic/gin"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/auth"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/investigation"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/observability"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/server"
	dbstore "github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/ui/store"
)

type evidenceFixture struct{ calls atomic.Int32 }

func (f *evidenceFixture) Query(_ context.Context, q observability.Query) (observability.Result, error) {
	f.calls.Add(1)
	return observability.Result{Status: observability.StatusAvailable, Window: q.Window, ObservedAt: time.Now(), Rows: []observability.Row{{"service.name": "demo_web", "body": `<script>alert(1)</script> password="test-secret" bare-credential`, "severity_text": "ERROR"}}}, nil
}

func TestNativeObservabilityScopeAndRendering(t *testing.T) {
	for _, tc := range []struct {
		name          string
		me, inventory int
		service       string
		wantCalls     int32
	}{
		{"available", 200, 200, "demo/web", 1}, {"scoped", 403, 200, "", 0},
		{"inventory-failed", 200, 503, "", 0}, {"unknown-service", 200, 200, "other/web", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/me":
					w.WriteHeader(tc.me)
					fmt.Fprint(w, `{"id":1,"name":"edge"}`)
				case "/api/services":
					w.WriteHeader(tc.inventory)
					fmt.Fprint(w, `{"services":[{"name":"demo_web","stack":"demo","replicas":1,"desired":1}]}`)
				case "/api/nodes":
					fmt.Fprint(w, `{"nodes":[]}`)
				case "/api/backups":
					fmt.Fprint(w, `{"backups":[]}`)
				case "/api/tls/hosts":
					fmt.Fprint(w, `{"hosts":[]}`)
				default:
					fmt.Fprint(w, `{}`)
				}
			}))
			defer daemon.Close()
			app, err := NewApp(Config{DataDir: ":memory:", PMAPIURL: daemon.URL, PMAPIToken: "bare-credential", LoginDisabled: true, UpstreamTimeout: time.Second})
			if err != nil {
				t.Fatal(err)
			}
			defer app.Store.Close()
			source := &evidenceFixture{}
			app.ctrl.Telemetry = source
			req := httptest.NewRequest("GET", "http://x/web/observability?service="+tc.service, nil)
			w := httptest.NewRecorder()
			app.Handler().ServeHTTP(w, req)
			body := w.Body.String()
			if source.calls.Load() != tc.wantCalls {
				t.Fatalf("unauthorized source calls: %d", source.calls.Load())
			}
			if strings.Contains(body, "test-secret") || strings.Contains(body, "bare-credential") || strings.Contains(body, "<script>alert(1)</script>") {
				t.Fatal("unsafe evidence rendered")
			}
			if !strings.Contains(body, "<!doctype html>") || !strings.Contains(body, "Observability") {
				t.Fatalf("native shell missing: status%d", w.Code)
			}
		})
	}
}

func TestNativeObservabilityRequiresLogin(t *testing.T) {
	app, err := NewApp(Config{DataDir: ":memory:", CookieName:"pmui_session", EnvUser:"admin", EnvPass:"fixture-password", SessionSecret: []byte("0123456789abcdef")})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Store.Close()
	w := httptest.NewRecorder()
	app.Handler().ServeHTTP(w, httptest.NewRequest("GET", "http://x/web/observability", nil))
	_, _ = io.Copy(io.Discard, w.Result().Body)
	if w.Code < 300 || w.Code >= 400 {
		t.Fatalf("unauthenticated status=%d", w.Code)
	}
}

type investigationAdminLookup struct{}

func (investigationAdminLookup) UserByToken(context.Context, string) (*auth.User, error) {
	return &auth.User{ID: 1, Name: "edge", Role: auth.RoleAdmin}, nil
}

func TestNativeInvestigationEndToEndWithFixtures(t *testing.T) {
	var calls atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var request struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		if json.NewDecoder(r.Body).Decode(&request) != nil {
			t.Error("provider request")
		}
		var in investigation.Input
		if len(request.Messages) != 2 || json.Unmarshal([]byte(request.Messages[1].Content), &in) != nil || len(in.Evidence) == 0 {
			t.Error("invalid evidence input")
			w.WriteHeader(400)
			return
		}
		if strings.Contains(request.Messages[1].Content, "test-secret") || strings.Contains(request.Messages[1].Content, "bare-credential") {
			t.Error("secret reached provider")
		}
		verdict := investigation.Verdict{Summary: "<script>alert(1)</script> Observed failures", Evidence: []string{in.Evidence[0].ID}}
		content, _ := json.Marshal(verdict)
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"content": string(content)}, "finish_reason": "stop"}}, "usage": map[string]int{"total_tokens": 64}})
	}))
	defer provider.Close()
	st, err := dbstore.Open(filepath.Join(t.TempDir(), "daemon.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	local := &investigation.Local{Store: st}
	p := investigation.DefaultPolicy()
	p.Enabled = true
	p.Sharing = true
	p.Model = "fixture"
	p.BaseURL = provider.URL + "/v1"
	p.DailyTokens = 18000
	if err = local.SetPolicy(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	accounting := server.New(server.Deps{Lookup: investigationAdminLookup{}, Investigation: local})
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/investigation/") {
			accounting.ServeHTTP(w, r)
			return
		}
		switch r.URL.Path {
		case "/api/me":
			fmt.Fprint(w, `{"id":1,"name":"edge"}`)
		case "/api/services":
			fmt.Fprint(w, `{"services":[{"name":"demo_web","stack":"demo","replicas":1,"desired":1}]}`)
		case "/api/nodes":
			fmt.Fprint(w, `{"nodes":[]}`)
		case "/api/backups":
			fmt.Fprint(w, `{"backups":[]}`)
		case "/api/tls/hosts":
			fmt.Fprint(w, `{"hosts":[]}`)
		default:
			fmt.Fprint(w, `{}`)
		}
	}))
	defer daemon.Close()
	app, err := NewApp(Config{DataDir: ":memory:", LoginDisabled: true, PMAPIURL: daemon.URL, PMAPIToken: "bare-credential", InvestigationKey: "fixture-key"})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	app.ctrl.Telemetry = &evidenceFixture{}
	form := url.Values{"question": {"Why password=test-secret?"}, "service": {"demo/web"}, "range": {"1h"}}.Encode()
	req := httptest.NewRequest("POST", "http://x/web/observability/investigations", strings.NewReader(form))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "http://x")
	req.Header.Set("HX-Request", "true")
	w := httptest.NewRecorder()
	app.Handler().ServeHTTP(w, req)
	match := regexp.MustCompile(`/web/observability/investigations/([a-f0-9]{32})`).FindStringSubmatch(w.Body.String())
	if len(match) != 2 {
		t.Fatalf("job not queued: %s", w.Body.String())
	}
	var result string
	for i := 0; i < 5; i++ {
		poll := httptest.NewRequest("GET", "http://x/web/observability/investigations/"+match[1], nil)
		poll.Header.Set("HX-Request", "true")
		out := httptest.NewRecorder()
		app.Handler().ServeHTTP(out, poll)
		result = out.Body.String()
		if strings.Contains(result, "Investigation complete") {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(result, "Investigation complete") || strings.Contains(result, "<script>alert(1)</script>") || !strings.Contains(result, "&lt;script&gt;") {
		t.Fatalf("unsafe or incomplete result: %s", result)
	}
	if calls.Load() != 1 {
		t.Fatalf("provider calls=%d", calls.Load())
	}
	b, _ := st.InvestigationBudget(context.Background())
	if b.Charged != 64 || b.Held != 0 {
		t.Fatalf("budget=%+v", b)
	}
}

func TestInvestigationOriginAndRoleGates(t *testing.T) {
	app, err := NewApp(Config{DataDir: ":memory:", LoginDisabled: true})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	for _, origin := range []string{"", "null", "http://evil.example", "https://x", "http://user@x"} {
		req := httptest.NewRequest("POST", "http://x/web/observability/investigations", strings.NewReader("question=why"))
		req.Header.Set("Origin", origin)
		w := httptest.NewRecorder()
		app.Handler().ServeHTTP(w, req)
		if w.Code != 403 {
			t.Fatalf("origin %q status%d", origin, w.Code)
		}
	}
	local, err := NewApp(Config{DataDir: ":memory:", CookieName:"pmui_session", EnvUser:"admin", EnvPass:"fixture-password", SessionSecret: []byte("0123456789abcdef")})
	if err != nil {
		t.Fatal(err)
	}
	defer local.Close()
	if _, err = local.Store.CreateUser(context.Background(), "viewer", "hash", true, store.RoleViewer); err != nil {
		t.Fatal(err)
	}
	jar := map[string]*http.Cookie{}
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest("GET", "http://x/", nil)
	local.Auth.SetCookie(ctx, "viewer", time.Hour)
	for _, cookie := range recorder.Result().Cookies() {
		jar[cookie.Name] = cookie
	}
	for _, path := range []string{"/web/observability/investigations", "/web/observability/settings"} {
		resp := doRequest(t, local, "POST", path, "question=why", jar)
		if resp.StatusCode != 403 {
			t.Fatalf("viewer path%s status%d", path, resp.StatusCode)
		}
	}
}

func TestObservabilityExpiredCertificateIsExplicit(t *testing.T) {
	for _, expired := range []bool{true, false} {
		t.Run(fmt.Sprint(expired), func(t *testing.T) {
			expiry := time.Now().Add(time.Hour)
			if expired {
				expiry = time.Now().Add(-time.Hour)
			}
			daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/me":
					fmt.Fprint(w, `{"id":1}`)
				case "/api/services":
					fmt.Fprint(w, `{"services":[]}`)
				case "/api/nodes":
					fmt.Fprint(w, `{"nodes":[]}`)
				case "/api/backups":
					fmt.Fprint(w, `{"backups":[]}`)
				case "/api/tls/hosts":
					fmt.Fprint(w, `{"hosts":[]}`)
				case "/api/tls/site":
					_ = json.NewEncoder(w).Encode(map[string]string{"not_after": expiry.Format(time.RFC3339)})
				default:
					fmt.Fprint(w, `{}`)
				}
			}))
			defer daemon.Close()
			app, err := NewApp(Config{DataDir: ":memory:", LoginDisabled: true, PMAPIURL: daemon.URL, PMAPIToken: "fixture"})
			if err != nil {
				t.Fatal(err)
			}
			defer app.Close()
			w := httptest.NewRecorder()
			app.Handler().ServeHTTP(w, httptest.NewRequest("GET", "http://x/web/observability", nil))
			if strings.Contains(w.Body.String(), ">Expired<") != expired {
				t.Fatalf("expired=%v status%d", expired, w.Code)
			}
		})
	}
}
