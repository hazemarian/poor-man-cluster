// Package credentials handles AES-256-GCM at-rest encryption for values
// pmcluster stores in SQLite. The 32-byte key lives at
// $DataDir/.encryption_key (mode 0600); losing it renders all encrypted
// credentials unrecoverable — back it up with the DB.
//
// Format: 12-byte nonce || gcm_seal(plaintext). Tamper → ErrInvalidCiphertext.
package credentials

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
)

const (
	keyBytes   = 32
	nonceBytes = 12
)

// ErrInvalidCiphertext: malformed input or GCM auth tag mismatch.
var ErrInvalidCiphertext = errors.New("invalid or tampered ciphertext")

// Cipher is safe for concurrent use after construction.
type Cipher struct {
	gcm cipher.AEAD
}

// Open loads the key, creating it (mode 0600) on first use. The parent
// directory must already exist with safe permissions.
func Open(path string) (*Cipher, error) {
	key, err := loadOrCreateKey(path)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("aes new cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("new GCM: %w", err)
	}
	return &Cipher{gcm: gcm}, nil
}

// Encrypt: nonce(12B) || gcm_seal(plaintext). Fresh nonce per call.
func (c *Cipher) Encrypt(plaintext []byte) ([]byte, error) {
	nonce := make([]byte, nonceBytes)
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("read nonce: %w", err)
	}
	sealed := c.gcm.Seal(nil, nonce, plaintext, nil)
	out := make([]byte, 0, len(nonce)+len(sealed))
	out = append(out, nonce...)
	out = append(out, sealed...)
	return out, nil
}

// Decrypt returns ErrInvalidCiphertext for any tamper or malformed
// input; never panics, never leaks plaintext on auth failure.
func (c *Cipher) Decrypt(ciphertext []byte) ([]byte, error) {
	if len(ciphertext) < nonceBytes {
		return nil, ErrInvalidCiphertext
	}
	nonce, sealed := ciphertext[:nonceBytes], ciphertext[nonceBytes:]
	plain, err := c.gcm.Open(nil, nonce, sealed, nil)
	if err != nil {
		return nil, ErrInvalidCiphertext
	}
	return plain, nil
}

func loadOrCreateKey(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err == nil {
		if len(data) != keyBytes {
			return nil, fmt.Errorf("key file %s has wrong length (%d, expected %d)", path, len(data), keyBytes)
		}
		return data, nil
	}
	if !os.IsNotExist(err) {
		return nil, fmt.Errorf("read key file: %w", err)
	}

	key := make([]byte, keyBytes)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		return nil, fmt.Errorf("generate key: %w", err)
	}

	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, fmt.Errorf("create key file: %w", err)
	}
	if _, err := f.Write(key); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("write key file: %w", err)
	}
	if err := f.Close(); err != nil {
		return nil, fmt.Errorf("close key file: %w", err)
	}
	return key, nil
}
