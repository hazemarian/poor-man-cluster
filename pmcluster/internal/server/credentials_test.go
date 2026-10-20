package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/auth"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/cluster"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
)

type credentialsServiceStub struct {
	rotateCalls int
}

func (s *credentialsServiceStub) List(context.Context) ([]*store.ManagedCredential, error) {
	return nil, nil
}

func (s *credentialsServiceStub) Rotate(context.Context, string) (*cluster.ManagedCredential, error) {
	s.rotateCalls++
	return &cluster.ManagedCredential{
		Name:            "traefik_dashboard",
		Username:        "admin",
		Password:        "shown-once",
		SwarmSecretName: "admin_credentials_v2",
	}, nil
}

func TestCredentialsRotateRequiresAdminRole(t *testing.T) {
	lookup := &fakeLookup{users: map[string]*auth.User{
		"viewer-token":   {ID: 1, Name: "viewer", Role: auth.RoleViewer},
		"operator-token": {ID: 2, Name: "operator", Role: auth.RoleOperator},
		"admin-token":    {ID: 3, Name: "admin", Role: auth.RoleAdmin},
	}}
	service := &credentialsServiceStub{}
	srv := httptest.NewServer(New(Deps{
		Lookup:      lookup,
		Credentials: &CredentialsHTTP{Svc: service},
	}))
	t.Cleanup(srv.Close)

	for _, tc := range []struct {
		name   string
		token  string
		status int
	}{
		{name: "viewer is denied", token: "viewer-token", status: http.StatusForbidden},
		{name: "operator is denied", token: "operator-token", status: http.StatusForbidden},
		{name: "admin can rotate", token: "admin-token", status: http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodPost, srv.URL+"/api/credentials/traefik_dashboard/rotate", nil)
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
	if service.rotateCalls != 1 {
		t.Fatalf("rotate service calls = %d, want 1", service.rotateCalls)
	}
}
