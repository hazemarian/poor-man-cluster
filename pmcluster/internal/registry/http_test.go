package registry_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/auth"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/credentials"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/registry"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/server"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
)

type lookupStub struct{ users map[string]*auth.User }

func (l lookupStub) UserByToken(_ context.Context, token string) (*auth.User, error) {
	return l.users[token], nil
}

type registryStoreStub struct {
	created []*store.Registry
	lookup  *store.Registry
	deleted []string
}

func (s *registryStoreStub) CreateRegistry(_ context.Context, r *store.Registry) error {
	s.created = append(s.created, r)
	return nil
}
func (*registryStoreStub) UpdateRegistry(context.Context, *store.Registry) error { return nil }
func (s *registryStoreStub) Registry(_ context.Context, host string) (*store.Registry, error) {
	if s.lookup != nil && s.lookup.Host == host {
		return s.lookup, nil
	}
	return nil, store.ErrRegistryNotFound
}
func (*registryStoreStub) ListRegistries(context.Context) ([]*store.Registry, error) {
	return nil, nil
}
func (s *registryStoreStub) DeleteRegistry(_ context.Context, host string) error {
	s.deleted = append(s.deleted, host)
	return nil
}

type runnerStub struct{ loginCalls int }

func (r *runnerStub) Login(context.Context, string, string, string) error {
	r.loginCalls++
	return nil
}
func (*runnerStub) Logout(context.Context, string) error { return nil }

func TestRegistryMutationsRequireAdminRole(t *testing.T) {
	cipher, err := credentials.Open(t.TempDir() + "/key")
	if err != nil {
		t.Fatal(err)
	}
	st := &registryStoreStub{}
	runner := &runnerStub{}
	svc := &registry.Service{Store: st, Cipher: cipher, Runner: runner}
	srv := httptest.NewServer(server.New(server.Deps{
		Lookup: lookupStub{users: map[string]*auth.User{
			"viewer-token":   {ID: 1, Name: "viewer", Role: auth.RoleViewer},
			"operator-token": {ID: 2, Name: "operator", Role: auth.RoleOperator},
			"admin-token":    {ID: 3, Name: "admin", Role: auth.RoleAdmin},
		}},
		Registries: &registry.HTTP{Svc: svc},
	}))
	t.Cleanup(srv.Close)

	for _, tc := range []struct {
		name   string
		token  string
		status int
	}{
		{name: "viewer is denied", token: "viewer-token", status: http.StatusForbidden},
		{name: "operator is denied", token: "operator-token", status: http.StatusForbidden},
		{name: "admin can add", token: "admin-token", status: http.StatusCreated},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodPost, srv.URL+"/api/registries", strings.NewReader(`{"host":"registry.example","username":"alice","password":"secret"}`))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Authorization", "Bearer "+tc.token)
			req.Header.Set("Content-Type", "application/json")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != tc.status {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tc.status)
			}
		})
	}
	if len(st.created) != 1 || runner.loginCalls != 1 {
		t.Fatalf("mutation calls = store:%d runner:%d, want 1 each", len(st.created), runner.loginCalls)
	}

	st.lookup = &store.Registry{Host: "registry.example", Username: "alice"}
	for _, tc := range []struct {
		name   string
		token  string
		status int
	}{
		{name: "viewer cannot delete", token: "viewer-token", status: http.StatusForbidden},
		{name: "operator cannot delete", token: "operator-token", status: http.StatusForbidden},
		{name: "admin can delete", token: "admin-token", status: http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodDelete, srv.URL+"/api/registries/registry.example", nil)
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Authorization", "Bearer "+tc.token)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != tc.status {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tc.status)
			}
		})
	}
	if len(st.deleted) != 1 || st.deleted[0] != "registry.example" {
		t.Fatalf("delete calls = %#v, want one admin deletion", st.deleted)
	}
}
