package cluster

import (
	"context"
	"fmt"

	"github.com/hazemarian/poor-man-stack/pmcluster/internal/auth"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/credentials"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/docker"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/store"
)

// CredentialKind tags a managed credential with its consumer; drives the
// secret's payload format (plain vs htpasswd).
type CredentialKind string

const (
	KindTraefikAdmin CredentialKind = "traefik"
	KindPortainer    CredentialKind = "portainer"
	KindOpenObserve  CredentialKind = "openobserve"
	// KindEdge marks credentials consumed by the pmcluster-edge console (the
	// operator UI). Unlike the other kinds these are never force-restarted by
	// rotate: the console persists them once, so Swarm secrets only matter for
	// fresh provisioned volumes.
	KindEdge CredentialKind = "edge"
)

// ManagedCredential is the in-memory shape of a bootstrap credential. The
// two "Created" flags diverge only in the "lost DB, kept Swarm" recovery
// scenario (fresh DB but a Swarm secret with that name already existed).
type ManagedCredential struct {
	Name               string
	Kind               CredentialKind
	Username           string
	Password           string
	SwarmSecretName    string
	NewlyCreated       bool
	SwarmSecretCreated bool
	UsernameChanged    bool // true when username was updated on this bootstrap
}

type BootstrapInput struct {
	TraefikAdminUser      string
	OpenObserveAdminEmail string
}

type CredentialsManager struct {
	Store    *store.Store
	Cipher   *credentials.Cipher
	Docker   docker.Client
	Deployer StackDeployer // only required for Rotate's force-restart step
}

// consumingService maps each managed credential to the swarm service that
// mounts its secret. Names are "<stack>_<service>" — what `docker stack
// deploy` produces. Keep in sync with the bundled stacks.
var consumingService = map[string]string{
	"traefik_dashboard": "infra_traefik",
	"portainer":         "infra_portainer",
	"openobserve_admin": "observability_openobserve",
}

// Bootstrap ensures every bundled component has a credential. Idempotent:
// re-runs preserve existing values, only missing entries are created.
// Returned plaintext passwords are decrypted from the store on re-runs so
// downstream renderers (e.g. the OTel config's basic-auth header) work.
func (m *CredentialsManager) Bootstrap(ctx context.Context, in BootstrapInput) (map[string]*ManagedCredential, error) {
	if in.TraefikAdminUser == "" {
		in.TraefikAdminUser = "admin"
	}
	if in.OpenObserveAdminEmail == "" {
		return nil, fmt.Errorf("OpenObserve admin email is required (use --openobserve-email or set in config)")
	}

	usernameFor := map[string]string{
		"traefik_dashboard": in.TraefikAdminUser,
		"portainer":         "admin",
		"openobserve_admin": in.OpenObserveAdminEmail,
		"edge_admin":        "admin",
		"edge_ui_secret":    "session",
		"edge_api_token":    "edge",
	}
	specs := bootstrapSpecs()
	for i := range specs {
		specs[i].username = usernameFor[specs[i].name]
	}

	out := make(map[string]*ManagedCredential, len(specs))
	for _, spec := range specs {
		mc, err := m.ensure(ctx, spec)
		if err != nil {
			return nil, fmt.Errorf("bootstrap %s: %w", spec.name, err)
		}
		out[spec.name] = mc
	}
	return out, nil
}

type secretFormat int

const (
	formatPlain    secretFormat = iota // raw password bytes
	formatHtpasswd                     // "user:bcrypt(password)\n"
)

type bootstrapSpec struct {
	name            string
	kind            CredentialKind
	username        string
	swarmSecretName string
	format          secretFormat

	// generate, when set, replaces RandomPassword as the fresh-mint value
	// source. Used by edge_api_token, whose value must be a real daemon Bearer
	// token (minted via auth.GenerateToken + inserted as a users row) rather
	// than an arbitrary password. nil means mint via RandomPassword.
	generate func(ctx context.Context, s *store.Store) (string, error)
}

