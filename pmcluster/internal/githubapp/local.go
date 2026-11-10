package githubapp

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/credentials"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
)

// DefaultRegistryBase is GHCR's registry API. The package HEAD probe uses it
// directly (docker login is not involved) so the check proves the token, not
// the local docker config.
const DefaultRegistryBase = "https://ghcr.io"

// ErrNoProbeTarget means the app is configured but neither probe_repo nor
// probe_package names anything to read, so only the mint itself could be
// asserted. Doctor reports this as a warning, never a pass.
var ErrNoProbeTarget = errors.New("no probe_repo or probe_package configured")

// RequiredPermissions are the permissions the cluster needs from the app, with
// the level each is needed at:
//
//   - contents:read  — `go mod download` against private modules and checkout;
//   - packages:read  — pulling private GHCR images.
const (
	PermContentsRead = "contents:read"
	PermPackagesRead = "packages:read"
)

// RequiredPermissionSet is RequiredPermissions in the shape Permissions.Missing
// takes.
var RequiredPermissionSet = map[string]string{
	"contents": "read",
	"packages": "read",
}

// Local is the control-plane adapter: it reads the encrypted app registration
// from the store, decrypts the key, and owns the cluster's single Minter so a
// fan-out of pulls mints one token.
//
// It also implements the broker's SecretReader: each repository's HMAC secret
// is stored encrypted and read only on a mint request.
type Local struct {
	Store  *store.Store
	Cipher *credentials.Cipher
	Client *Client
	// RegistryBase overrides DefaultRegistryBase (tests).
	RegistryBase string
	Log          *zerolog.Logger

	mu          sync.Mutex
	minter      *Minter
	fingerprint string
}

// TokenSource makes Local usable as the broker's minter.
var _ TokenSource = (*Local)(nil)

// Save encrypts and stores the app registration. An empty privateKeyPEM keeps
// the existing key (rotation of ids/probe targets without re-uploading the key).
//
// This is the ONE write path: the CLI's `github app set`, the console's set and
// rotate route and `pmcluster setup` all end here, so the encryption, the
// fingerprint and the minter invalidation cannot drift between them.
func (l *Local) Save(ctx context.Context, appID, installationID int64, privateKeyPEM []byte, probeRepo, probePackage string) error {
	if appID <= 0 {
		return fmt.Errorf("%w: app id must be positive", ErrBadKey)
	}
	if installationID < 0 {
		return fmt.Errorf("%w: installation id cannot be negative", ErrInstallationNotFound)
	}
	existing, err := l.Store.GitHubApp(ctx)
	if err != nil && !errors.Is(err, store.ErrGitHubAppNotConfigured) {
		return err
	}
	if len(privateKeyPEM) > 0 {
		if _, err := NewApp(appID, privateKeyPEM); err != nil {
			return err
		}
	} else {
		if existing == nil {
			return fmt.Errorf("%w: --key-file is required for the first configuration", ErrBadKey)
		}
		privateKeyPEM, err = l.decryptKey(existing)
		if err != nil {
			return err
		}
	}
	fingerprint, err := Fingerprint(privateKeyPEM)
	if err != nil {
		return err
	}
	ciphertext, err := l.Cipher.Encrypt(privateKeyPEM)
	if err != nil {
		return fmt.Errorf("encrypt app private key: %w", err)
	}
	row := &store.GitHubApp{
		AppID:                appID,
		InstallationID:       installationID,
		PrivateKeyCiphertext: ciphertext,
		ProbeRepo:            strings.TrimSpace(probeRepo),
		ProbePackage:         strings.TrimSpace(probePackage),
		KeyFingerprint:       fingerprint,
	}
	// The last probe stays attached to the targets it was made against: a save
	// that moves the probe elsewhere must not show the old result as if it
	// described the new target.
	if existing != nil && existing.ProbeRepo == row.ProbeRepo && existing.ProbePackage == row.ProbePackage {
		row.LastProbeOK = existing.LastProbeOK
		row.LastProbeDetail = existing.LastProbeDetail
		row.LastProbeAt = existing.LastProbeAt
	}
	if err := l.Store.SetGitHubApp(ctx, row); err != nil {
		return err
	}
	// A changed registration must not serve the old installation's token.
	l.invalidate()
	return nil
}

