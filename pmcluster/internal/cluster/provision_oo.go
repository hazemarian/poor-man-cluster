package cluster

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/hazemarian/poor-man-stack/pmcluster/internal/credentials"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/openobserve"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/store"
)

// provisionRetryAttempts / provisionRetryDelay bound how long EnsureUserAndToken
// keeps retrying OpenObserve API calls. On a FRESH `cluster up` there is a
// window where OpenObserve is healthy (its container is up) but Traefik has not
// yet registered the router for observ.<domain> — the very first provisioning
// call then answers "404 page not found" (Traefik's no-router response). Once
// the router is registered the call succeeds, so a short bounded retry turns a
// spurious fresh-cluster-up failure into a success. In steady state (long-
// running OpenObserve) the first attempt just works.
const (
	provisionRetryAttempts = 10
	provisionRetryDelay    = 2 * time.Second
)

// provisionedOpenObserveCreds are the managed credentials the provisioner owns.
// Unlike the bootstrap credentials in bootstrapSpecs(), these are NOT mirrored
// to Swarm secrets: they are created against OpenObserve's API and only needed
// to render pmcluster-owned configs (the collector's basic-auth header). Pass +
// token stay in the pmcluster store/config — never baked into the OO volume.
const (
	credOpenObserveUser  = "openobserve_user"
	credOpenObserveToken = "openobserve_token"

	// pendingIngestionToken is a placeholder rendered into the OTel collector
	// config during the FIRST deploy pass of a fresh install, when the real
	// ingestion token does not exist yet (OpenObserve must be up before the API
	// can mint it). After provisioning, the config is re-rendered with the real
	// token and observability is re-deployed — so this is transient only.
	pendingIngestionToken = "o2oi_pending-provisioning"

	ingestionTokenName      = "pmcluster" // name of the dedicated OO ingestion token
	automationUserLocalPart = "automation"
)

// OpenObserveProvisioner creates (idempotently) an OpenObserve automation user
// and a dedicated ingestion token, persisting them as separate managed
// credentials so the collector can authenticate ingestion using the token.
type OpenObserveProvisioner struct {
	Store  *store.Store
	Cipher *credentials.Cipher
	Client *openobserve.Client
	Stdout io.Writer
	Org    string // OpenObserve org (default "default")
}

func (p *OpenObserveProvisioner) org() string {
	if p.Org != "" {
		return p.Org
	}
	return "default"
}

func (p *OpenObserveProvisioner) printf(format string, args ...any) {
	if p.Stdout != nil {
		fmt.Fprintf(p.Stdout, format, args...)
	}
}

// EnsureUserAndToken provisions the automation user and the ingestion token if
// they are missing, and returns the stored credentials for "openobserve_user"
// and "openobserve_token". Idempotent: on re-runs (e.g. `cluster update`, or an
// interrupted `cluster up`) any rows already present are reused untouched, so a
// password rotation never re-mints the token and never changes the collector
// config — the whole point of the decoupling.
func (p *OpenObserveProvisioner) EnsureUserAndToken(ctx context.Context) (userCred, tokenCred *store.ManagedCredential, err error) {
	var lastErr error
	for attempt := 1; attempt <= provisionRetryAttempts; attempt++ {
		if attempt > 1 {
			p.printf("  ⏳ OpenObserve not ready yet (attempt %d/%d)…\n", attempt, provisionRetryAttempts)
			select {
			case <-ctx.Done():
				return nil, nil, fmt.Errorf("provision OpenObserve: %w", ctx.Err())
			case <-time.After(provisionRetryDelay):
			}
		}
		userCred, tokenCred, lastErr = p.ensureUserAndTokenOnce(ctx)
		if lastErr == nil {
			return userCred, tokenCred, nil
		}
	}
	return nil, nil, fmt.Errorf("provision OpenObserve after %d attempts: %w", provisionRetryAttempts, lastErr)
}