// ensure is the get-or-create primitive: load from store and reconcile
// the matching Swarm secret, or mint a fresh password when missing.
//
// When the username changes for an OpenObserve credential, the password
// is also rotated.  OpenObserve only reads ZO_ROOT_USER_* on first boot;
// changing just the email would leave the container with a stale root
// user.  A fresh password + email pair ensures the compose template
// changes, triggering a service update, and the new env vars take effect
// once the data volume is reset (handled by the caller in up.go).
func (m *CredentialsManager) ensure(ctx context.Context, spec bootstrapSpec) (*ManagedCredential, error) {
	existing, err := m.Store.GetCredential(ctx, spec.name)
	if err == nil {
		username := existing.Username
		usernameChanged := spec.username != "" && spec.username != existing.Username
		if usernameChanged {
			username = spec.username
		}

		plaintext, err := m.Cipher.Decrypt(existing.PasswordCiphertext)
		if err != nil {
			return nil, fmt.Errorf("decrypt %s: %w", spec.name, err)
		}

		// OpenObserve ignores env-vars after first boot, so when the
		// email changes we must also rotate the password.  The caller
		// (up.go) resets the data volume so the new pair takes effect.
		if usernameChanged && spec.kind == KindOpenObserve {
			newPass, err := RandomPassword()
			if err != nil {
				return nil, fmt.Errorf("rotate password for %s: %w", spec.name, err)
			}
			ciphertext, err := m.Cipher.Encrypt([]byte(newPass))
			if err != nil {
				return nil, fmt.Errorf("encrypt new password: %w", err)
			}
			if err := m.Store.RotateCredential(ctx, spec.name, ciphertext); err != nil {
				return nil, fmt.Errorf("rotate credential %s: %w", spec.name, err)
			}
			if err := m.Store.UpdateCredentialUsername(ctx, spec.name, username); err != nil {
				return nil, fmt.Errorf("update username for %s: %w", spec.name, err)
			}
			plaintext = []byte(newPass)
		}

		secretPayload, err := serialisePassword(spec, username, string(plaintext))
		if err != nil {
			return nil, err
		}
		secretCreated, err := EnsureSecret(ctx, m.Docker, existing.SwarmSecretName, secretPayload)
		if err != nil {
			return nil, fmt.Errorf("ensure swarm secret %s: %w", existing.SwarmSecretName, err)
		}

		// Persist the updated username to the store if it changed (and
		// we haven't already via the rotate path above).
		if usernameChanged && spec.kind != KindOpenObserve {
			if updateErr := m.Store.UpdateCredentialUsername(ctx, spec.name, username); updateErr != nil {
				return nil, fmt.Errorf("update username for %s: %w", spec.name, updateErr)
			}
		}

		return &ManagedCredential{
			Name:               existing.Name,
			Kind:               CredentialKind(existing.Kind),
			Username:           username,
			Password:           string(plaintext),
			SwarmSecretName:    existing.SwarmSecretName,
			NewlyCreated:       false,
			SwarmSecretCreated: secretCreated,
			UsernameChanged:    usernameChanged,
		}, nil
	}
	if err != store.ErrCredentialNotFound {
		return nil, fmt.Errorf("lookup %s: %w", spec.name, err)
	}

	password, err := spec.generatePassword(ctx, m.Store)
	if err != nil {
		return nil, fmt.Errorf("generate value for %s: %w", spec.name, err)
	}
	ciphertext, err := m.Cipher.Encrypt([]byte(password))
	if err != nil {
		return nil, fmt.Errorf("encrypt: %w", err)
	}
	if err := m.Store.InsertCredential(ctx, &store.ManagedCredential{
		Name:               spec.name,
		Kind:               string(spec.kind),
		Username:           spec.username,
		PasswordCiphertext: ciphertext,
		SwarmSecretName:    spec.swarmSecretName,
	}); err != nil {
		return nil, fmt.Errorf("insert credential %s: %w", spec.name, err)
	}
	secretPayload, err := serialisePassword(spec, spec.username, password)
	if err != nil {
		return nil, err
	}
	secretCreated, err := EnsureSecret(ctx, m.Docker, spec.swarmSecretName, secretPayload)
	if err != nil {
		return nil, fmt.Errorf("create swarm secret %s: %w", spec.swarmSecretName, err)
	}
	return &ManagedCredential{
		Name:               spec.name,
		Kind:               spec.kind,
		Username:           spec.username,
		Password:           password,
		SwarmSecretName:    spec.swarmSecretName,
		NewlyCreated:       true,
		SwarmSecretCreated: secretCreated,
	}, nil
}

