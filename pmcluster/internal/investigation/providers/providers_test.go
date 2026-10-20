package providers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/investigation"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/investigation/compatible"
)

func fixtureInput() investigation.Input {
	return investigation.Input{Question: "What failed?", Evidence: []investigation.Evidence{{ID: "log:actual", Message: "connection refused", Count: 3}}}
}

func verdictJSON(id string) string {
	b, _ := json.Marshal(investigation.Verdict{Summary: "Observed connection failures", Evidence: []string{id}})
	return string(b)
}

func policy(proto, base string) investigation.Policy {
	p := investigation.DefaultPolicy()
	p.Protocol, p.BaseURL, p.Model = proto, base, "fixture-model"
	return p
}

func TestAnthropicContract(t *testing.T) {
	for name, tc := range map[string]struct {
		text, stop string
		wantErr    bool
	}{
		"ok":         {verdictJSON("log:actual"), "end_turn", false},
		"fenced":     {"```json\n" + verdictJSON("log:actual") + "\n```", "end_turn", false},
		"invented":   {verdictJSON("log:invented"), "end_turn", true},
		"cut off":    {verdictJSON("log:actual"), "max_tokens", true},
		"prose":      {"Here is my answer: " + verdictJSON("log:actual"), "end_turn", true},
		"extra json": {verdictJSON("log:actual") + `{"x":1}`, "end_turn", true},
	} {
		t.Run(name, func(t *testing.T) {
			var calls atomic.Int32
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.URL.Path != "/v1/messages" || r.Header.Get("x-api-key") != "fixture-key" || r.Header.Get("anthropic-version") == "" || r.Header.Get("Authorization") != "" {
					t.Errorf("request contract: %s %v", r.URL.Path, r.Header)
				}
				var req map[string]any
				_ = json.NewDecoder(r.Body).Decode(&req)
				if req["model"] != "fixture-model" || req["max_tokens"] != float64(2000) || req["system"] == "" {
					t.Errorf("body: %v", req)
				}
				_ = json.NewEncoder(w).Encode(map[string]any{
					"content": []any{map[string]string{"type": "text", "text": tc.text}}, "stop_reason": tc.stop,
					"usage": map[string]int{"input_tokens": 100, "output_tokens": 20},
				})
			}))
			defer s.Close()
			_, usage, err := Router{}.Run(context.Background(), policy("anthropic", s.URL), "fixture-key", fixtureInput())
			if (err != nil) != tc.wantErr {
				t.Fatalf("err=%v", err)
			}
			if usage == nil || *usage != 120 {
				t.Errorf("usage=%v", usage)
			}
			if calls.Load() != 1 {
				t.Error("automatic retry")
			}
		})
	}
}

func TestAnthropicBaseMayIncludeV1(t *testing.T) {
	var path string
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		_ = json.NewEncoder(w).Encode(map[string]any{"content": []any{map[string]string{"type": "text", "text": verdictJSON("log:actual")}}, "stop_reason": "end_turn"})
	}))
	defer s.Close()
	if _, _, err := (Router{}).Run(context.Background(), policy("anthropic", s.URL+"/v1/"), "k", fixtureInput()); err != nil || path != "/v1/messages" {
		t.Fatalf("%v %q", err, path)
	}
}

func TestGeminiContract(t *testing.T) {
	for _, bad := range []bool{false, true} {
		id := "log:actual"
		if bad {
			id = "log:invented"
		}
		var query string
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			query = r.URL.RawQuery
			if r.URL.Path != "/v1beta/models/fixture-model:generateContent" || r.Header.Get("x-goog-api-key") != "fixture-key" {
				t.Errorf("request contract: %s", r.URL.Path)
			}
			var req map[string]any
			_ = json.NewDecoder(r.Body).Decode(&req)
			cfg, _ := req["generationConfig"].(map[string]any)
			if cfg["responseMimeType"] != "application/json" || cfg["maxOutputTokens"] != float64(2000) {
				t.Errorf("generationConfig: %v", cfg)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"candidates":    []any{map[string]any{"finishReason": "STOP", "content": map[string]any{"parts": []any{map[string]string{"text": verdictJSON(id)}}}}},
				"usageMetadata": map[string]int{"totalTokenCount": 77},
			})
		}))
		_, usage, err := Router{}.Run(context.Background(), policy("gemini", s.URL+"/v1beta"), "fixture-key", fixtureInput())
		s.Close()
		if (err != nil) != bad {
			t.Fatalf("bad=%v err=%v", bad, err)
		}
		if usage == nil || *usage != 77 {
			t.Errorf("usage=%v", usage)
		}
		if strings.Contains(query, "fixture-key") || query != "" {
			t.Errorf("key or parameters in query string: %q", query)
		}
	}
}

