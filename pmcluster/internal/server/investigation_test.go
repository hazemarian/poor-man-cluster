package server

import (
	"context"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/auth"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/investigation"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
)

type investigationLookup struct{}

func (investigationLookup) UserByToken(_ context.Context, token string) (*auth.User, error) {
	u := &auth.User{ID: 1, Name: token, Role: token}
	if token == "scoped" {
		u.Role = auth.RoleAdmin
		u.Stack = "demo"
	}
	return u, nil
}

func TestInvestigationAccountingRejectsOperatorTampering(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	l := &investigation.Local{Store: st}
	p := investigation.DefaultPolicy()
	p.Enabled = true
	p.Sharing = true
	p.Model = "fixture"
	p.BaseURL = "http://127.0.0.1:1/v1"
	p.DailyTokens = 18000
	if err = l.SetPolicy(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	h := New(Deps{Lookup: investigationLookup{}, Investigation: l})
	for _, tc := range []struct {
		role, method, path, body string
		status                   int
	}{
		{"viewer", "GET", "/api/investigation/policy", "", 200},
		{"operator", "POST", "/api/investigation/reservations/00000000000000000000000000000000/settle", `{"usage":0}`, 403},
		{"viewer", "POST", "/api/investigation/reservations/00000000000000000000000000000000/reserve", "", 403},
		{"scoped", "GET", "/api/investigation/policy", "", 403},
		{"operator", "PUT", "/api/investigation/policy", `{}`, 403},
		{"admin", "POST", "/api/investigation/reservations/00000000000000000000000000000000/reserve", "", 200},
	} {
		t.Run(tc.role+tc.path, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			req.Header.Set("Authorization", "Bearer "+tc.role)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			if w.Code != tc.status {
				t.Fatalf("status%d want%d: %s", w.Code, tc.status, w.Body.String())
			}
		})
	}
}
