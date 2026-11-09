package githubapp

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Runner registration calls are installation-token calls, not JWT calls. These
// tests assert the wire shape (method, path, bearer) against an httptest server
// so no test reaches github.com, and they pin the error mapping doctor relies on.

func runnerTestClient(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); !strings.HasPrefix(got, "Bearer ") {
			t.Errorf("Authorization = %q, want a bearer token", got)
		}
		h(w, r)
	}))
	t.Cleanup(srv.Close)
	return &Client{APIBase: srv.URL, HTTP: srv.Client()}
}

func TestRunnerRegistrationTokenMintsWithTheInstallationToken(t *testing.T) {
	const wantToken = "ghs_installation"
	c := runnerTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/repos/acme/api/actions/runners/registration-token" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer "+wantToken {
			t.Errorf("Authorization = %q, want the installation token", got)
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"token":"AABBCC","expires_at":"2026-11-09T23:00:00Z"}`))
	})

	tok, err := c.RunnerRegistrationToken(context.Background(), wantToken, "acme/api")
	if err != nil {
		t.Fatalf("RunnerRegistrationToken: %v", err)
	}
	if tok != "AABBCC" {
		t.Fatalf("token = %q", tok)
	}
}

func TestRunnerRegistrationTokenNamesAdministrationWriteOnForbidden(t *testing.T) {
	c := runnerTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"Resource not accessible by integration"}`))
	})
	_, err := c.RunnerRegistrationToken(context.Background(), "t", "acme/api")
	if !errors.Is(err, ErrInsufficientPermission) {
		t.Fatalf("err = %v, want ErrInsufficientPermission", err)
	}
	if !strings.Contains(err.Error(), "administration:write") {
		t.Fatalf("err = %q, want it to name administration:write (GitHub names neither)", err)
	}
}

func TestListRunnersDecodesTheRegistrationSet(t *testing.T) {
	c := runnerTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/repos/acme/api/actions/runners" {
			t.Errorf("%s %s", r.Method, r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"total_count":2,"runners":[
			{"id":11,"name":"pmc-node1-acme-api","status":"online","busy":true},
			{"id":12,"name":"laptop","status":"offline","busy":false}]}`))
	})
	list, err := c.Runners(context.Background(), "t", "acme/api")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("got %d runners", len(list))
	}
	if list[0].ID != 11 || list[0].Name != "pmc-node1-acme-api" || !list[0].Busy {
		t.Fatalf("runner[0] = %+v", list[0])
	}
	if list[1].Status != "offline" {
		t.Fatalf("runner[1] = %+v", list[1])
	}
}

func TestDeleteRunnerIsIdempotent(t *testing.T) {
	for _, status := range []int{http.StatusNoContent, http.StatusNotFound} {
		c := runnerTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodDelete || r.URL.Path != "/repos/acme/api/actions/runners/11" {
				t.Errorf("%s %s", r.Method, r.URL.Path)
			}
			w.WriteHeader(status)
		})
		if err := c.DeleteRunner(context.Background(), "t", "acme/api", 11); err != nil {
			t.Errorf("status %d: %v (a runner GitHub no longer knows is already gone)", status, err)
		}
	}
}

func TestRepoPrivate(t *testing.T) {
	for _, private := range []bool{true, false} {
		body := `{"private":false}`
		if private {
			body = `{"private":true}`
		}
		c := runnerTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/repos/acme/api" {
				t.Errorf("path = %s", r.URL.Path)
			}
			_, _ = w.Write([]byte(body))
		})
		got, err := c.RepoPrivate(context.Background(), "t", "acme/api")
		if err != nil {
			t.Fatal(err)
		}
		if got != private {
			t.Fatalf("RepoPrivate = %v, want %v", got, private)
		}
	}
}

func TestRepoPrivateNotFoundIsInstallationScope(t *testing.T) {
	c := runnerTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	if _, err := c.RepoPrivate(context.Background(), "t", "acme/missing"); !errors.Is(err, ErrInstallationNotFound) {
		t.Fatalf("err = %v, want ErrInstallationNotFound", err)
	}
}

func TestRunnerAPIsRejectAMalformedRepo(t *testing.T) {
	c := &Client{APIBase: "http://127.0.0.1:1"}
	if _, err := c.RunnerRegistrationToken(context.Background(), "t", "acme"); err == nil {
		t.Fatal("accepted a repo with no owner")
	}
	if _, err := c.Runners(context.Background(), "t", "acme/api/extra"); err == nil {
		t.Fatal("accepted a repo with three path segments")
	}
}
