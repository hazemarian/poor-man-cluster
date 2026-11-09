package githubapp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Runner registration calls are installation-token calls, not JWT calls. These
// tests assert the wire shape (method, path, bearer, body) against an httptest
// server so no test reaches github.com, and they pin the error mapping the doctor
// relies on.
//
// The path is the assertion that matters for the org-level switch: a repo-scoped
// token cannot register an org runner, so the endpoint has to be /orgs/{org}/… and
// not /repos/{owner}/{repo}/….

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

func TestOrgRegistrationTokenUsesTheOrgEndpoint(t *testing.T) {
	const wantToken = "«redacted:ghs_…»"
	c := runnerTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/orgs/acme/actions/runners/registration-token" {
			t.Errorf("path = %s, want the ORG registration-token endpoint", r.URL.Path)
		}
		if strings.Contains(r.URL.Path, "/repos/") {
			t.Errorf("path = %s: a repo-scoped token cannot register an org runner", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer "+wantToken {
			t.Errorf("Authorization = %q, want the installation token", got)
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"token":"AABBCC","expires_at":"2026-11-09T23:00:00Z"}`))
	})

	tok, err := c.OrgRegistrationToken(context.Background(), wantToken, "acme")
	if err != nil {
		t.Fatalf("OrgRegistrationToken: %v", err)
	}
	if tok != "AABBCC" {
		t.Fatalf("token = %q", tok)
	}
}

func TestOrgRegistrationTokenNamesAdministrationWriteOnForbidden(t *testing.T) {
	c := runnerTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"Resource not accessible by integration"}`))
	})
	_, err := c.OrgRegistrationToken(context.Background(), "t", "acme")
	if !errors.Is(err, ErrInsufficientPermission) {
		t.Fatalf("err = %v, want ErrInsufficientPermission", err)
	}
	if !strings.Contains(err.Error(), "administration:write") {
		t.Fatalf("err = %q, want it to name administration:write (GitHub names neither)", err)
	}
	if !strings.Contains(err.Error(), "Organization") {
		t.Fatalf("err = %q, want it to name the ORGANIZATION permission level", err)
	}
}

func TestOrgRunnersDecodesTheRegistrationSet(t *testing.T) {
	c := runnerTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/orgs/acme/actions/runners" {
			t.Errorf("%s %s", r.Method, r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"total_count":2,"runners":[
			{"id":11,"name":"pmc-node1-org","status":"online","busy":true},
			{"id":12,"name":"pmc-org-runner","status":"offline","busy":false}]}`))
	})
	list, err := c.OrgRunners(context.Background(), "t", "acme")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("got %d runners", len(list))
	}
	if list[0].ID != 11 || list[0].Name != "pmc-node1-org" || !list[0].Busy {
		t.Fatalf("runner[0] = %+v", list[0])
	}
	if list[1].Status != "offline" {
		t.Fatalf("runner[1] = %+v", list[1])
	}
}

func TestDeleteOrgRunnerIsIdempotent(t *testing.T) {
	for _, status := range []int{http.StatusNoContent, http.StatusNotFound} {
		c := runnerTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodDelete || r.URL.Path != "/orgs/acme/actions/runners/11" {
				t.Errorf("%s %s", r.Method, r.URL.Path)
			}
			w.WriteHeader(status)
		})
		if err := c.DeleteOrgRunner(context.Background(), "t", "acme", 11); err != nil {
			t.Errorf("status %d: %v (a runner GitHub no longer knows is already gone)", status, err)
		}
	}
}

func TestCreateRunnerGroupIsSelectedVisibility(t *testing.T) {
	var body map[string]any
	c := runnerTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/orgs/acme/actions/runner-groups" {
			t.Errorf("%s %s", r.Method, r.URL.Path)
		}
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Fatalf("decode create body: %v", err)
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":7,"name":"pmcluster","visibility":"selected"}`))
	})
	g, err := c.CreateRunnerGroup(context.Background(), "t", "acme", "pmcluster", []int64{1, 2})
	if err != nil {
		t.Fatal(err)
	}
	if g.ID != 7 || g.Visibility != GroupVisibilitySelected {
		t.Fatalf("group = %+v", g)
	}
	if got := body["visibility"]; got != GroupVisibilitySelected {
		t.Fatalf("create body visibility = %v, want %q — a group created any other way can take jobs from repos nobody vetted", got, GroupVisibilitySelected)
	}
	ids, _ := body["selected_repository_ids"].([]any)
	if len(ids) != 2 {
		t.Fatalf("selected_repository_ids = %v, want the two private repos", body["selected_repository_ids"])
	}
}

func TestPrivateRepoIDsKeepsPrivateNonArchivedOnly(t *testing.T) {
	c := runnerTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/orgs/acme/repos" || r.URL.Query().Get("type") != "private" {
			t.Errorf("path = %s?%s, want the org's private repo list", r.URL.Path, r.URL.RawQuery)
		}
		_, _ = w.Write([]byte(`[
			{"id":1,"private":true,"archived":false},
			{"id":2,"private":false,"archived":false},
			{"id":3,"private":true,"archived":true}]`))
	})
	ids, err := c.PrivateRepoIDs(context.Background(), "t", "acme")
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != 1 {
		t.Fatalf("ids = %v, want only the live private repo (a public repo's fork PRs would run on the runner)", ids)
	}
}

func TestOrgLatestRunURLDecodesRepoAndRun(t *testing.T) {
	c := runnerTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/orgs/acme/actions/runs" {
			t.Errorf("path = %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"workflow_runs":[{"html_url":"https://github.com/acme/api/actions/runs/42","repository":{"full_name":"acme/api"}}]}`))
	})
	repo, url, err := c.OrgLatestRunURL(context.Background(), "t", "acme")
	if err != nil {
		t.Fatal(err)
	}
	if repo != "acme/api" || !strings.HasSuffix(url, "/runs/42") {
		t.Fatalf("repo=%q url=%q", repo, url)
	}
}

func TestRunnerAPIsRejectAMalformedOrg(t *testing.T) {
	c := &Client{APIBase: "http://127.0.0.1:1"}
	if _, err := c.OrgRegistrationToken(context.Background(), "t", "acme/api"); err == nil {
		t.Fatal("accepted a repo path as an organisation")
	}
	if _, err := c.OrgRunners(context.Background(), "t", "../etc"); err == nil {
		t.Fatal("accepted a path-traversing organisation")
	}
	if _, err := c.CreateRunnerGroup(context.Background(), "t", "acme", "a/b", nil); err == nil {
		t.Fatal("accepted a slash in a group name")
	}
}
