package store

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
)

func TestInvestigationAccountingPersistsAndSerializes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err = st.SetSetting(ctx, InvestigationPolicyKey, `{"enabled":true,"sharing":true,"daily_tokens":36000,"max_input_tokens":16000,"max_output_tokens":2000}`); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var admitted []string
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := fmt.Sprintf("%032x", i)
			if _, err := st.ReserveInvestigation(ctx, id); err == nil {
				mu.Lock()
				admitted = append(admitted, id)
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()
	if len(admitted) != 2 {
		t.Fatalf("admitted=%d", len(admitted))
	}
	if _, err = st.DispatchInvestigation(ctx, admitted[0]); err != nil {
		t.Fatal(err)
	}
	if _, err = st.DispatchInvestigation(ctx, admitted[0]); err == nil {
		t.Fatal("dispatch replay succeeded")
	}
	usage := int64(1200)
	if _, err = st.SettleInvestigation(ctx, admitted[0], &usage); err != nil {
		t.Fatal(err)
	}
	if _, err = st.SettleInvestigation(ctx, admitted[0], nil); err != nil {
		t.Fatal(err)
	}
	if _, err = st.SettleInvestigation(ctx, admitted[1], nil); err != nil {
		t.Fatal(err)
	}
	unknown := fmt.Sprintf("%032x", 100)
	if _, err = st.ReserveInvestigation(ctx, unknown); err != nil {
		t.Fatal(err)
	}
	if _, err = st.DispatchInvestigation(ctx, unknown); err != nil {
		t.Fatal(err)
	}
	if err = st.Close(); err != nil {
		t.Fatal(err)
	}
	st, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	b, err := st.InvestigationBudget(ctx)
	if err != nil || b.Held != 18000 || b.Charged != 1200 {
		t.Fatalf("reopened=%+v err=%v", b, err)
	}
	if _, err = st.SettleInvestigation(ctx, unknown, nil); err != nil {
		t.Fatal(err)
	}
	b, _ = st.InvestigationBudget(ctx)
	if b.Held != 0 || b.Charged != 19200 {
		t.Fatalf("unknown usage refunded: %+v", b)
	}
	if r, err := st.ReserveInvestigation(ctx, unknown); err != nil || r.Status != "settled" {
		t.Fatalf("idempotent reserve=%+v %v", r, err)
	}
}
