package cookies

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
)

// ErrDecrypt reports a stored value that cannot be decrypted: the key is not
// the one it was encrypted with, or the data was altered.
var ErrDecrypt = errors.New("cookies: cannot decrypt value")

// Cipher encrypts cookie values with AES-256-GCM. The output is the 12-byte
// random nonce followed by the ciphertext and tag. It is safe for concurrent
// use.
type Cipher struct {
	aead cipher.AEAD
}

// NewCipher returns a Cipher for a 32-byte key.
func NewCipher(key []byte) (*Cipher, error) {
	if len(key) != keySize {
		return nil, fmt.Errorf("cookies: key is %d bytes, want %d", len(key), keySize)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("cookies: init cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("cookies: init cipher: %w", err)
	}
	return &Cipher{aead: aead}, nil
}

// Seal encrypts plaintext. aad is authenticated but not stored; Open needs the
// same value, which binds a ciphertext to the cookie it belongs to.
func (c *Cipher) Seal(plaintext, aad []byte) ([]byte, error) {
	nonce := make([]byte, c.aead.NonceSize(), c.aead.NonceSize()+len(plaintext)+c.aead.Overhead())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("cookies: generate nonce: %w", err)
	}
	return c.aead.Seal(nonce, nonce, plaintext, aad), nil
}

// Open decrypts the output of Seal.
func (c *Cipher) Open(sealed, aad []byte) ([]byte, error) {
	n := c.aead.NonceSize()
	if len(sealed) < n+c.aead.Overhead() {
		return nil, fmt.Errorf("%w: value too short", ErrDecrypt)
	}
	plain, err := c.aead.Open(nil, sealed[:n], sealed[n:], aad)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrDecrypt, err)
	}
	return plain, nil
}