// RecordProbe stores the outcome of the last mint-and-probe so the console can
// show what was proven and when. Failure to record it is reported to the
// caller but never undoes a registration that already validated.
func (l *Local) RecordProbe(ctx context.Context, ok bool, detail string) error {
	row, err := l.Store.GitHubApp(ctx)
	if err != nil {
		return err
	}
	row.LastProbeOK = ok
	row.LastProbeDetail = strings.TrimSpace(detail)
	row.LastProbeAt = time.Now().Unix()
	return l.Store.SetGitHubApp(ctx, row)
}

// Clear removes the app registration. Broker secrets are left alone on purpose:
// they are per-repo revocations, not app state, and clearing both at once would
// hide which one an operator meant.
func (l *Local) Clear(ctx context.Context) error {
	if err := l.Store.DeleteGitHubApp(ctx); err != nil {
		return err
	}
	l.invalidate()
	return nil
}

// Configured reports whether an app registration exists.
func (l *Local) Configured(ctx context.Context) bool {
	_, err := l.Store.GitHubApp(ctx)
	return err == nil
}

// Token implements TokenSource through the cached minter.
func (l *Local) Token(ctx context.Context) (Token, error) {
	m, err := l.Minter(ctx)
	if err != nil {
		return Token{}, err
	}
	return m.Token(ctx)
}

// Minter returns the cached Minter for the current registration, rebuilding it
// only when the app id, installation id or key changed (a CLI edit while the
// daemon is running).
func (l *Local) Minter(ctx context.Context) (*Minter, error) {
	row, err := l.Store.GitHubApp(ctx)
	if err != nil {
		if errors.Is(err, store.ErrGitHubAppNotConfigured) {
			return nil, ErrNoApp
		}
		return nil, err
	}
	plain, err := l.decryptKey(row)
	if err != nil {
		return nil, err
	}
	app, err := NewApp(row.AppID, plain)
	if err != nil {
		return nil, err
	}
	fp := fmt.Sprintf("%d/%d/%x", row.AppID, row.InstallationID, sha256Sum(plain))

	l.mu.Lock()
	defer l.mu.Unlock()
	if l.minter != nil && l.fingerprint == fp && l.minter.InstallationID == row.InstallationID {
		return l.minter, nil
	}
	l.minter = &Minter{
		Client:         l.Client,
		App:            app,
		InstallationID: row.InstallationID,
		Log:            l.Log,
	}
	l.fingerprint = fp
	return l.minter, nil
}

// invalidate drops the cached minter after a credential change.
func (l *Local) invalidate() {
	l.mu.Lock()
	l.minter = nil
	l.fingerprint = ""
	l.mu.Unlock()
}

func (l *Local) decryptKey(row *store.GitHubApp) ([]byte, error) {
	plain, err := l.Cipher.Decrypt(row.PrivateKeyCiphertext)
	if err != nil {
		return nil, fmt.Errorf("%w: stored private key does not decrypt (the encryption key changed?): %v", ErrBadKey, err)
	}
	return plain, nil
}

// ---- broker secrets -------------------------------------------------------

// CreateBrokerSecret generates a 32-byte hex secret for repo, stores it
// encrypted, and returns the plaintext exactly once.
func (l *Local) CreateBrokerSecret(ctx context.Context, repo, description string) (string, error) {
	if err := validateRepo(repo); err != nil {
		return "", err
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate broker secret: %w", err)
	}
	plain := hex.EncodeToString(buf)
	ciphertext, err := l.Cipher.Encrypt([]byte(plain))
	if err != nil {
		return "", fmt.Errorf("encrypt broker secret: %w", err)
	}
	row := &store.GitHubBrokerSecret{Repo: repo, SecretCiphertext: ciphertext, Description: description}
	if err := l.Store.CreateGitHubBrokerSecret(ctx, row); err != nil {
		if errors.Is(err, store.ErrGitHubBrokerSecretExists) {
			return "", fmt.Errorf("%w: %s (remove it first to rotate)", err, repo)
		}
		return "", err
	}
	return plain, nil
}

