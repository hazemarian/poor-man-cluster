package credentials

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestOpen_CreatesNewKeyFile verifies that Open creates a key file when the
// path doesn't exist, and that the file has mode 0600.
func TestOpen_CreatesNewKeyFile(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, ".encryption_key")

	c, err := Open(keyPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if c == nil {
		t.Fatal("Open returned nil Cipher")
	}

	info, err := os.Stat(keyPath)
	if err != nil {
		t.Fatalf("Stat key file: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("key file mode = %o, want 0600", info.Mode().Perm())
	}

	data, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatalf("ReadFile key: %v", err)
	}
	if len(data) != keyBytes {
		t.Errorf("key file length = %d, want %d", len(data), keyBytes)
	}
}

// TestOpen_ReadsExistingKeyFile verifies that Open reads an existing key file
// without overwriting it.
func TestOpen_ReadsExistingKeyFile(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, ".encryption_key")

	key := make([]byte, keyBytes)
	for i := range key {
		key[i] = byte(i)
	}
	if err := os.WriteFile(keyPath, key, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	c, err := Open(keyPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if c == nil {
		t.Fatal("Open returned nil Cipher")
	}

	got, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatalf("ReadFile after Open: %v", err)
	}
	if !bytes.Equal(got, key) {
		t.Error("Open modified the existing key file")
	}
}

// TestOpen_RejectsWrongKeyLength verifies that Open returns an error when the
// key file exists but has the wrong byte count.
func TestOpen_RejectsWrongKeyLength(t *testing.T) {
	cases := []struct {
		name string
		size int
	}{
		{"too short", 16},
		{"too long", 64},
		{"empty", 0},
		{"off by one", keyBytes - 1},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			keyPath := filepath.Join(dir, ".encryption_key")
			if err := os.WriteFile(keyPath, make([]byte, tc.size), 0o600); err != nil {
				t.Fatalf("WriteFile: %v", err)
			}
			_, err := Open(keyPath)
			if err == nil {
				t.Errorf("Open with %d-byte key: expected error, got nil", tc.size)
			}
		})
	}
}

// TestEncryptDecrypt_RoundTrip verifies multiple payloads encrypt and then
// decrypt back to the original bytes.
func TestEncryptDecrypt_RoundTrip(t *testing.T) {
	c := newTestCipher(t)

	cases := []struct {
		name      string
		plaintext []byte
	}{
		{"empty", []byte{}},
		{"ascii", []byte("hello, pmcluster!")},
		{"binary", []byte{0x00, 0xFF, 0x7F, 0x80, 0x01}},
		{"long", bytes.Repeat([]byte("x"), 4096)},
		{"unicode", []byte("こんにちは世界 🌐")},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ct, err := c.Encrypt(tc.plaintext)
			if err != nil {
				t.Fatalf("Encrypt: %v", err)
			}
			got, err := c.Decrypt(ct)
			if err != nil {
				t.Fatalf("Decrypt: %v", err)
			}
			if !bytes.Equal(got, tc.plaintext) {
				t.Errorf("round-trip mismatch: got %q, want %q", got, tc.plaintext)
			}
		})
	}
}

// TestEncrypt_RandomNonce verifies that encrypting the same plaintext twice
// yields different ciphertexts (proof that the nonce is random each call).
func TestEncrypt_RandomNonce(t *testing.T) {
	c := newTestCipher(t)
	plaintext := []byte("test-random-nonce")

	seen := make(map[string]bool)
	for i := 0; i < 20; i++ {
		ct, err := c.Encrypt(plaintext)
		if err != nil {
			t.Fatalf("Encrypt #%d: %v", i, err)
		}
		k := string(ct)
		if seen[k] {
			t.Fatal("duplicate ciphertext produced — nonce not random")
		}
		seen[k] = true
	}
}

// TestDecrypt_TamperedCiphertext verifies that flipping a byte in the sealed
// portion returns ErrInvalidCiphertext.
func TestDecrypt_TamperedCiphertext(t *testing.T) {
	c := newTestCipher(t)

	ct, err := c.Encrypt([]byte("tamper me"))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}

	tampered := make([]byte, len(ct))
	copy(tampered, ct)
	mid := nonceBytes + len(ct[nonceBytes:])/2
	tampered[mid] ^= 0xFF

	_, err = c.Decrypt(tampered)
	if !errors.Is(err, ErrInvalidCiphertext) {
		t.Errorf("Decrypt(tampered): err = %v, want ErrInvalidCiphertext", err)
	}
}

// TestDecrypt_TooShortCiphertext verifies that ciphertexts shorter than 12
// bytes (nonce length) return ErrInvalidCiphertext.
func TestDecrypt_TooShortCiphertext(t *testing.T) {
	c := newTestCipher(t)

	cases := []struct {
		name string
		ct   []byte
	}{
		{"empty", []byte{}},
		{"one byte", []byte{0x01}},
		{"eleven bytes", make([]byte, nonceBytes-1)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := c.Decrypt(tc.ct)
			if !errors.Is(err, ErrInvalidCiphertext) {
				t.Errorf("Decrypt(%d bytes): err = %v, want ErrInvalidCiphertext", len(tc.ct), err)
			}
		})
	}
}

// TestOpen_KeyPersistence verifies that a second Open on the same file yields
// a Cipher that can decrypt what the first Cipher wrote.
func TestOpen_KeyPersistence(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, ".encryption_key")

	c1, err := Open(keyPath)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}

	plaintext := []byte("persist across opens")
	ct, err := c1.Encrypt(plaintext)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}

	c2, err := Open(keyPath)
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}

	got, err := c2.Decrypt(ct)
	if err != nil {
		t.Fatalf("Decrypt with second cipher: %v", err)
	}
	if !bytes.Equal(got, plaintext) {
		t.Errorf("key persistence failure: got %q, want %q", got, plaintext)
	}
}

// newTestCipher is a helper that creates a Cipher backed by a temp key file.
func newTestCipher(t *testing.T) *Cipher {
	t.Helper()
	dir := t.TempDir()
	c, err := Open(filepath.Join(dir, ".encryption_key"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return c
}
