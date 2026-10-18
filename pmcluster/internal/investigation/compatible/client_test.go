package compatible

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/investigation"
)

func TestCompatibleContractAndCitations(t *testing.T) {
	for _, bad := range []bool{false, true} {
		t.Run(fmt.Sprint(bad), func(t *testing.T) {
			var calls atomic.Int32
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer fixture-key" {
					t.Error("request contract")
				}
				var req map[string]any
				if json.NewDecoder(r.Body).Decode(&req) != nil {
					t.Error("bad body")
				}
				if req["model"] != "fixture-model" || req["max_completion_tokens"] != float64(2000) {
					t.Error(req)
				}
				id := "log:actual"
				if bad {
					id = "log:invented"
				}
				verdict := investigation.Verdict{Summary: "Observed connection failures", Evidence: []string{id}}
				content, _ := json.Marshal(verdict)
				_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"content": string(content)}, "finish_reason": "stop"}}, "usage": map[string]int{"total_tokens": 120}})
			}))
			defer s.Close()
			p := investigation.DefaultPolicy()
			p.BaseURL = s.URL + "/v1"
			p.Model = "fixture-model"
			_, usage, err := (Client{}).Run(context.Background(), p, "fixture-key", investigation.Input{Question: "What failed?", Evidence: []investigation.Evidence{{ID: "log:actual", Message: "connection refused"}}})
			if (err != nil) != bad || usage == nil || *usage != 120 {
				t.Fatalf("err=%v usage=%v", err, usage)
			}
			if calls.Load() != 1 {
				t.Fatal("automatic retry")
			}
		})
	}
}

func TestCompatibleRejectsRedirectAndOversizedBody(t *testing.T) {
	for _, redirect := range []bool{true, false} {
		t.Run(fmt.Sprint(redirect), func(t *testing.T) {
			var leaked atomic.Int32
			destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaked.Add(1) }))
			defer destination.Close()
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if redirect {
					http.Redirect(w, r, destination.URL, 307)
				} else {
					fmt.Fprint(w, strings.Repeat("a", (512<<10)+1))
				}
			}))
			defer s.Close()
			p := investigation.DefaultPolicy()
			p.BaseURL = s.URL
			_, _, err := (Client{}).Run(context.Background(), p, "fixture-key", investigation.Input{})
			if err == nil || leaked.Load() != 0 {
				t.Fatalf("err=%v redirected=%d", err, leaked.Load())
			}
		})
	}
}