func TestGeminiRejectsModelNamesThatEscapeThePath(t *testing.T) {
	var called atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called.Add(1) }))
	defer s.Close()
	for _, model := range []string{"../admin", "m?key=x", "a/b", "m:generateContent#", ""} {
		p := policy("gemini", s.URL)
		p.Model = model
		if _, _, err := (Router{}).Run(context.Background(), p, "k", fixtureInput()); err == nil {
			t.Errorf("accepted %q", model)
		}
	}
	if called.Load() != 0 {
		t.Error("request sent for a rejected model name")
	}
}

func TestRouterDefaultsToOpenAIAndNeedsKey(t *testing.T) {
	var path string
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"content": verdictJSON("log:actual")}, "finish_reason": "stop"}}})
	}))
	defer s.Close()
	p := policy("", s.URL+"/v1")
	if _, _, err := (Router{}).Run(context.Background(), p, "k", fixtureInput()); err != nil || path != "/v1/chat/completions" {
		t.Fatalf("%v %q", err, path)
	}
	if _, _, err := (Router{}).Run(context.Background(), p, "", fixtureInput()); err == nil {
		t.Error("ran without a key")
	}
}

func TestProvidersRejectOversizedInputBeforeSending(t *testing.T) {
	var called atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called.Add(1) }))
	defer s.Close()
	in := fixtureInput()
	in.Evidence[0].Message = strings.Repeat("x", 40000)
	for _, proto := range []string{"anthropic", "gemini"} {
		if _, _, err := (Router{}).Run(context.Background(), policy(proto, s.URL), "k", in); err == nil {
			t.Errorf("%s: oversized input accepted", proto)
		}
	}
	if called.Load() != 0 {
		t.Error("oversized input reached the provider")
	}
}

func TestPolicyProtocolValidation(t *testing.T) {
	p := investigation.DefaultPolicy()
	for _, ok := range []string{"", "openai", "anthropic", "gemini"} {
		p.Protocol = ok
		if p.Validate() != nil {
			t.Errorf("rejected %q", ok)
		}
	}
	p.Protocol = "bedrock"
	if p.Validate() == nil {
		t.Error("accepted unknown protocol")
	}
}

func TestOpenCodeGoGetsSessionHeaderOthersDoNot(t *testing.T) {
	if h := routingHeaders("https://opencode.ai/inference/go/openai/v1"); len(h["x-opencode-session"]) != 32 {
		t.Errorf("opencode.ai: %v", h)
	}
	if a, b := routingHeaders("https://opencode.ai/zen/go/v1"), routingHeaders("https://opencode.ai/zen/go/v1"); a["x-opencode-session"] == b["x-opencode-session"] {
		t.Error("session id reused across investigations")
	}
	for _, base := range []string{"https://api.openai.com/v1", "https://api.anthropic.com", "https://evilopencode.ai/v1", "https://opencode.ai.evil.example/v1", "://bad"} {
		if h := routingHeaders(base); h != nil {
			t.Errorf("%s got %v", base, h)
		}
	}
}

func TestOpenAIClientForwardsRoutingHeaders(t *testing.T) {
	var got string
	h := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("x-opencode-session")
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"content": verdictJSON("log:actual")}, "finish_reason": "stop"}}})
	}))
	defer h.Close()
	c := compatible.Client{Headers: map[string]string{"x-opencode-session": "abc"}}
	if _, _, err := c.Run(context.Background(), policy("", h.URL+"/v1"), "k", fixtureInput()); err != nil || got != "abc" {
		t.Fatalf("%v %q", err, got)
	}
}

func TestOpenCodeUsesMaxTokensWhateverThePolicySays(t *testing.T) {
	if !isOpenCode("https://opencode.ai/inference/go/openai/v1") || isOpenCode("https://api.openai.com/v1") {
		t.Fatal("host detection")
	}
}
