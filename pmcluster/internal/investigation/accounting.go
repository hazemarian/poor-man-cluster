// Package investigation implements bounded, read-only model investigations.
package investigation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
)

// Policy is durable, nonsecret configuration; a provider key is mounted separately.
type Policy struct {
	Enabled         bool   `json:"enabled"`
	Sharing         bool   `json:"sharing"`
	BaseURL         string `json:"base_url"`
	Model           string `json:"model"`
	DailyTokens     int64  `json:"daily_tokens"`
	MaxInputTokens  int64  `json:"max_input_tokens"`
	MaxOutputTokens int64  `json:"max_output_tokens"`
	TokenField      string `json:"token_field"`
}

// DefaultPolicy disables paid work until an administrator configures it.
func DefaultPolicy() Policy {
	return Policy{MaxInputTokens: 16000, MaxOutputTokens: 2000, TokenField: "max_completion_tokens"}
}

// Validate rejects unsupported endpoints, excessive allowances and incomplete enabled policies.
func (p Policy) Validate() error {
	if p.MaxInputTokens < 1024 || p.MaxInputTokens > 200000 || p.MaxOutputTokens < 128 || p.MaxOutputTokens > 16000 || p.DailyTokens < 0 || p.DailyTokens > 1_000_000_000 {
		return fmt.Errorf("invalid token limits")
	}
	if p.TokenField != "max_completion_tokens" && p.TokenField != "max_tokens" {
		return fmt.Errorf("unsupported output token field")
	}
	if len(p.Model) > 200 || len(p.BaseURL) > 2048 {
		return fmt.Errorf("provider configuration too long")
	}
	if p.BaseURL != "" {
		u, err := url.Parse(p.BaseURL)
		if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return fmt.Errorf("invalid provider URL")
		}
		ip := net.ParseIP(u.Hostname())
		local := u.Hostname() == "localhost" || (ip != nil && ip.IsLoopback())
		if u.Scheme != "https" && !(u.Scheme == "http" && local) {
			return fmt.Errorf("provider requires HTTPS or loopback HTTP")
		}
	}
	if p.Enabled && (!p.Sharing || strings.TrimSpace(p.Model) == "" || p.BaseURL == "" || p.DailyTokens < p.MaxInputTokens+p.MaxOutputTokens) {
		return fmt.Errorf("enable requires provider, model, sharing consent and budget")
	}
	return nil
}

// Reservation and Budget are durable daemon-owned accounting records.
type Reservation = store.InvestigationReservation

// Budget is a global UTC reservation-day usage snapshot.
type Budget = store.InvestigationBudget

// Ledger is the edge's only access to durable policy and budget state.
type Ledger interface {
	Policy(context.Context) (Policy, error)
	Reserve(context.Context, string) (Reservation, error)
	Dispatch(context.Context, string) (Reservation, error)
	Settle(context.Context, string, *int64) (Reservation, error)
	Budget(context.Context) (Budget, error)
}

// Service adds administrator-only policy changes to the ledger contract.
type Service interface {
	Ledger
	SetPolicy(context.Context, Policy) error
}

// Local adapts durable store accounting to the domain interface.
type Local struct{ Store *store.Store }

// Policy reads configuration strictly; database errors never become enabled defaults.
func (l *Local) Policy(ctx context.Context) (Policy, error) {
	raw, err := l.Store.Setting(ctx, store.InvestigationPolicyKey)
	if errors.Is(err, store.ErrSettingNotFound) {
		return DefaultPolicy(), nil
	}
	if err != nil {
		return Policy{}, err
	}
	var p Policy
	if err = json.Unmarshal([]byte(raw), &p); err != nil {
		return p, err
	}
	return p, p.Validate()
}

// SetPolicy validates before persisting the entire nonsecret policy atomically.
func (l *Local) SetPolicy(ctx context.Context, p Policy) error {
	if err := p.Validate(); err != nil {
		return err
	}
	b, err := json.Marshal(p)
	if err != nil {
		return err
	}
	return l.Store.SetSetting(ctx, store.InvestigationPolicyKey, string(b))
}

// Reserve admits one full configured allowance.
func (l *Local) Reserve(ctx context.Context, id string) (Reservation, error) {
	return l.Store.ReserveInvestigation(ctx, id)
}

// Dispatch marks an admitted job as potentially billable.
func (l *Local) Dispatch(ctx context.Context, id string) (Reservation, error) {
	return l.Store.DispatchInvestigation(ctx, id)
}

// Settle records authoritative usage or conservative unknown usage.
func (l *Local) Settle(ctx context.Context, id string, n *int64) (Reservation, error) {
	return l.Store.SettleInvestigation(ctx, id, n)
}

// Budget returns durable current-day accounting.
func (l *Local) Budget(ctx context.Context) (Budget, error) { return l.Store.InvestigationBudget(ctx) }
