package githubapp

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestInstallationsAndToken(t *testing.T) {
	fake := newFakeGitHub(t)
	c := fake.client()
	app := mustApp(t, 4242)
	ctx := context.Background()

	installs, err := c.Installations(ctx, app)
	if err != nil {
		t.Fatalf("Installations: %v", err)
	}
	if len(installs) != 1 || installs[0].ID != 7 || installs[0].AccountLogin != "hazemarian" {
		t.Fatalf("installations = %+v", installs)
	}

	tok, err := c.Token(ctx, app, 7)
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if tok.Token != fake.token {
		t.Errorf("token = %q, want the fake's token", tok.Token)
	}
	if d := time.Until(tok.ExpiresAt); d < 55*time.Minute || d > 65*time.Minute {
		t.Errorf("expires in %s, want about an hour", d)
	}
}

func TestToken_ErrorsNameTheCause(t *testing.T) {
	cases := []struct {
		name   string
		status int
		want   error
		text   string
	}{
		{"installation removed", http.StatusNotFound, ErrInstallationNotFound, "uninstalled"},
		{"key rejected", http.StatusUnauthorized, ErrBadKey, "app JWT"},
		{"forbidden", http.StatusForbidden, ErrInsufficientPermission, "forbade"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := newFakeGitHub(t)
			fake.tokenStatus = tc.status
			_, err := fake.client().Token(context.Background(), mustApp(t, 1), 7)
			if err == nil {
				t.Fatal("Token succeeded on a failing fake")
			}
			if !errors.Is(err, tc.want) {
				t.Errorf("error %v does not wrap %v", err, tc.want)
			}
			if !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(tc.text)) {
				t.Errorf("error %q does not mention %q", err, tc.text)
			}
		})
	}
}

func TestToken_ZeroInstallationID(t *testing.T) {
	fake := newFakeGitHub(t)
	_, err := fake.client().Token(context.Background(), mustApp(t, 1), 0)
	if !errors.Is(err, ErrInstallationNotFound) {
		t.Fatalf("error = %v, want ErrInstallationNotFound", err)
	}
	if fake.tokenCalls.Load() != 0 {
		t.Error("a zero installation id must not reach the network")
	}
}

func TestPermissions_ReportsGrantedSetAndMissing(t *testing.T) {
	fake := newFakeGitHub(t)
	fake.permissions = map[string]string{"contents": "read", "metadata": "read"}
	perms, err := fake.client().Permissions(context.Background(), mustApp(t, 4242))
	if err != nil {
		t.Fatalf("Permissions: %v", err)
	}
	if !perms.Has("contents", "read") {
		t.Error("contents:read should be granted")
	}
	if perms.Has("packages", "read") {
		t.Error("packages:read should be missing")
	}
	missing := perms.Missing(RequiredPermissionSet)
	if len(missing) != 1 || missing[0] != PermPackagesRead {
		t.Errorf("missing = %v, want [%s]", missing, PermPackagesRead)
	}
}

func TestPermissions_KeyRejected(t *testing.T) {
	fake := newFakeGitHub(t)
	fake.appStatus = http.StatusUnauthorized
	_, err := fake.client().Permissions(context.Background(), mustApp(t, 1))
	if !errors.Is(err, ErrBadKey) {
		t.Fatalf("error = %v, want ErrBadKey", err)
	}
}

func TestRankOrdering(t *testing.T) {
	p := &Permissions{Permissions: map[string]string{"packages": "write"}}
	if !p.Has("packages", "read") || !p.Has("packages", "write") || p.Has("packages", "admin") {
		t.Errorf("rank ordering is wrong for %+v", p.Permissions)
	}
	if p.Has("contents", "read") {
		t.Error("an absent permission must not read as granted")
	}
}
