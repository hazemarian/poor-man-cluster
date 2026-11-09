package githubapp

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"time"
)

// JWT claim window, from GitHub's documentation: `exp` must be at most ten
// minutes in the future, and `iat` is backdated 30 seconds to absorb clock
// drift between this host and GitHub.
const (
	jwtBackdate = 30 * time.Second
	jwtLifetime = 9 * time.Minute
)

// rsaKey wraps the private key so the key cannot be printed or marshalled by
// accident: %v on App prints only the id.
type rsaKey struct{ key *rsa.PrivateKey }

// parseRSAKey accepts PKCS#1 ("RSA PRIVATE KEY") and PKCS#8 ("PRIVATE KEY")
// PEM blocks, which are the two shapes GitHub hands out.
func parseRSAKey(pemBytes []byte) (*rsaKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, errors.New("no PEM block found (expected the .pem downloaded from the GitHub App settings)")
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return &rsaKey{key: key}, nil
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse private key as PKCS#1 or PKCS#8: %w", err)
	}
	key, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("private key is %T, not RSA", parsed)
	}
	return &rsaKey{key: key}, nil
}

// signJWT builds a GitHub App JWT: RS256 over the standard header and the
// {iat, exp, iss} claims.
func signJWT(app *App, now time.Time) (string, error) {
	if app == nil || app.key == nil || app.key.key == nil {
		return "", errors.New("sign app JWT: no private key loaded")
	}
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	claims, err := json.Marshal(map[string]any{
		"iat": now.Add(-jwtBackdate).Unix(),
		"exp": now.Add(jwtLifetime).Unix(),
		"iss": app.ID,
	})
	if err != nil {
		return "", fmt.Errorf("marshal app JWT claims: %w", err)
	}
	signingInput := header + "." + base64.RawURLEncoding.EncodeToString(claims)

	digest := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, app.key.key, crypto.SHA256, digest[:])
	if err != nil {
		return "", fmt.Errorf("sign app JWT: %w", err)
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}
