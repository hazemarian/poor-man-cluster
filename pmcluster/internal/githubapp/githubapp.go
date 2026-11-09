// Package githubapp mints short-lived GitHub App installation tokens and
// serves them to two consumers that must not hold a long-lived credential:
//
//   - pmcluster itself, for private GHCR image pulls;
//   - GitHub Actions workflows in the org's repos, through an HMAC-authenticated
//     broker endpoint (POST /api/github/token), so a repo can delete its
//     `secrets.GH_PAT`.
//
// The App's private key never leaves the control plane: it is stored encrypted
// in data.db, decrypted to sign a 9-minute RS256 JWT, and used only to ask
// GitHub for an installation token (one hour). Neither the key, the JWT, nor a
// minted token is ever logged or persisted.
//
// Permissions are a property of the APP, not of this package. The App's own
// configuration is the ceiling: a broker caller receives every permission the
// installation was granted, because GitHub's token endpoint issues installation
// tokens and cannot narrow them. The console and `pmcluster doctor` therefore
// report the granted permission set instead of implying per-call scoping.
package githubapp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/rs/zerolog"
)

// DefaultAPIBase is GitHub's REST API root. Tests point Client at an httptest
// server instead; no test reaches github.com.
const DefaultAPIBase = "https://api.github.com"

// Token lifetime and refresh point.
//
// GitHub documents installation tokens as valid for one hour. A token is
// refreshed after 50 minutes so a pull or a deploy that starts just before
// expiry still has ten minutes of headroom, and so no request ever carries a
// token that is about to die mid-pull.
const (
	DefaultRefreshAfter = 50 * time.Minute
	defaultTokenTTL     = time.Hour
)

// Sentinel errors. Callers (doctor, the console card, the broker) switch on
// these to say which of the four causes it was instead of surfacing an opaque
// 403 later.
var (
	// ErrNoApp means no app credentials are configured. Callers treat it as a
	// skip, not a failure: a cluster that pulls only public images is fine.
	ErrNoApp = errors.New("github app not configured")
	// ErrBadKey means GitHub rejected the app identity: a malformed PEM, the
	// wrong app id, or a key that does not belong to that app.
	ErrBadKey = errors.New("github app private key rejected")
	// ErrInstallationNotFound means the installation no longer exists — it was
	// uninstalled, or the configured id belongs to another app.
	ErrInstallationNotFound = errors.New("github app installation not found")
	// ErrTokenRejected means GitHub refused a request made with a token or JWT
	// it had issued, which points at a revoked installation or a clock skew.
	ErrTokenRejected = errors.New("github token rejected")
	// ErrInsufficientPermission means the installation is alive but its granted
	// permissions do not include what the caller needed (usually contents:read
	// or packages:read).
	ErrInsufficientPermission = errors.New("github app lacks a required permission")
)

// App is a validated app identity: an app id plus its parsed RSA private key.
// The key is unexported so no caller can accidentally log or serialise it.
type App struct {
	ID  int64
	key *rsaKey
}

// Client talks to the GitHub REST API. All methods are safe for concurrent use;
// it holds no mutable state.
type Client struct {
	// APIBase overrides DefaultAPIBase (tests).
	APIBase string
	// HTTP overrides http.DefaultClient.
	HTTP *http.Client
	// Now overrides time.Now (JWT claims and expiry arithmetic in tests).
	Now func() time.Time
	// Log receives non-secret diagnostics. Optional.
	Log *zerolog.Logger
}

// apiBase returns the effective API root.
func (c *Client) apiBase() string {
	if c.APIBase != "" {
		return strings.TrimRight(c.APIBase, "/")
	}
	return DefaultAPIBase
}

// now returns the effective clock.
func (c *Client) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// httpClient returns the effective HTTP client.
func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return http.DefaultClient
}

// debugf logs a non-secret diagnostic line. Nothing that reaches this helper
// may contain a token, a JWT, or key material; callers pass ids and statuses.
func (c *Client) debugf(msg string, fields map[string]any) {
	if c.Log == nil {
		return
	}
	ev := c.Log.Debug()
	for k, v := range fields {
		ev = ev.Interface(k, v)
	}
	ev.Msg(msg)
}