// Rotate mints a new password, re-encrypts in store, swaps the Swarm
// secret, and force-restarts the consuming service. Returns plaintext
// (caller prints once and discards).
//
// If SecretRemove fails (typically: the secret is still mounted by a
// running service), the store has already been updated but Swarm still
// has the old value. Operator scales the consuming service to 0 and
// re-runs.
func (m *CredentialsManager) Rotate(ctx context.Context, name string) (*ManagedCredential, error) {
	// The OTel root token is set once and cached by OpenObserve on first
	// boot; rotating it breaks collector ingestion without a destructive
	// volume reset. Refuse instead of silently wedging the pipeline.
	if name == "openobserve_token" {
		return nil, fmt.Errorf("cannot rotate %q: OpenObserve caches this token in its data volume on first boot; rotating it would break collector ingestion without a volume reset (it is meant to stay fixed)", name)
	}
	// The edge API token is minted as a real daemon "edge" user (hash in the
	// users table). Rotating the stored value without rewriting the user row
	// would leave the daemon rejecting the console's Bearer. Refuse.
	if name == "edge_api_token" {
		return nil, fmt.Errorf("cannot rotate %q: it is the daemon Bearer token for the %q user; rotating it would break the edge console's API access (remove the %q user row and the credential, then re-run to re-provision instead)", name, edgeAPITokenUser, edgeAPITokenUser)
	}

	existing, err := m.Store.GetCredential(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("lookup %s: %w", name, err)
	}

	password, err := RandomPassword()
	if err != nil {
		return nil, fmt.Errorf("generate password: %w", err)
	}
	ciphertext, err := m.Cipher.Encrypt([]byte(password))
	if err != nil {
		return nil, fmt.Errorf("encrypt: %w", err)
	}
	if err := m.Store.RotateCredential(ctx, name, ciphertext); err != nil {
		return nil, fmt.Errorf("update credential row: %w", err)
	}

	spec, ok := specFor(name, existing)
	if !ok {
		return nil, fmt.Errorf("rotate %s: no spec for credential kind %q", name, existing.Kind)
	}
	payload, err := serialisePassword(spec, existing.Username, password)
	if err != nil {
		return nil, err
	}

	// Order matters: Docker refuses to remove a secret in use, so this
	// fails loud if the consuming service is still running.
	if err := m.Docker.SecretRemove(ctx, existing.SwarmSecretName); err != nil {
		return nil, fmt.Errorf("remove old swarm secret %s (is the consuming service still using it?): %w", existing.SwarmSecretName, err)
	}
	if _, err := EnsureSecret(ctx, m.Docker, existing.SwarmSecretName, payload); err != nil {
		return nil, fmt.Errorf("re-create swarm secret %s: %w", existing.SwarmSecretName, err)
	}

	// Best-effort: when no service mapping exists, the secret is still in
	// place and will be picked up on the next deploy.
	if svc, ok := consumingService[name]; ok && m.Deployer != nil {
		if err := m.Deployer.ForceUpdateService(ctx, svc); err != nil {
			return nil, fmt.Errorf("force-restart %s: %w (new secret IS in place; operator may restart manually)", svc, err)
		}
	}

	return &ManagedCredential{
		Name:               existing.Name,
		Kind:               CredentialKind(existing.Kind),
		Username:           existing.Username,
		Password:           password,
		SwarmSecretName:    existing.SwarmSecretName,
		NewlyCreated:       false,
		SwarmSecretCreated: true,
	}, nil
}

func specFor(name string, _ *store.ManagedCredential) (bootstrapSpec, bool) {
	for _, s := range bootstrapSpecs() {
		if s.name == name {
			return s, true
		}
	}
	return bootstrapSpec{}, false
}