// RotateBrokerSecret replaces an existing repo secret and returns the new
// plaintext once.
func (l *Local) RotateBrokerSecret(ctx context.Context, repo, description string) (string, error) {
	if err := validateRepo(repo); err != nil {
		return "", err
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate broker secret: %w", err)
	}
	plain := hex.EncodeToString(buf)
	ciphertext, err := l.Cipher.Encrypt([]byte(plain))
	if err != nil {
		return "", fmt.Errorf("encrypt broker secret: %w", err)
	}
	row := &store.GitHubBrokerSecret{Repo: repo, SecretCiphertext: ciphertext, Description: description}
	if err := l.Store.UpdateGitHubBrokerSecret(ctx, row); err != nil {
		return "", err
	}
	return plain, nil
}

// ListBrokerSecrets returns repo names, descriptions and last use — never the
// secret material.
func (l *Local) ListBrokerSecrets(ctx context.Context) ([]*store.GitHubBrokerSecret, error) {
	return l.Store.ListGitHubBrokerSecrets(ctx)
}

// DeleteBrokerSecret revokes one repo's broker access.
func (l *Local) DeleteBrokerSecret(ctx context.Context, repo string) error {
	return l.Store.DeleteGitHubBrokerSecret(ctx, repo)
}

// BrokerSecret implements the broker's SecretReader.
func (l *Local) BrokerSecret(ctx context.Context, repo string) ([]byte, error) {
	row, err := l.Store.GitHubBrokerSecret(ctx, repo)
	if err != nil {
		if errors.Is(err, store.ErrGitHubBrokerSecretNotFound) {
			return nil, err
		}
		return nil, err
	}
	plain, err := l.Cipher.Decrypt(row.SecretCiphertext)
	if err != nil {
		return nil, fmt.Errorf("decrypt broker secret for %s: %w", repo, err)
	}
	return plain, nil
}

// MarkBrokerSecretUsed records a successful mint (best-effort).
func (l *Local) MarkBrokerSecretUsed(ctx context.Context, repo string) error {
	return l.Store.MarkGitHubBrokerSecretUsed(ctx, repo)
}

// ---- status / diagnosis ---------------------------------------------------

// Status is what the console card and the CLI show: ids and granted
// permissions, never key material.
type Status struct {
	Configured     bool              `json:"configured"`
	AppID          int64             `json:"app_id"`
	InstallationID int64             `json:"installation_id"`
	ProbeRepo      string            `json:"probe_repo"`
	ProbePackage   string            `json:"probe_package"`
	Slug           string            `json:"slug"`
	Permissions    map[string]string `json:"permissions"`
	// Warning names a missing permission or an unreadable permission set. Empty
	// means the app reports both permissions the cluster needs.
	Warning string `json:"warning"`
	// KeyFingerprint identifies the installed private key without carrying it:
	// the console shows it before an operator overwrites the key, and compares
	// it afterwards to say whether the key actually changed.
	KeyFingerprint string `json:"key_fingerprint"`
	// LastProbe* are the outcome and time of the last mint-and-probe (a
	// console save or the Test button). LastProbeAt is Unix seconds, 0 = never.
	LastProbeOK     bool   `json:"last_probe_ok"`
	LastProbeDetail string `json:"last_probe_detail"`
	LastProbeAt     int64  `json:"last_probe_at"`
}

