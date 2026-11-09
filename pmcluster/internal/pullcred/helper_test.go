package pullcred

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeGetter is the helper's credential source in tests, so the protocol is
// exercised without a listener.
type fakeGetter struct {
	tok    Token
	err    error
	server string
	called int
}

func (f *fakeGetter) Get(_ context.Context, server string) (Token, error) {
	f.called++
	f.server = server
	return f.tok, f.err
}

// runHelper executes one verb and returns (exit, stdout, stderr).
func runHelper(g Getter, stdin, args string) (int, string, string) {
	var out, errOut bytes.Buffer
	h := Helper{Getter: g, Out: &out, Err: &errOut}
	code := h.Serve(strings.Fields("docker-credential-pmcluster "+args), strings.NewReader(stdin))
	return code, out.String(), errOut.String()
}

// TestHelperGetPrintsExactlyOneCredentialObject is the whole contract with
// Docker: exit 0 and ONE JSON object on stdout. A stray log line on stdout
// breaks every pull, so this asserts the byte shape, not just the fields.
func TestHelperGetPrintsExactlyOneCredentialObject(t *testing.T) {
	g := &fakeGetter{tok: Token{Username: "x-access-token", Secret: "ghs_pull_test", ExpiresAt: time.Now().Add(time.Hour)}}
	code, out, errOut := runHelper(g, "ghcr.io\n", "get")

	if code != ExitOK {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, errOut)
	}
	if errOut != "" {
		t.Errorf("stderr = %q, want empty on success", errOut)
	}
	if strings.Count(strings.TrimSpace(out), "\n") != 0 {
		t.Errorf("stdout has more than one line: %q", out)
	}
	var got Credential
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("stdout is not one JSON object: %v (%q)", err, out)
	}
	want := Credential{ServerURL: "ghcr.io", Username: "x-access-token", Secret: "ghs_pull_test"}
	if got != want {
		t.Errorf("credential = %+v, want %+v", got, want)
	}
	if g.server != "ghcr.io" {
		t.Errorf("asked the cluster for %q, want the server from stdin", g.server)
	}
}

// TestHelperGetFailureIsEmptyStdout: Docker parses stdout, so a failure must
// leave it untouched and put the reason on stderr.
func TestHelperGetFailureIsEmptyStdout(t *testing.T) {
	g := &fakeGetter{err: errors.New("reach the credential listener at https://127.0.0.1:9443: connection refused")}
	code, out, errOut := runHelper(g, "ghcr.io\n", "get")

	if code == ExitOK {
		t.Fatal("exit = 0 on a failed get")
	}
	if out != "" {
		t.Errorf("stdout = %q, want empty on failure", out)
	}
	if !strings.Contains(errOut, "connection refused") {
		t.Errorf("stderr = %q, want the cause Docker will show the operator", errOut)
	}
}

// TestHelperGetRejectsEmptyStdin: no registry URL means nothing to ask for.
func TestHelperGetRejectsEmptyStdin(t *testing.T) {
	g := &fakeGetter{tok: Token{Secret: "ghs_never"}}
	code, out, _ := runHelper(g, "\n \n", "get")
	if code == ExitOK {
		t.Fatal("exit = 0 with no server URL on stdin")
	}
	if out != "" {
		t.Errorf("stdout = %q, want empty", out)
	}
	if g.called != 0 {
		t.Error("asked the cluster for a credential for an empty registry")
	}
}

// TestHelperGetRejectsAnEmptyToken: a success body with no secret is not a
// credential, and printing it would make Docker retry the same failing pull.
func TestHelperGetRejectsAnEmptyToken(t *testing.T) {
	g := &fakeGetter{tok: Token{Username: "x-access-token", Secret: ""}}
	code, out, errOut := runHelper(g, "ghcr.io\n", "get")
	if code == ExitOK {
		t.Fatal("exit = 0 with an empty token")
	}
	if out != "" {
		t.Errorf("stdout = %q, want empty", out)
	}
	if !strings.Contains(errOut, "empty token") {
		t.Errorf("stderr = %q, want it to say the token was empty", errOut)
	}
}

// TestHelperNoOpVerbsAreSilent: store/erase must succeed without storing
// anything and without a byte on stdout — Docker calls them after a `docker
// login` and a helper that failed there would fail an unrelated login.
func TestHelperNoOpVerbsAreSilent(t *testing.T) {
	g := &fakeGetter{tok: Token{Secret: "ghs_never"}}
	for _, verb := range []string{"store", "erase"} {
		for _, stdin := range []string{`{"ServerURL":"ghcr.io","Username":"u","Secret":"s"}`, "ghcr.io\n", ""} {
			code, out, errOut := runHelper(g, stdin, verb)
			if code != ExitOK {
				t.Errorf("%s: exit = %d, want 0", verb, code)
			}
			if out != "" {
				t.Errorf("%s: stdout = %q, want empty", verb, out)
			}
			if errOut != "" {
				t.Errorf("%s: stderr = %q, want empty", verb, errOut)
			}
		}
	}
	if g.called != 0 {
		t.Error("store/erase contacted the cluster; they must be no-ops")
	}
}

