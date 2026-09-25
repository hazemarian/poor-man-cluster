package cluster

import (
	"strings"
	"testing"
)

func TestOpenObserveSessionCookie_MatchesMinted(t *testing.T) {
	// The value OO mints on POST /auth/login for these creds — verified live.
	want := "eyJhY2Nlc3NfdG9rZW4iOiJCYXNpYyBZV1J0YVc1QWJtVjRkSEoxYlMxemVTNWpiMjA2UldkbmJ6UktaMVIwY2pCNE1Ga2hTWGM0VG5WWFNVbHNiMFZTYzNkclJHZENielZwIiwicmVmcmVzaF90b2tlbiI6IiJ9"
	got := openObserveSessionCookie("admin@nextrum-sy.com", "Eggo4JgTtr0x0Y!Iw8NuWIIloERswkDgBo5i")
	if got != want {
		t.Fatalf("cookie mismatch:\n got %s\nwant %s", got, want)
	}
}

func TestOpenObserveSessionCookie_Empty(t *testing.T) {
	if openObserveSessionCookie("", "x") != "" {
		t.Fatal("expected empty cookie for empty email")
	}
	if openObserveSessionCookie("a@b.c", "") != "" {
		t.Fatal("expected empty cookie for empty password")
	}
}

func TestRenderTraefikDynamic_CookieInjected(t *testing.T) {
	in := RenderInput{
		Domain:                   "example.com",
		OpenObserveBasicAuth:     openObserveBasicAuth("admin@example.com", "pw"),
		OpenObserveSessionCookie: openObserveSessionCookie("admin@example.com", "pw"),
	}
	out, err := RenderTraefikDynamic(in)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if !strings.Contains(s, "auth_tokens=") {
		t.Fatal("missing auth_tokens Set-Cookie header in rendered traefik config")
	}
	if !strings.Contains(s, "Max-Age=2592000") {
		t.Fatal("missing Max-Age on the auth_tokens cookie")
	}
	if !strings.Contains(s, "HttpOnly") {
		t.Fatal("missing HttpOnly on the auth_tokens cookie")
	}
}
