package providers

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/investigation"
)

const anthropicVersion = "2023-06-01"

// anthropic uses the Messages API. The base URL may be the host root or end in
// /v1. The API has no JSON mode, so the prompt carries the output contract and
// the reply is validated strictly.
func (r Router) anthropic(ctx context.Context, p investigation.Policy, key string, in investigation.Input) (investigation.Verdict, *int64, error) {
	input, err := encodeInput(p, in)
	if err != nil {
		return investigation.Verdict{}, nil, err
	}
	url := trimBase(p.BaseURL)
	if len(url) < 3 || url[len(url)-3:] != "/v1" {
		url += "/v1"
	}
	body := map[string]any{
		"model": p.Model, "max_tokens": p.MaxOutputTokens, "system": investigation.SystemPrompt,
		"messages": []map[string]string{{"role": "user", "content": input}},
	}
	data, err := investigation.PostJSON(ctx, r.HTTP, url+"/messages", withRouting(map[string]string{"x-api-key": key, "anthropic-version": anthropicVersion}, p.BaseURL), body)
	if err != nil {
		return investigation.Verdict{}, nil, err
	}
	var out struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
		Usage      *struct {
			In  int64 `json:"input_tokens"`
			Out int64 `json:"output_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal(data, &out) != nil {
		return investigation.Verdict{}, nil, errors.New("invalid provider response")
	}
	var usage *int64
	if out.Usage != nil {
		usage = investigation.PositiveUsage(out.Usage.In + out.Usage.Out)
	}
	if out.StopReason != "end_turn" && out.StopReason != "stop_sequence" {
		return investigation.Verdict{}, usage, errors.New("provider result incomplete")
	}
	var text string
	for _, c := range out.Content {
		if c.Type == "text" {
			text += c.Text
		}
	}
	verdict, err := investigation.ParseVerdict(text, in)
	return verdict, usage, err
}