// TestHelperListPrintsAnEmptyMap: nothing is stored, so `docker-credential
// list` reports nothing.
func TestHelperListPrintsAnEmptyMap(t *testing.T) {
	code, out, errOut := runHelper(&fakeGetter{}, "", "list")
	if code != ExitOK {
		t.Fatalf("exit = %d", code)
	}
	if strings.TrimSpace(out) != "{}" {
		t.Errorf("stdout = %q, want {}", out)
	}
	if errOut != "" {
		t.Errorf("stderr = %q, want empty", errOut)
	}
}

// TestHelperUsageErrors: an unknown verb and a missing verb are usage errors,
// never a silent success.
func TestHelperUsageErrors(t *testing.T) {
	for _, args := range []string{"", "frobnicate"} {
		code, out, errOut := runHelper(&fakeGetter{}, "", args)
		if code != ExitUsage {
			t.Errorf("%q: exit = %d, want %d", args, code, ExitUsage)
		}
		if out != "" {
			t.Errorf("%q: stdout = %q, want empty", args, out)
		}
		if !strings.Contains(errOut, "usage") {
			t.Errorf("%q: stderr = %q, want a usage line", args, errOut)
		}
	}
}

// TestRemotePresentsTheNodeCertificate is the client half end to end: a real
// TLS listener that demands a client certificate, a real node certificate read
// from a directory laid out like Docker's, and the helper's credential path.
func TestRemotePresentsTheNodeCertificate(t *testing.T) {
	ca := newTestAuthority(t, "swarm-ca")
	nodeCert, _ := ca.newNodeCert(t, testSwarmID, testNodeID)
	certDir := writeNodeCert(t, nodeCert)

	serverCert, err := SelfSignedServerCert("127.0.0.1")
	if err != nil {
		t.Fatalf("server certificate: %v", err)
	}
	tlsCfg, err := newVerifier(testNodeID).TLSConfig(ca.pem, serverCert)
	if err != nil {
		t.Fatalf("TLSConfig: %v", err)
	}
	srv := httptest.NewUnstartedServer((&Server{
		Verifier: newVerifier(testNodeID),
		Tokens:   &fakeTokens{tok: Token{Secret: "ghs_remote_test", ExpiresAt: time.Now().Add(time.Hour)}},
	}).Handler())
	srv.TLS = tlsCfg
	srv.StartTLS()
	defer srv.Close()

	tok, err := Remote{URL: srv.URL, CertDir: certDir, Timeout: 5 * time.Second}.Get(context.Background(), Registry)
	if err != nil {
		t.Fatalf("Remote.Get: %v", err)
	}
	if tok.Secret != "ghs_remote_test" || tok.Username != Username {
		t.Fatalf("token = %+v, want the minted one", tok)
	}

	// And the same client against a listener that does NOT trust its CA: the
	// handshake fails, so no token is minted and the helper reports the failure.
	foreign := newTestAuthority(t, "swarm-ca")
	foreignTLS, err := newVerifier(testNodeID).TLSConfig(foreign.pem, serverCert)
	if err != nil {
		t.Fatalf("TLSConfig (foreign): %v", err)
	}
	other := httptest.NewUnstartedServer((&Server{
		Verifier: newVerifier(testNodeID),
		Tokens:   &fakeTokens{tok: Token{Secret: "ghs_never"}},
	}).Handler())
	other.TLS = foreignTLS
	other.StartTLS()
	defer other.Close()

	if tok, err := (Remote{URL: other.URL, CertDir: certDir, Timeout: 5 * time.Second}).Get(context.Background(), Registry); err == nil {
		t.Fatalf("a listener that does not trust this node's CA handed out %q", tok.Secret)
	}
}

// TestRemoteFailsClearlyWithoutACertificate: a node without the swarm files
// gets a message that names the directory, not a TLS alert.
func TestRemoteFailsClearlyWithoutACertificate(t *testing.T) {
	dir := t.TempDir()
	_, err := Remote{URL: "https://127.0.0.1:1", CertDir: dir, Timeout: 500 * time.Millisecond}.Get(context.Background(), Registry)
	if err == nil {
		t.Fatal("Get succeeded without a node certificate")
	}
	if !strings.Contains(err.Error(), dir) {
		t.Errorf("error = %v, want it to name %s", err, dir)
	}
}

// TestRemoteReportsAListenerRefusal: a 401 from the listener is Docker's
// "unauthorized" plus the reason, not a bare error.
func TestRemoteReportsAListenerRefusal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
	}))
	defer srv.Close()

	_, err := Remote{URL: srv.URL, HTTPClient: srv.Client(), Timeout: 2 * time.Second}.Get(context.Background(), Registry)
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("error = %v, want it to report the listener's status", err)
	}
}

// writeNodeCert lays out swarm-node.crt / swarm-node.key exactly where Docker
// keeps them, so tls.LoadX509KeyPair exercises the real file names.
func writeNodeCert(t *testing.T, certKeyPair tls.Certificate) string {
	t.Helper()
	dir := t.TempDir()
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certKeyPair.Certificate[0]})
	key, ok := certKeyPair.PrivateKey.(*ecdsa.PrivateKey)
	if !ok {
		t.Fatalf("unexpected key type %T", certKeyPair.PrivateKey)
	}
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der})
	if err := os.WriteFile(filepath.Join(dir, "swarm-node.crt"), certPEM, 0o600); err != nil {
		t.Fatalf("write cert: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "swarm-node.key"), keyPEM, 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	return dir
}
