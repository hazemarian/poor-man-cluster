package githubapp

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

func TestMinter_TenConcurrentCallersMintOneToken(t *testing.T) {
	fake := newFakeGitHub(t)
	app := mustApp(t, 4242)
	m := &Minter{Client: fake.client(), App: app, InstallationID: 7}

	const callers = 10
	var wg sync.WaitGroup
	tokens := make([]string, callers)
	errs := make([]error, callers)
	start := make(chan struct{})
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			tok, err := m.Token(context.Background())
			tokens[i], errs[i] = tok.Token, err
		}(i)
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("caller %d: %v", i, err)
		}
		if tokens[i] != fake.token {
			t.Fatalf("caller %d got %q, want the shared token", i, tokens[i])
		}
	}
	if got := fake.tokenCalls.Load(); got != 1 {
		t.Fatalf("minted %d tokens for %d concurrent callers, want 1", got, callers)
	}
}

func TestMinter_RefreshesAfterTheRefreshPoint(t *testing.T) {
	fake := newFakeGitHub(t)
	clock := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	client := fake.client()
	client.Now = func() time.Time { return clock }
	m := &Minter{Client: client, App: mustApp(t, 4242), InstallationID: 7}

	if _, err := m.Token(context.Background()); err != nil {
		t.Fatalf("first mint: %v", err)
	}
	// Well inside the 50-minute window: reused.
	clock = clock.Add(49 * time.Minute)
	if _, err := m.Token(context.Background()); err != nil {
		t.Fatalf("cached mint: %v", err)
	}
	if got := fake.tokenCalls.Load(); got != 1 {
		t.Fatalf("minted %d tokens inside the window, want 1", got)
	}
	// Past the refresh point: one more mint, not one per call.
	clock = clock.Add(2 * time.Minute)
	for i := 0; i < 3; i++ {
		if _, err := m.Token(context.Background()); err != nil {
			t.Fatalf("refresh mint: %v", err)
		}
	}
	if got := fake.tokenCalls.Load(); got != 2 {
		t.Fatalf("minted %d tokens after the refresh point, want 2", got)
	}
}

func TestMinter_FailedMintIsNotCached(t *testing.T) {
	fake := newFakeGitHub(t)
	fake.tokenStatus = 500
	m := &Minter{Client: fake.client(), App: mustApp(t, 1), InstallationID: 7}
	if _, err := m.Token(context.Background()); err == nil {
		t.Fatal("mint succeeded on a failing fake")
	}
	fake.tokenStatus = 0
	if _, err := m.Token(context.Background()); err != nil {
		t.Fatalf("second mint after the fake recovered: %v", err)
	}
	if got := fake.tokenCalls.Load(); got != 2 {
		t.Fatalf("token endpoint called %d times, want 2 (no cached failure)", got)
	}
}

func TestMinter_NoInstallationID(t *testing.T) {
	m := &Minter{Client: newFakeGitHub(t).client(), App: mustApp(t, 1)}
	_, err := m.Token(context.Background())
	if !errors.Is(err, ErrInstallationNotFound) {
		t.Fatalf("error = %v, want ErrInstallationNotFound", err)
	}
}

// TestMinter_NeverLogsSecrets is the log contract: the token, the JWT and the
// private key must never appear in the daemon's log, even at debug level.
func TestMinter_NeverLogsSecrets(t *testing.T) {
	var buf bytes.Buffer
	logger := zerolog.New(&buf).Level(zerolog.DebugLevel)
	fake := newFakeGitHub(t)
	client := fake.client()
	client.Log = &logger

	keyPEM := testKeyPEM(t)
	app, err := NewApp(4242, keyPEM)
	if err != nil {
		t.Fatalf("NewApp: %v", err)
	}
	m := &Minter{Client: client, App: app, InstallationID: 7, Log: &logger}
	if _, err := m.Token(context.Background()); err != nil {
		t.Fatalf("mint: %v", err)
	}
	output := buf.String()
	if output == "" {
		t.Fatal("nothing was logged, so the redaction assertion would be vacuous")
	}
	if !strings.Contains(output, "4242") || !strings.Contains(output, "installation_id") {
		t.Errorf("expected the mint to log its ids; got %q", output)
	}
	if strings.Contains(output, fake.token) {
		t.Errorf("the installation token leaked into the log: %q", output)
	}
	// The PEM body, not its BEGIN/END labels, is the secret.
	for _, line := range strings.Split(string(keyPEM), "\n") {
		if len(line) > 32 && strings.Contains(output, line) {
			t.Errorf("private key material leaked into the log: %q", output)
		}
	}
	for _, forbidden := range []string{"Bearer ", "eyJ", "PRIVATE KEY", "ghs_"} {
		if strings.Contains(output, forbidden) {
			t.Errorf("log output contains %q, which is key or token material: %q", forbidden, output)
		}
	}
}
