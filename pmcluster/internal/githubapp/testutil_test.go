package githubapp

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// hmacHex is the workflow-side signature: HMAC-SHA256 over timestamp+body.
func hmacHex(secret []byte, material string) string {
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write([]byte(material))
	return hex.EncodeToString(mac.Sum(nil))
}

// testKeyPEM generates a throwaway RSA key in the PKCS#8 shape GitHub
// downloads. 2048 bits is the smallest GitHub accepts; generating one per test
// keeps the suite free of fixture files.
func testKeyPEM(t *testing.T) []byte {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
}

type fakeGitHub struct {
	srv *httptest.Server
	// tokenCalls counts POST access_tokens requests.
	tokenCalls atomic.Int32
	// token is what the access_tokens endpoint returns.
	token string
	// ttl is the reported lifetime.
	ttl time.Duration
	// permissions is what GET /app reports.
	permissions map[string]string
	// tokenStatus, when non-zero, makes the mint fail with that status.
	tokenStatus int
	// installationsStatus, when non-zero, makes GET /app/installations fail.
	installationsStatus int
	// appStatus, when non-zero, makes GET /app fail with that status.
	appStatus int
}

func newFakeGitHub(t *testing.T) *fakeGitHub {
	t.Helper()
	f := &fakeGitHub{
		token: "ghs_fake_installation_token_do_not_leak",
		ttl:   time.Hour,
		permissions: map[string]string{
			"contents": "read",
			"packages": "read",
			"metadata": "read",
		},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/app", func(w http.ResponseWriter, r *http.Request) {
		if f.appStatus != 0 {
			w.WriteHeader(f.appStatus)
			_, _ = w.Write([]byte(`{"message":"Resource not accessible by integration"}`))
			return
		}
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": 4242, "slug": "pmcluster", "permissions": f.permissions,
		})
	})
	mux.HandleFunc("/app/installations", func(w http.ResponseWriter, r *http.Request) {
		if f.installationsStatus != 0 {
			w.WriteHeader(f.installationsStatus)
			_, _ = w.Write([]byte(`{"message":"Bad credentials"}`))
			return
		}
		_ = json.NewEncoder(w).Encode([]map[string]any{{
			"id": 7, "repository_selection": "all",
			"account": map[string]any{"login": "hazemarian"},
		}})
	})
	mux.HandleFunc("/app/installations/7/access_tokens", func(w http.ResponseWriter, r *http.Request) {
		f.tokenCalls.Add(1)
		if f.tokenStatus != 0 {
			w.WriteHeader(f.tokenStatus)
			_, _ = w.Write([]byte(`{"message":"Not Found"}`))
			return
		}
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token":      f.token,
			"expires_at": time.Now().Add(f.ttl).UTC().Format(time.RFC3339),
		})
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

// client returns a Client pointed at the fake.
func (f *fakeGitHub) client() *Client {
	return &Client{APIBase: f.srv.URL, HTTP: f.srv.Client()}
}

// mustApp builds an App from a fresh key.
func mustApp(t *testing.T, appID int64) *App {
	t.Helper()
	app, err := NewApp(appID, testKeyPEM(t))
	if err != nil {
		t.Fatalf("NewApp: %v", err)
	}
	return app
}

// signedRequest builds a broker request the way a workflow would: HMAC over
// timestamp+body.
func signedRequest(t *testing.T, url, repo string, secret []byte, body string, ts time.Time) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	tsStr := fmt.Sprintf("%d", ts.Unix())
	req.Header.Set(BrokerRepoHeader, repo)
	req.Header.Set(BrokerTimestampHeader, tsStr)
	req.Header.Set(BrokerSignatureHeader, "sha256="+hmacHex(secret, tsStr+body))
	return req
}
