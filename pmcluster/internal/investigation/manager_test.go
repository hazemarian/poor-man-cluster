package investigation

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/observability"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
)

type blockingProvider struct{ entered chan struct{} }

func (p blockingProvider) Run(ctx context.Context, _ Policy, _ string, _ Input) (Verdict, *int64, error) {
	close(p.entered)
	<-ctx.Done()
	return Verdict{}, nil, errors.New("cancelled")
}

func TestManagerCancelKeepsDispatchedSpend(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	l := &Local{Store: st}
	p := DefaultPolicy()
	p.Enabled = true
	p.Sharing = true
	p.Model = "fixture"
	p.BaseURL = "http://127.0.0.1:1/v1"
	p.DailyTokens = 18000
	if err = l.SetPolicy(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	provider := blockingProvider{entered: make(chan struct{})}
	m := NewManager(provider, "fixture-key", observability.NewRedactor())
	in := Compact("why?", "start", "end", []observability.Row{observability.RowForLog("demo_web", "ERROR", "failure", "")}, false)
	job, err := m.Start(context.Background(), l, "owner", p, in)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-provider.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("worker did not start")
	}
	if _, ok := m.Get(job.ID, "other"); ok {
		t.Fatal("cross-owner read")
	}
	if m.Cancel(job.ID, "other") {
		t.Fatal("cross-owner cancel")
	}
	if !m.Cancel(job.ID, "owner") {
		t.Fatal("cancel failed")
	}
	m.Close()
	got, _ := m.Get(job.ID, "owner")
	if got.Status != "failed" {
		t.Fatalf("status=%s", got.Status)
	}
	b, _ := l.Budget(context.Background())
	if b.Charged != 18000 || b.Held != 0 {
		t.Fatalf("refund after dispatch: %+v", b)
	}
}

func TestManagerQueuedCancellationReleasesUndispatchedAllowance(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	l := &Local{Store: st}
	p := DefaultPolicy()
	p.Enabled = true
	p.Sharing = true
	p.Model = "fixture"
	p.BaseURL = "http://127.0.0.1:1/v1"
	p.DailyTokens = 36000
	if err = l.SetPolicy(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	provider := blockingProvider{entered: make(chan struct{})}
	m := NewManager(provider, "fixture-key", observability.NewRedactor())
	in := Compact("why?", "start", "end", []observability.Row{observability.RowForLog("demo_web", "ERROR", "failure", "")}, false)
	first, err := m.Start(context.Background(), l, "owner", p, in)
	if err != nil {
		t.Fatal(err)
	}
	<-provider.entered
	m.mu.Lock()
	m.lastStart = time.Time{}
	m.mu.Unlock()
	second, err := m.Start(context.Background(), l, "owner", p, in)
	if err != nil {
		t.Fatal(err)
	}
	m.Cancel(second.ID, "owner")
	m.Cancel(first.ID, "owner")
	m.Close()
	b, _ := l.Budget(context.Background())
	if b.Charged != 18000 || b.Held != 0 {
		t.Fatalf("queued cancellation not released: %+v", b)
	}
}