// Installation is one installation of the app.
type Installation struct {
	ID      int64  `json:"id"`
	Account string `json:"-"`
	// AccountLogin is the org/user the app is installed on.
	AccountLogin string `json:"account_login"`
	// RepositorySelection is "all" or "selected".
	RepositorySelection string `json:"repository_selection"`
}

// installationJSON is GitHub's wire shape (account is a nested object).
type installationJSON struct {
	ID                  int64  `json:"id"`
	RepositorySelection string `json:"repository_selection"`
	Account             struct {
		Login string `json:"login"`
	} `json:"account"`
}

// Token is one minted installation token.
type Token struct {
	Token     string
	ExpiresAt time.Time
}

// Permissions is the app's granted permission set, as GitHub reports it under
// GET /app. Keys are GitHub permission names ("contents", "packages", …) and
// values are "read", "write" or "admin".
type Permissions struct {
	AppID       int64             `json:"app_id"`
	Slug        string            `json:"slug"`
	Permissions map[string]string `json:"permissions"`
}

// Has reports whether the app holds at least level ("read" < "write" < "admin")
// for name. An unknown level ranks below read.
func (p *Permissions) Has(name, level string) bool {
	if p == nil {
		return false
	}
	return permRank(p.Permissions[name]) >= permRank(level)
}

// Missing returns the permission names from required that the app does not
// hold at the given level, in the order given.
func (p *Permissions) Missing(required map[string]string) []string {
	var out []string
	for name, level := range required {
		if !p.Has(name, level) {
			out = append(out, name+":"+level)
		}
	}
	sort.Strings(out)
	return out
}

func permRank(level string) int {
	switch strings.ToLower(level) {
	case "read":
		return 1
	case "write":
		return 2
	case "admin":
		return 3
	}
	return 0
}

// NewApp validates an app id and PEM private key. The error is ErrBadKey for
// any unparseable or non-RSA key, so the CLI can say "this is not a usable app
// key" instead of dumping an x509 message the operator cannot act on.
func NewApp(appID int64, privateKeyPEM []byte) (*App, error) {
	if appID <= 0 {
		return nil, fmt.Errorf("%w: app id must be positive, got %d", ErrBadKey, appID)
	}
	key, err := parseRSAKey(privateKeyPEM)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBadKey, err)
	}
	return &App{ID: appID, key: key}, nil
}

// Installations lists the app's installations (GET /app/installations).
func (c *Client) Installations(ctx context.Context, app *App) ([]Installation, error) {
	jwt, err := signJWT(app, c.now())
	if err != nil {
		return nil, err
	}
	body, status, err := c.do(ctx, http.MethodGet, "/app/installations", jwt, nil)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, c.statusError(status, body, "list installations")
	}
	var raw []installationJSON
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("decode installations: %w", err)
	}
	out := make([]Installation, 0, len(raw))
	for _, r := range raw {
		out = append(out, Installation{
			ID:                  r.ID,
			AccountLogin:        r.Account.Login,
			RepositorySelection: r.RepositorySelection,
		})
	}
	return out, nil
}

