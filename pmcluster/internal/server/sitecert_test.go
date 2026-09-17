package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/hazemarian/poor-man-stack/pmcluster/internal/auth"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/store"
)

// newSiteCertServer wires a fresh store (with a persisted domain) plus a
// SiteCertService whose Apply is a recording stub returning the same row the
// real cluster.ApplySiteCert would.
func newSiteCertServer(t *testing.T) (*httptest.Server, *store.Store, *siteCertApplyRecorder) {
	t.Helper()
	st, err := store.Open(t.TempDir() + "/sitecert.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	ctx := context.Background()
	if err := st.SetSetting(ctx, "domain", "test.example.com"); err != nil {
		t.Fatalf("persist domain: %v", err)
	}

	rec := &siteCertApplyRecorder{store: st, main: "test.example.com"}
	sc := &SiteCertService{Svc: rec}

	srv := httptest.NewServer(New(Deps{
		Lookup:   &fakeLookup{users: map[string]*auth.User{"tok": {Name: "admin"}}},
		SiteCert: sc,
	}))
	t.Cleanup(srv.Close)
	return srv, st, rec
}

type siteCertApplyRecorder struct {
	store   *store.Store
	main    string
	calls   int
	domain  string
	certPEM string
	keyPEM  string
	err     error
}

// SiteCert and ApplyHostCert satisfy service.TLSService; the handler only
// knows the port.
func (r *siteCertApplyRecorder) SiteCert(ctx context.Context, domain, certPEM, keyPEM string) (*store.SiteCertRow, error) {
	return r.apply(ctx, domain, certPEM, keyPEM)
}

func (r *siteCertApplyRecorder) ApplyHostCert(ctx context.Context, host, certPEM, keyPEM string, _ bool) (*store.SiteCertRow, error) {
	return r.apply(ctx, host, certPEM, keyPEM)
}

func (r *siteCertApplyRecorder) RemoveHostCert(ctx context.Context, host string, _ bool) error {
	if _, err := r.store.GetSiteCert(ctx, host); err != nil {
		return err
	}
	return r.store.DeleteSiteCert(ctx, host)
}

func (r *siteCertApplyRecorder) List(ctx context.Context) ([]store.SiteCertRow, error) {
	rows, err := r.store.ListSiteCerts(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]store.SiteCertRow, 0, len(rows))
	return append(out, rows...), nil
}

func (r *siteCertApplyRecorder) GetSiteCert(ctx context.Context, domain string) (*store.SiteCertRow, error) {
	row, err := r.store.GetSiteCert(ctx, domain)
	if err != nil {
		return nil, err
	}
	return &row, nil
}

func (r *siteCertApplyRecorder) MainDomain(ctx context.Context) (string, error) {
	return r.main, nil
}

func (r *siteCertApplyRecorder) apply(ctx context.Context, domain, certPEM, keyPEM string) (*store.SiteCertRow, error) {
	r.calls++
	r.domain, r.certPEM, r.keyPEM = domain, certPEM, keyPEM
	if r.err != nil {
		return nil, r.err
	}
	now := time.Now().UTC()
	row := &store.SiteCertRow{
		Domain:     domain,
		CertSecret: "cert_v042",
		KeySecret:  "key_v042",
		NotBefore:  now.Add(-time.Hour),
		NotAfter:   now.Add(24 * time.Hour),
		SANs:       []string{"test.example.com"},
		CertHash:   store.ConfigHash(certPEM),
		KeyHash:    store.ConfigHash(keyPEM),
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	if err := r.store.PutSiteCert(ctx, *row); err != nil {
		return nil, err
	}
	return row, nil
}

func TestSiteCertAPI(t *testing.T) {
	srv, st, rec := newSiteCertServer(t)
	base := srv.URL + "/api/tls/site"
	cert, key := genCert(t, "test.example.com")

	t.Run("unauth → 401", func(t *testing.T) {
		resp := doJSON(t, http.MethodGet, base, "", nil)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401", resp.StatusCode)
		}
	})

	t.Run("get before any apply → 404", func(t *testing.T) {
		resp := doJSON(t, http.MethodGet, base, "tok", nil)
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("status = %d, want 404", resp.StatusCode)
		}
	})

	t.Run("put missing fields → 400", func(t *testing.T) {
		resp := doJSON(t, http.MethodPut, base, "tok", map[string]any{"cert": cert})
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", resp.StatusCode)
		}
	})

	t.Run("put apply error → 500", func(t *testing.T) {
		rec.err = errors.New("boom")
		defer func() { rec.err = nil }()
		resp := doJSON(t, http.MethodPut, base, "tok", map[string]any{"cert": cert, "key": key})
		if resp.StatusCode != http.StatusInternalServerError {
			t.Errorf("status = %d, want 500", resp.StatusCode)
		}
	})

	t.Run("put success → 200 + row persisted", func(t *testing.T) {
		rec.calls = 0
		resp := doJSON(t, http.MethodPut, base, "tok", map[string]any{"cert": cert, "key": key})
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		var got siteCertResponse
		if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if got.Domain != "test.example.com" || got.CertSecret != "cert_v042" || got.NotAfter == "" {
			t.Errorf("unexpected response: %+v", got)
		}
		if rec.calls != 1 || rec.domain != "test.example.com" {
			t.Errorf("apply should be called once for the right domain: calls=%d domain=%q", rec.calls, rec.domain)
		}
		if rec.certPEM != cert || rec.keyPEM != key {
			t.Error("apply should receive the exact uploaded PEM bytes")
		}

		row, err := st.GetSiteCert(context.Background(), "test.example.com")
		if err != nil {
			t.Fatalf("row not persisted: %v", err)
		}
		if row.CertSecret != "cert_v042" {
			t.Errorf("row secret = %q", row.CertSecret)
		}
	})

	t.Run("get after apply → 200 with metadata", func(t *testing.T) {
		resp := doJSON(t, http.MethodGet, base, "tok", nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		var got siteCertResponse
		if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if got.Domain != "test.example.com" || got.CertSecret != "cert_v042" || got.NotAfter == "" || got.NotBefore == "" {
			t.Errorf("unexpected get response: %+v", got)
		}
	})
}
