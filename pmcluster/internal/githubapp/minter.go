package githubapp

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/rs/zerolog"
)

// TokenSource is what the broker and the registry-login path need from a
// minter: one current token, without caring whether it was cached.
type TokenSource interface {
	Token(ctx context.Context) (Token, error)
}

// Minter mints and caches ONE installation token.
//
// Ten concurrent image pulls must produce one mint, not ten: GitHub rate-limits
// the app JWT surface, and a fan-out of pulls is exactly when the cluster has
// least headroom for a failed auth. The mutex is held across the HTTP call, so
// concurrent callers wait for the single mint in flight and then share its
// result. A failed mint leaves the cache empty, so the next caller retries
// instead of inheriting the failure for an hour.
type Minter struct {
	Client         *Client
	App            *App
	InstallationID int64

	// RefreshAfter is how long a minted token is reused. Zero means
	// DefaultRefreshAfter (50 minutes). It is clamped so a refresh never lands
	// at or after the token's own expiry.
	RefreshAfter time.Duration

	// Log is optional. It receives ids and expiry only — never the token.
	Log *zerolog.Logger

	mu     sync.Mutex
	cached *cachedToken
}

type cachedToken struct {
	token     Token
	refreshAt time.Time
}

// Token returns a valid installation token, minting one only when the cache is
// empty or has reached its refresh point.
func (m *Minter) Token(ctx context.Context) (Token, error) {
	if m == nil || m.Client == nil || m.App == nil {
		return Token{}, ErrNoApp
	}
	if m.InstallationID <= 0 {
		return Token{}, fmt.Errorf("%w: no installation id configured (`pmcluster github app set --installation-id …`)", ErrInstallationNotFound)
	}

	// The lock covers the mint on purpose: one mint per refresh, no singleflight
	// bookkeeping, and a waiter that arrives after a failed mint gets its own
	// attempt when it takes the lock.
	m.mu.Lock()
	defer m.mu.Unlock()

	now := m.Client.now()
	if m.cached != nil && now.Before(m.cached.refreshAt) {
		return m.cached.token, nil
	}

	tok, err := m.Client.Token(ctx, m.App, m.InstallationID)
	if err != nil {
		return Token{}, err
	}

	refresh := m.RefreshAfter
	if refresh <= 0 {
		refresh = DefaultRefreshAfter
	}
	refreshAt := now.Add(refresh)
	// Never schedule a refresh at/after expiry: a token whose expiry is sooner
	// than RefreshAfter (GitHub shortened it, or the clock differs) is refreshed
	// a minute before it dies instead of being reused past death.
	if latest := tok.ExpiresAt.Add(-time.Minute); refreshAt.After(latest) {
		refreshAt = latest
	}
	m.cached = &cachedToken{token: tok, refreshAt: refreshAt}

	if m.Log != nil {
		m.Log.Info().
			Int64("app_id", m.App.ID).
			Int64("installation_id", m.InstallationID).
			Time("expires_at", tok.ExpiresAt.UTC()).
			Time("refresh_at", refreshAt.UTC()).
			Msg("minted GitHub App installation token")
	}
	return tok, nil
}

// Invalidate drops the cache so the next call mints a fresh token. Used when a
// consumer learns the token was rejected (installation suspended, key rotated)
// rather than at the scheduled refresh.
func (m *Minter) Invalidate() {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.cached = nil
	m.mu.Unlock()
}

// ExpiresAt reports the cached token's expiry, or the zero time when nothing is
// cached. Cheap enough for the console card; does not mint.
func (m *Minter) ExpiresAt() time.Time {
	if m == nil {
		return time.Time{}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cached == nil {
		return time.Time{}
	}
	return m.cached.token.ExpiresAt
}