// Status reads the registration and asks GitHub for the granted permissions.
// A permissions read failure is reported in Warning rather than as an error:
// the ids are still worth showing when the network or the key is the problem.
func (l *Local) Status(ctx context.Context) (*Status, error) {
	row, err := l.Store.GitHubApp(ctx)
	if err != nil {
		if errors.Is(err, store.ErrGitHubAppNotConfigured) {
			return &Status{Configured: false, Permissions: map[string]string{}}, nil
		}
		return nil, err
	}
	st := &Status{
		Configured:      true,
		AppID:           row.AppID,
		InstallationID:  row.InstallationID,
		ProbeRepo:       row.ProbeRepo,
		ProbePackage:    row.ProbePackage,
		Permissions:     map[string]string{},
		LastProbeOK:     row.LastProbeOK,
		LastProbeDetail: row.LastProbeDetail,
		LastProbeAt:     row.LastProbeAt,
	}
	plain, err := l.decryptKey(row)
	if err != nil {
		st.Warning = err.Error()
		return st, nil
	}
	// A row written before the fingerprint column existed (or by a hand-edited
	// database) still gets one: it is derived from the key, not stored state.
	st.KeyFingerprint = row.KeyFingerprint
	if st.KeyFingerprint == "" {
		if fp, ferr := Fingerprint(plain); ferr == nil {
			st.KeyFingerprint = fp
		}
	}
	app, err := NewApp(row.AppID, plain)
	if err != nil {
		st.Warning = err.Error()
		return st, nil
	}
	perms, err := l.Client.Permissions(ctx, app)
	if err != nil {
		st.Warning = "could not read granted permissions: " + err.Error()
		return st, nil
	}
	st.Slug = perms.Slug
	st.Permissions = perms.Permissions
	if missing := perms.Missing(RequiredPermissionSet); len(missing) > 0 {
		st.Warning = "the app is missing " + strings.Join(missing, ", ") +
			" (update the App's permissions in GitHub, then approve them on the installation)"
	}
	return st, nil
}

// TestResult is the console Test button's answer.
type TestResult struct {
	OK        bool      `json:"ok"`
	Message   string    `json:"message"`
	ExpiresAt time.Time `json:"-"`
}

// Test mints a token and runs the same probes doctor uses, collapsing the
// outcome into a sentence the card can show. The outcome is recorded so the
// console can say when the pairing was last proven.
func (l *Local) Test(ctx context.Context) TestResult {
	detail, expires, err := l.Diagnose(ctx)
	var res TestResult
	switch {
	case errors.Is(err, ErrNoProbeTarget):
		res = TestResult{OK: true, ExpiresAt: expires,
			Message: "Minted an installation token. Set a probe repo/package (`pmcluster github app set --probe-repo … --probe-package …`) to also prove read access."}
	case err != nil:
		res = TestResult{OK: false, Message: err.Error()}
	default:
		res = TestResult{OK: true, Message: detail, ExpiresAt: expires}
	}
	// ok=false for "no probe target": the mint worked, the read did not happen,
	// and the card's age line must not claim a read that never ran.
	if recErr := l.RecordProbe(ctx, err == nil, res.Message); recErr != nil {
		_ = recErr // a bookkeeping failure must not fail the answer
	}
	return res
}

// Diagnose mints a token and asserts what it can:
//
//   - the mint itself (always);
//   - a repository read, when probe_repo is configured (contents:read);
//   - a GHCR manifest HEAD, when probe_package is configured (packages:read).
//
// It returns the minted token's expiry so callers can report the real lifetime.
// Errors wrap the sentinels above so doctor can name the cause.
func (l *Local) Diagnose(ctx context.Context) (string, time.Time, error) {
	tok, err := l.Token(ctx)
	if err != nil {
		return "", time.Time{}, err
	}
	row, err := l.Store.GitHubApp(ctx)
	if err != nil {
		return "", tok.ExpiresAt, err
	}
	repo := strings.TrimSpace(row.ProbeRepo)
	pkg := strings.TrimSpace(row.ProbePackage)
	if repo == "" && pkg == "" {
		return "", tok.ExpiresAt, ErrNoProbeTarget
	}

	var checks []string
	if repo != "" {
		if err := l.probeRepo(ctx, tok.Token, repo); err != nil {
			return "", tok.ExpiresAt, err
		}
		checks = append(checks, "read "+repo)
	}
	if pkg != "" {
		if err := l.probePackage(ctx, tok.Token, pkg); err != nil {
			return "", tok.ExpiresAt, err
		}
		checks = append(checks, "HEAD package "+pkg)
	}
	return fmt.Sprintf("minted a token valid until %s; %s OK",
		tok.ExpiresAt.UTC().Format(time.RFC3339), strings.Join(checks, ", ")), tok.ExpiresAt, nil
}

