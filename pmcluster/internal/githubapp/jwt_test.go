package githubapp

import (
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"strings"
	"testing"
	"time"
)

func TestSignJWT_VerifiesAndCarriesGitHubClaims(t *testing.T) {
	pemBytes := testKeyPEM(t)
	app, err := NewApp(4242, pemBytes)
	if err != nil {
		t.Fatalf("NewApp: %v", err)
	}
	now := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)

	token, err := signJWT(app, now)
	if err != nil {
		t.Fatalf("signJWT: %v", err)
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("JWT has %d segments, want 3", len(parts))
	}

	header, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		t.Fatalf("decode header: %v", err)
	}
	if !strings.Contains(string(header), `"alg":"RS256"`) || !strings.Contains(string(header), `"typ":"JWT"`) {
		t.Errorf("header = %s, want RS256/JWT", header)
	}

	claims, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("decode claims: %v", err)
	}
	var got struct {
		Iat int64 `json:"iat"`
		Exp int64 `json:"exp"`
		Iss int64 `json:"iss"`
	}
	if err := json.Unmarshal(claims, &got); err != nil {
		t.Fatalf("unmarshal claims: %v", err)
	}
	if got.Iss != 4242 {
		t.Errorf("iss = %d, want 4242", got.Iss)
	}
	if got.Iat != now.Add(-30*time.Second).Unix() {
		t.Errorf("iat = %d, want now-30s (%d)", got.Iat, now.Add(-30*time.Second).Unix())
	}
	if got.Exp != now.Add(9*time.Minute).Unix() {
		t.Errorf("exp = %d, want now+9m (%d)", got.Exp, now.Add(9*time.Minute).Unix())
	}
	if got.Exp-got.Iat > 600 {
		t.Errorf("JWT window is %ds; GitHub rejects anything over 600s", got.Exp-got.Iat)
	}

	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatalf("decode signature: %v", err)
	}
	block, _ := pem.Decode(pemBytes)
	der, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		t.Fatalf("parse key: %v", err)
	}
	pub := &der.(*rsa.PrivateKey).PublicKey
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(pub, crypto.SHA256, digest[:], sig); err != nil {
		t.Errorf("signature does not verify with the app public key: %v", err)
	}
}

func TestNewApp_RejectsUnusableKeys(t *testing.T) {
	cases := map[string]struct {
		appID int64
		pem   []byte
		want  string
	}{
		"zero app id":       {0, testKeyPEM(t), "app id"},
		"not pem":           {1, []byte("hello"), "no PEM block"},
		"pem but not a key": {1, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte("nope")}), "parse private key"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := NewApp(tc.appID, tc.pem)
			if err == nil {
				t.Fatal("NewApp accepted an unusable key")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not name the cause %q", err, tc.want)
			}
			if !strings.Contains(err.Error(), ErrBadKey.Error()) {
				t.Errorf("error %q does not wrap ErrBadKey", err)
			}
		})
	}
}