// bootstrapSpecs is the canonical list of managed credentials, used by
// both Bootstrap and Rotate.
func bootstrapSpecs() []bootstrapSpec {
	return []bootstrapSpec{
		{
			name:            "traefik_dashboard",
			kind:            KindTraefikAdmin,
			swarmSecretName: "admin_credentials",
			format:          formatHtpasswd,
		},
		{
			name:            "portainer",
			kind:            KindPortainer,
			swarmSecretName: "portainer_admin_password",
			format:          formatPlain,
		},
		{
			name:            "openobserve_admin",
			kind:            KindOpenObserve,
			swarmSecretName: "zo_root_user_password",
			format:          formatPlain,
		},
		// Edge console credentials. pmcluster mints all three and mirrors them
		// to Swarm secrets the edge container mounts at /run/secrets/: the UI
		// login password, the session-cookie HMAC key, and the daemon API token.
		// The console stores them once on first boot; the secrets matter only
		// for fresh provisioned volumes (see internal/ui + cmd/edge).
		{
			name:            "edge_admin",
			kind:            KindEdge,
			swarmSecretName: "edge_admin_password",
			format:          formatPlain,
		},
		{
			name:            "edge_ui_secret",
			kind:            KindEdge,
			swarmSecretName: "edge_ui_secret",
			format:          formatPlain,
		},
		{
			name:            "edge_api_token",
			kind:            KindEdge,
			swarmSecretName: "edge_api_token",
			format:          formatPlain,
			// A real daemon Bearer token: minted via auth.GenerateToken and
			// registered as the "edge" user so the daemon accepts it.
			generate: ensureEdgeAPIToken,
		},
		// openobserve_user / openobserve_token are NOT bootstrap credentials:
		// they are created against the OpenObserve API by OpenObserveProvisioner
		// (automation admin user + dedicated ingestion token) and kept in the
		// pmcluster store/config, never mirrored to a Swarm secret or baked into
		// the OO data volume.
	}
}

// generatePassword returns the plaintext value to store for a freshly-minted
// spec: the spec's custom generator when set (e.g. a real daemon token),
// otherwise a random password.
func (spec bootstrapSpec) generatePassword(ctx context.Context, s *store.Store) (string, error) {
	if spec.generate != nil {
		return spec.generate(ctx, s)
	}
	return RandomPassword()
}

// edgeAPITokenUser is the daemon user row that owns the edge console's API
// token. The daemon authenticates the console's Bearer against this user.
const edgeAPITokenUser = "edge"

// ensureEdgeAPIToken mints a dedicated pmcluster API token for the edge console
// and registers it as the "edge" daemon user so the daemon accepts it. Called
// only on a fresh mint (missing managed credential); on re-runs the stored token
// is reused and its user row already exists.
func ensureEdgeAPIToken(ctx context.Context, s *store.Store) (string, error) {
	token, err := auth.GenerateToken()
	if err != nil {
		return "", fmt.Errorf("generate api token: %w", err)
	}
	tokenID, secret := auth.SplitToken(token)
	hash, err := auth.HashToken(secret)
	if err != nil {
		return "", fmt.Errorf("hash api token: %w", err)
	}
	if _, err := s.CreateUser(ctx, edgeAPITokenUser, tokenID, hash); err != nil {
		if err == store.ErrUserExists {
			return "", fmt.Errorf("daemon user %q already exists but the edge credential is missing (store+users diverged) — remove the %q user row from the store's users table or the %q credential, then re-run", edgeAPITokenUser, edgeAPITokenUser, "edge_api_token")
		}
		return "", fmt.Errorf("create %s user: %w", edgeAPITokenUser, err)
	}
	return token, nil
}

func serialisePassword(spec bootstrapSpec, username, password string) ([]byte, error) {
	switch spec.format {
	case formatPlain:
		return []byte(password), nil
	case formatHtpasswd:
		line, err := HtpasswdLine(username, password)
		if err != nil {
			return nil, err
		}
		return []byte(line), nil
	default:
		return nil, fmt.Errorf("unknown secret format %d", spec.format)
	}
}
