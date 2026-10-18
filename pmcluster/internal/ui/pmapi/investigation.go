package pmapi

import (
	"context"
	"net/http"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/investigation"
)

// InvestigationLedger adapts the fixed daemon client to durable accounting.
type InvestigationLedger struct{ Client *Client }

// Policy reads nonsecret model configuration.
func (l InvestigationLedger) Policy(ctx context.Context) (investigation.Policy, error) {
	var p investigation.Policy
	err := l.Client.do(ctx, http.MethodGet, "/investigation/policy", nil, &p)
	return p, err
}

// SetPolicy changes policy through the daemon's administrator-only endpoint.
func (l InvestigationLedger) SetPolicy(ctx context.Context, p investigation.Policy) error {
	return l.Client.do(ctx, http.MethodPut, "/investigation/policy", p, nil)
}

// Reserve admits one configured allowance.
func (l InvestigationLedger) Reserve(ctx context.Context, id string) (investigation.Reservation, error) {
	return l.change(ctx, id, "reserve", nil)
}

// Dispatch marks a potentially billable request.
func (l InvestigationLedger) Dispatch(ctx context.Context, id string) (investigation.Reservation, error) {
	return l.change(ctx, id, "dispatch", nil)
}

// Settle reconciles usage, retaining uncertain spend conservatively.
func (l InvestigationLedger) Settle(ctx context.Context, id string, n *int64) (investigation.Reservation, error) {
	return l.change(ctx, id, "settle", map[string]any{"usage": n})
}

// Budget reads current reservation-day token totals.
func (l InvestigationLedger) Budget(ctx context.Context) (investigation.Budget, error) {
	var b investigation.Budget
	err := l.Client.do(ctx, http.MethodGet, "/investigation/budget", nil, &b)
	return b, err
}
func (l InvestigationLedger) change(ctx context.Context, id, action string, body any) (investigation.Reservation, error) {
	var r investigation.Reservation
	err := l.Client.do(ctx, http.MethodPost, "/investigation/reservations/"+id+"/"+action, body, &r)
	return r, err
}
