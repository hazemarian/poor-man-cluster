package providers

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/investigation"
)

// gemini uses the native generateContent API with JSON output. The base URL
// includes the API version, for example
// https://generativelanguage.googleapis.com/v1beta. The key travels in a header,
// never the query string, so it cannot reach an access log.
func (r Router) gemini(ctx context.Context, p investigation.Policy, key string, in investigation.Input) (investigation.Verdict, *int64, error) {
	if !modelName.MatchString(p.Model) {
		return investigation.Verdict{}, nil, errors.New("unsupported model name")
	}
	input, err := encodeInput(p, in)
	if err != nil {
		return investigation.Verdict{}, nil, err
	}
	body := map[string]any{
		"systemInstruction": map[string]any{"parts": []map[string]string{{"text": investigation.SystemPrompt}}},
		"contents":          []map[string]any{{"role": "user", "parts": []map[string]string{{"text": input}}}},
		"generationConfig":  map[string]any{"responseMimeType": "application/json", "maxOutputTokens": p.MaxOutputTokens},
	}
	endpoint := trimBase(p.BaseURL) + "/models/" + url.PathEscape(p.Model) + ":generateContent"
	data, err := investigation.PostJSON(ctx, r.HTTP, endpoint, withRouting(map[string]string{"x-goog-api-key": key}, p.BaseURL), body)
	if err != nil {
		return investigation.Verdict{}, nil, err
	}
	var out struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"content"`
			FinishReason string `json:"finishReason"`
		} `json:"candidates"`
		Usage *struct {
			Total int64 `json:"totalTokenCount"`
		} `json:"usageMetadata"`
	}
	if json.Unmarshal(data, &out) != nil {
		return investigation.Verdict{}, nil, errors.New("invalid provider response")
	}
	var usage *int64
	if out.Usage != nil {
		usage = investigation.PositiveUsage(out.Usage.Total)
	}
	if len(out.Candidates) != 1 || out.Candidates[0].FinishReason != "STOP" {
		return investigation.Verdict{}, usage, errors.New("provider result incomplete")
	}
	var text string
	for _, part := range out.Candidates[0].Content.Parts {
		text += part.Text
	}
	verdict, err := investigation.ParseVerdict(text, in)
	return verdict, usage, err
}