// probeRepo lists one repository's root contents with the installation token.
// It reads `/contents/`, not `/repos/{repo}`: the repository object is visible
// with Metadata: read alone (always granted), so it would pass without
// Contents: read and the check would prove the wrong thing.
func (l *Local) probeRepo(ctx context.Context, token, repo string) error {
	if err := validateRepo(repo); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, l.Client.apiBase()+"/repos/"+repo+"/contents/", nil)
	if err != nil {
		return fmt.Errorf("build repo probe: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := l.Client.httpClient().Do(req)
	if err != nil {
		return fmt.Errorf("repo probe %s: %w", repo, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	switch resp.StatusCode {
	case http.StatusOK:
		return nil
	case http.StatusUnauthorized:
		return fmt.Errorf("%w: the installation token was refused while reading %s", ErrTokenRejected, repo)
	case http.StatusForbidden:
		return fmt.Errorf("%w: no contents:read on %s — grant the App Contents: Read and approve it on the installation", ErrInsufficientPermission, repo)
	case http.StatusNotFound:
		return fmt.Errorf("%w: %s is not visible to the installation (repository selection is not 'all', or the name is wrong)", ErrInstallationNotFound, repo)
	default:
		return fmt.Errorf("repo probe %s: %s", repo, httpErrorText(resp.StatusCode, body))
	}
}

// probePackage sends a manifest HEAD to GHCR with the installation token as
// basic auth (user x-access-token), which is exactly how `docker login` uses
// it. pkg is "owner/name:tag" (tag defaults to latest).
func (l *Local) probePackage(ctx context.Context, token, pkg string) error {
	name, ref := pkg, "latest"
	if i := strings.LastIndex(pkg, ":"); i > strings.LastIndex(pkg, "/") {
		name, ref = pkg[:i], pkg[i+1:]
	}
	if name == "" || ref == "" {
		return fmt.Errorf("%w: probe_package must look like owner/name:tag", ErrNoProbeTarget)
	}
	base := l.RegistryBase
	if base == "" {
		base = DefaultRegistryBase
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodHead,
		strings.TrimRight(base, "/")+"/v2/"+name+"/manifests/"+ref, nil)
	if err != nil {
		return fmt.Errorf("build package probe: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.list.v2+json, application/vnd.oci.image.manifest.v1+json")
	req.SetBasicAuth("x-access-token", token)
	resp, err := l.Client.httpClient().Do(req)
	if err != nil {
		return fmt.Errorf("package probe %s: %w", pkg, err)
	}
	defer func() { _ = resp.Body.Close() }()
	switch resp.StatusCode {
	case http.StatusOK:
		return nil
	case http.StatusUnauthorized:
		return fmt.Errorf("%w: GHCR refused the installation token for %s", ErrTokenRejected, pkg)
	case http.StatusForbidden:
		return fmt.Errorf("%w: no packages:read for %s — grant the App Packages: Read and approve it on the installation", ErrInsufficientPermission, pkg)
	case http.StatusNotFound:
		return fmt.Errorf("%w: package %s not found (wrong name/tag, or the installation cannot see it)", ErrInstallationNotFound, pkg)
	default:
		return fmt.Errorf("package probe %s: GHCR returned HTTP %d", pkg, resp.StatusCode)
	}
}

// validateRepo accepts only "owner/name": the broker secret key and the probe
// repo both end up in URLs, so a path-like or spaced value is refused rather
// than escaped.
func validateRepo(repo string) error {
	parts := strings.Split(repo, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return fmt.Errorf("repository must be owner/name, got %q", repo)
	}
	for _, p := range parts {
		if strings.ContainsAny(p, " \t\r\n?#") {
			return fmt.Errorf("repository %q contains characters that do not belong in a repo name", repo)
		}
	}
	return nil
}

// SortPermissions renders a permission map deterministically for messages.
func SortPermissions(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k, v := range m {
		out = append(out, k+":"+v)
	}
	sort.Strings(out)
	return out
}

func sha256Sum(b []byte) []byte {
	sum := sha256.Sum256(b)
	return sum[:]
}