// ensureUserAndTokenOnce performs a single provisioning pass. Idempotent: any
// rows already present are reused untouched (see EnsureUserAndToken).
func (p *OpenObserveProvisioner) ensureUserAndTokenOnce(ctx context.Context) (userCred, tokenCred *store.ManagedCredential, err error) {
	root, err := p.Store.GetCredential(ctx, "openobserve_admin")
	if err != nil {
		return nil, nil, fmt.Errorf("load openobserve_admin (run bootstrap first): %w", err)
	}
	rootPass, err := p.Cipher.Decrypt(root.PasswordCiphertext)
	if err != nil {
		return nil, nil, fmt.Errorf("decrypt openobserve_admin password: %w", err)
	}

	user, err := p.ensureUser(ctx, root.Username, string(rootPass))
	if err != nil {
		return nil, nil, err
	}
	token, err := p.ensureToken(ctx, root.Username, string(rootPass))
	if err != nil {
		return nil, nil, err
	}
	return user, token, nil
}

func (p *OpenObserveProvisioner) ensureUser(ctx context.Context, rootUser, rootPass string) (*store.ManagedCredential, error) {
	existing, err := p.Store.GetCredential(ctx, credOpenObserveUser)
	if err == nil {
		p.printf("  ✓ %-20s already provisioned\n", credOpenObserveUser)
		return existing, nil
	}
	if err != store.ErrCredentialNotFound {
		return nil, fmt.Errorf("lookup %s: %w", credOpenObserveUser, err)
	}

	email := automationEmail(rootUser)
	password, err := RandomPassword()
	if err != nil {
		return nil, fmt.Errorf("generate %s password: %w", credOpenObserveUser, err)
	}
	if err := p.Client.EnsureUser(ctx, rootUser, rootPass, openobserve.User{
		Email:     email,
		FirstName: "PMCluster",
		LastName:  "Automation",
		Password:  password,
		Role:      "admin",
	}); err != nil {
		return nil, fmt.Errorf("create OpenObserve user %s: %w", email, err)
	}

	ciphertext, err := p.Cipher.Encrypt([]byte(password))
	if err != nil {
		return nil, fmt.Errorf("encrypt %s password: %w", credOpenObserveUser, err)
	}
	created := &store.ManagedCredential{
		Name:               credOpenObserveUser,
		Kind:               string(KindOpenObserve),
		Username:           email,
		PasswordCiphertext: ciphertext,
	}
	if err := p.Store.InsertCredential(ctx, created); err != nil {
		return nil, fmt.Errorf("store %s: %w", credOpenObserveUser, err)
	}
	p.printf("  ✓ %-20s created (OpenObserve admin user %s)\n", credOpenObserveUser, email)
	return created, nil
}

func (p *OpenObserveProvisioner) ensureToken(ctx context.Context, rootUser, rootPass string) (*store.ManagedCredential, error) {
	existing, err := p.Store.GetCredential(ctx, credOpenObserveToken)
	if err == nil {
		p.printf("  ✓ %-20s already provisioned\n", credOpenObserveToken)
		return existing, nil
	}
	if err != store.ErrCredentialNotFound {
		return nil, fmt.Errorf("lookup %s: %w", credOpenObserveToken, err)
	}

	token, err := p.Client.EnsureIngestionToken(ctx, rootUser, rootPass, ingestionTokenName)
	if err != nil {
		return nil, fmt.Errorf("create OpenObserve ingestion token: %w", err)
	}

	ciphertext, err := p.Cipher.Encrypt([]byte(token))
	if err != nil {
		return nil, fmt.Errorf("encrypt %s: %w", credOpenObserveToken, err)
	}
	created := &store.ManagedCredential{
		Name:               credOpenObserveToken,
		Kind:               string(KindOpenObserve),
		Username:           p.org(), // OpenObserve authenticates ingestion as Basic <org>:<token>
		PasswordCiphertext: ciphertext,
	}
	if err := p.Store.InsertCredential(ctx, created); err != nil {
		return nil, fmt.Errorf("store %s: %w", credOpenObserveToken, err)
	}
	p.printf("  ✓ %-20s created (ingestion token for %s)\n", credOpenObserveToken, p.org())
	return created, nil
}

// automationEmail derives a deterministic automation-user email from the root
// admin email by keeping its domain: "admin@example.com" -> "automation@example.com".
func automationEmail(rootEmail string) string {
	_, domain, ok := strings.Cut(rootEmail, "@")
	if !ok || domain == "" {
		domain = "localhost"
	}
	return automationUserLocalPart + "@" + domain
}