// Token mints an installation token (POST
// /app/installations/{id}/access_tokens). The request body is empty: the
// installation's granted permissions are the ceiling and this call cannot
// narrow them.
func (c *Client) Token(ctx context.Context, app *App, installationID int64) (Token, error) {
	if installationID <= 0 {
		return Token{}, fmt.Errorf("%w: installation id must be positive, got %d", ErrInstallationNotFound, installationID)
	}
	jwt, err := signJWT(app, c.now())
	if err != nil {
		return Token{}, err
	}
	path := fmt.Sprintf("/app/installations/%d/access_tokens", installationID)
	body, status, err := c.do(ctx, http.MethodPost, path, jwt, []byte("{}"))
	if err != nil {
		return Token{}, err
	}
	switch status {
	case http.StatusCreated, http.StatusOK:
	case http.StatusNotFound:
		return Token{}, fmt.Errorf("%w: installation %d (uninstalled, or it belongs to another app)", ErrInstallationNotFound, installationID)
	case http.StatusUnauthorized:
		return Token{}, fmt.Errorf("%w: GitHub refused the app JWT for installation %d (check the app id, the key, and this host's clock)", ErrBadKey, installationID)
	case http.StatusForbidden:
		return Token{}, fmt.Errorf("%w: GitHub forbade minting a token for installation %d: %s", ErrInsufficientPermission, installationID, apiMessage(body))
	default:
		return Token{}, fmt.Errorf("mint installation token: %s", httpErrorText(status, body))
	}
	var resp struct {
		Token     string `json:"token"`
		ExpiresAt string `json:"expires_at"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return Token{}, fmt.Errorf("decode installation token: %w", err)
	}
	if resp.Token == "" {
		return Token{}, errors.New("mint installation token: GitHub returned an empty token")
	}
	expires, err := time.Parse(time.RFC3339, resp.ExpiresAt)
	if err != nil {
		// An unparseable expiry is not fatal: assume GitHub's documented hour
		// so the cache still refreshes well before the token can die.
		expires = c.now().Add(defaultTokenTTL)
	}
	c.debugf("minted installation token", map[string]any{
		"app_id":          app.ID,
		"installation_id": installationID,
		"expires_at":      expires.UTC().Format(time.RFC3339),
	})
	return Token{Token: resp.Token, ExpiresAt: expires}, nil
}

// Permissions reads the app's granted permissions (GET /app). This is how the
// console and doctor name a missing packages:read instead of letting a registry
// pull fail with a 403 hours later.
func (c *Client) Permissions(ctx context.Context, app *App) (*Permissions, error) {
	jwt, err := signJWT(app, c.now())
	if err != nil {
		return nil, err
	}
	body, status, err := c.do(ctx, http.MethodGet, "/app", jwt, nil)
	if err != nil {
		return nil, err
	}
	switch status {
	case http.StatusOK:
	case http.StatusUnauthorized:
		return nil, fmt.Errorf("%w: GitHub refused the app JWT for app %d (check the app id, the key, and this host's clock)", ErrBadKey, app.ID)
	default:
		return nil, fmt.Errorf("read app permissions: %s", httpErrorText(status, body))
	}
	var resp struct {
		ID          int64             `json:"id"`
		Slug        string            `json:"slug"`
		Permissions map[string]string `json:"permissions"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("decode app permissions: %w", err)
	}
	perms := &Permissions{AppID: resp.ID, Slug: resp.Slug, Permissions: resp.Permissions}
	if perms.Permissions == nil {
		perms.Permissions = map[string]string{}
	}
	return perms, nil
}

// do performs one authenticated API call. The JWT is never included in an
// error: auth errors carry the status and GitHub's message only.
func (c *Client) do(ctx context.Context, method, path, jwt string, payload []byte) (body []byte, status int, err error) {
	var rdr io.Reader
	if payload != nil {
		rdr = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.apiBase()+path, rdr)
	if err != nil {
		return nil, 0, fmt.Errorf("build %s %s: %w", method, path, err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("Authorization", "Bearer "+jwt)
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	// 1 MiB bounds a hostile or misrouted response; every documented reply here
	// is far smaller.
	body, err = io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("read %s %s response: %w", method, path, err)
	}
	c.debugf("github api call", map[string]any{
		"method": method,
		"path":   path,
		"status": resp.StatusCode,
	})
	return body, resp.StatusCode, nil
}

// statusError maps a non-200 reply to one of the sentinel causes where the
// status allows a confident diagnosis.
func (c *Client) statusError(status int, body []byte, what string) error {
	switch status {
	case http.StatusUnauthorized:
		return fmt.Errorf("%w: GitHub refused the app JWT while trying to %s (check the app id, the key, and this host's clock)", ErrBadKey, what)
	case http.StatusForbidden:
		return fmt.Errorf("%w: GitHub forbade the request to %s: %s", ErrInsufficientPermission, what, apiMessage(body))
	default:
		return fmt.Errorf("%s: %s", what, httpErrorText(status, body))
	}
}

// apiMessage extracts GitHub's {"message": …} without echoing headers or the
// request. It never returns token material — GitHub's error bodies do not carry
// any, and a non-JSON body is reduced to a short status line.
func apiMessage(body []byte) string {
	var resp struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(body, &resp); err == nil && resp.Message != "" {
		return resp.Message
	}
	return "no message"
}

func httpErrorText(status int, body []byte) string {
	return fmt.Sprintf("GitHub returned HTTP %d: %s", status, apiMessage(body))
}
