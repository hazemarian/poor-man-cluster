package githubapp

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
)

// This file is the ONE registration-change path the console, `pmcluster setup`
// and the CLI share. It exists because "store the key" is not the interesting
// part — proving the app id, the key and the installation id belong together
// before overwriting a working registration is. Nothing here reimplements the
// write: the last step calls Local.Save, the same function the CLI has always
// used, so encryption, the fingerprint and the minter invalidation are the
// daemon's, not a second copy of them.

// ProbeOutcome is what a mint-and-probe established. OK is false when no probe
// target was configured: the token was minted, the read never happened, and
// saying otherwise would be a lie the operator would discover in production.
type ProbeOutcome struct {
	OK     bool
	Detail string
}

// SetRequest is one registration change.
//
// Zero/empty means "keep what is stored": AppID and InstallationID are only
// replaced when positive, and an empty PrivateKeyPEM keeps the stored key so a
// rotation of ids or probe targets does not require re-uploading the .pem. The
// probe fields are pointers because "absent" (keep the stored targets) and
// "empty" (clear them) are different intentions; the console always submits
// both.
type SetRequest struct {
	AppID          int64
	InstallationID int64
	PrivateKeyPEM  []byte
	ProbeRepo      *string
	ProbePackage   *string
}

// SetResult is the answer the console renders: the ids, which key is installed
// (a fingerprint, never the key), whether the key changed, and what the probe
// proved.
type SetResult struct {
	AppID          int64
	InstallationID int64
	KeyFingerprint string
	Rotated        bool
	Probe          ProbeOutcome
}

// Apply validates a registration change against GitHub and only then persists
// it:
//
//  1. parse the PEM (PKCS#1 or PKCS#8, RSA only) — a wrong file stops here;
//  2. sign an app JWT and exchange it for an installation token for the NEW
//     installation id, proving the app id, the key and the installation
//     belong together (a bad installation id never overwrites a working one);
//  3. run the configured probe with that token (contents:read / packages:read);
//  4. persist through Local.Save and record the probe.
//
// A failure at any step leaves the stored registration untouched.
func (l *Local) Apply(ctx context.Context, req SetRequest) (*SetResult, error) {
	existing, err := l.Store.GitHubApp(ctx)
	if err != nil && !errors.Is(err, store.ErrGitHubAppNotConfigured) {
		return nil, err
	}

	appID := req.AppID
	if appID <= 0 && existing != nil {
		appID = existing.AppID
	}
	if appID <= 0 {
		return nil, fmt.Errorf("%w: app id is required", ErrBadKey)
	}

	keyPEM := req.PrivateKeyPEM
	if len(keyPEM) == 0 {
		if existing == nil {
			return nil, fmt.Errorf("%w: no key is stored yet, so the .pem has to be uploaded once", ErrKeyRequired)
		}
		keyPEM, err = l.decryptKey(existing)
		if err != nil {
			return nil, err
		}
	}

	installationID := req.InstallationID
	if installationID <= 0 && existing != nil {
		installationID = existing.InstallationID
	}
	if installationID <= 0 {
		return nil, fmt.Errorf("%w: an installation id is required here — the pairing is proven by minting for it (find it at github.com/organizations/<org>/settings/installations/<id>)", ErrInstallationNotFound)
	}

	repo, pkg := "", ""
	if existing != nil {
		repo, pkg = existing.ProbeRepo, existing.ProbePackage
	}
	if req.ProbeRepo != nil {
		repo = strings.TrimSpace(*req.ProbeRepo)
	}
	if req.ProbePackage != nil {
		pkg = strings.TrimSpace(*req.ProbePackage)
	}

	// 1. Parse. NewApp is the CLI's validator: PKCS#1 or PKCS#8, RSA only, and
	// the app id has to be positive.
	app, err := NewApp(appID, keyPEM)
	if err != nil {
		return nil, err
	}
	if l.Client == nil {
		return nil, errors.New("github app: no API client configured")
	}

	// 2. Sign a JWT (inside Client.Token) and exchange it for an installation
	// token. This is the step that proves the three values belong together, so
	// nothing is written before it returns.
	tok, err := l.Client.Token(ctx, app, installationID)
	if err != nil {
		return nil, err
	}

	// 3. Probe with the token just minted, not the cached one.
	var checks []string
	if repo != "" {
		if err := l.probeRepo(ctx, tok.Token, repo); err != nil {
			return nil, err
		}
		checks = append(checks, "read "+repo)
	}
	if pkg != "" {
		if err := l.probePackage(ctx, tok.Token, pkg); err != nil {
			return nil, err
		}
		checks = append(checks, "HEAD package "+pkg)
	}
	probe := ProbeOutcome{
		OK: len(checks) > 0,
		Detail: fmt.Sprintf("minted a token valid until %s; %s OK",
			tok.ExpiresAt.UTC().Format(time.RFC3339), strings.Join(checks, ", ")),
	}
	if len(checks) == 0 {
		probe.Detail = ErrNoProbeTarget.Error() + ": the installation token was minted, but no read was proven"
	}

	fingerprint, err := Fingerprint(keyPEM)
	if err != nil {
		return nil, err
	}
	rotated := existing == nil || existing.KeyFingerprint != fingerprint

	// 4. Persist through the CLI's own path.
	if err := l.Save(ctx, appID, installationID, keyPEM, repo, pkg); err != nil {
		return nil, err
	}
	if err := l.RecordProbe(ctx, probe.OK, probe.Detail); err != nil {
		return nil, fmt.Errorf("record probe result: %w", err)
	}

	l.logf("github app registration updated: app_id=%d installation_id=%d key_rotated=%t probe_ok=%t",
		appID, installationID, rotated, probe.OK)

	return &SetResult{
		AppID:          appID,
		InstallationID: installationID,
		KeyFingerprint: fingerprint,
		Rotated:        rotated,
		Probe:          probe,
	}, nil
}

// logf writes one audit line when a logger is wired. It formats with %s/%d only:
// callers must never pass key material here, and no argument in this file is
// ever a PEM.
func (l *Local) logf(format string, args ...any) {
	if l.Log == nil {
		return
	}
	l.Log.Info().Msg(fmt.Sprintf(format, args...))
}

// Fingerprint identifies a private key without carrying it: SHA-256 over the
// DER-encoded public half, the first 128 bits rendered as colon-separated hex
// (the shape `ssh-keygen -lf` prints). It is stable for the same key on every
// call and different for another key, which is what makes a rotation something
// the console can show instead of assert.
func Fingerprint(pemBytes []byte) (string, error) {
	key, err := parseRSAKey(pemBytes)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrBadKey, err)
	}
	der, err := x509.MarshalPKIXPublicKey(&key.key.PublicKey)
	if err != nil {
		return "", fmt.Errorf("%w: encode public key: %v", ErrBadKey, err)
	}
	sum := sha256.Sum256(der)
	hexed := hex.EncodeToString(sum[:16])
	var b strings.Builder
	b.WriteString("SHA256:")
	for i := 0; i < len(hexed); i += 2 {
		if i > 0 {
			b.WriteByte(':')
		}
		b.WriteString(hexed[i : i+2])
	}
	return b.String(), nil
}
