package auth

import (
	"encoding/hex"
	"strings"
	"testing"
)

// TestGenerateToken_RandomAndLongEnough is the smoke test the sub-agent
// (Phase 1.7) should expand: token entropy distribution, multiple gens
// don't collide over N runs, etc.
func TestGenerateToken_RandomAndLongEnough(t *testing.T) {
	a, err := GenerateToken()
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}
	b, err := GenerateToken()
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}
	if a == b {
		t.Fatal("two consecutive tokens are identical (entropy issue?)")
	}
	if len(a) < 40 {
		t.Errorf("token too short: %d chars", len(a))
	}

	if !strings.HasPrefix(a, v2TokenPrefix) {
		t.Errorf("token missing v2 prefix: %s", a)
	}

	tid, sec := SplitToken(a)
	if tid == "" {
		t.Error("SplitToken: token_id is empty for v2 token")
	}
	if sec == "" {
		t.Error("SplitToken: secret is empty for v2 token")
	}
	if len(tid) != 8 {
		t.Errorf("token_id length = %d, want 8", len(tid))
	}

	if _, err := hex.DecodeString(tid); err != nil {
		t.Errorf("token_id is not valid hex: %s", tid)
	}
}

func TestSplitToken_Legacy(t *testing.T) {

	tid, sec := SplitToken("some-old-plain-token")
	if tid != "" {
		t.Errorf("legacy SplitToken: tokenID = %q, want empty", tid)
	}
	if sec != "some-old-plain-token" {
		t.Errorf("legacy SplitToken: secret = %q, want original token", sec)
	}
}

func TestHashAndVerify_RoundTrip(t *testing.T) {
	tok, err := GenerateToken()
	if err != nil {
		t.Fatalf("gen: %v", err)
	}

	_, secret := SplitToken(tok)
	h, err := HashToken(secret)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if !strings.HasPrefix(h, hashPrefix) {
		t.Errorf("missing hash prefix: %s", h)
	}

	ok, err := VerifyToken(secret, h)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !ok {
		t.Fatal("VerifyToken returned false for matching pair")
	}
}

func TestVerifyToken_WrongSecret(t *testing.T) {
	h, err := HashToken("real-token")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	ok, err := VerifyToken("wrong-token", h)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if ok {
		t.Fatal("VerifyToken returned true for mismatched pair")
	}
}

func TestVerifyToken_MalformedHash(t *testing.T) {
	_, err := VerifyToken("any", "not-a-pmcluster-hash")
	if err != ErrInvalidHash {
		t.Errorf("err = %v, want ErrInvalidHash", err)
	}
}

func TestHashToken_EmptyRejected(t *testing.T) {
	if _, err := HashToken(""); err != ErrInvalidToken {
		t.Errorf("err = %v, want ErrInvalidToken", err)
	}
}

func TestFastHash(t *testing.T) {
	h := FastHash("hello")
	if len(h) != 64 {
		t.Errorf("FastHash length = %d, want 64", len(h))
	}

	if FastHash("hello") != h {
		t.Error("FastHash is not deterministic")
	}
}

func TestExtractBearer(t *testing.T) {
	cases := []struct {
		header    string
		wantToken string
		wantOK    bool
	}{
		{"", "", false},
		{"Bearer abc", "abc", true},
		{"bearer abc", "abc", true},
		{"  Bearer   abc  ", "abc", true},
		{"Basic abc", "", false},
		{"Bearer", "", false},
		{"Bearer ", "", false},
	}
	for _, c := range cases {
		t.Run(c.header, func(t *testing.T) {
			tok, ok := extractBearer(c.header)
			if ok != c.wantOK || tok != c.wantToken {
				t.Errorf("extractBearer(%q) = (%q, %v), want (%q, %v)",
					c.header, tok, ok, c.wantToken, c.wantOK)
			}
		})
	}
}
