// Package securestore provides small authenticated-encryption helpers for
// sensitive values that must be persisted by the server.
package securestore

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"io"
)

// Cipher encrypts values with AES-256-GCM. A fresh nonce is prepended to every
// sealed value; callers must supply context-specific associated data.
type Cipher struct {
	aead cipher.AEAD
}

// New creates a cipher from a stable 32-byte secret.
func New(key []byte) (*Cipher, error) {
	if len(key) != 32 {
		return nil, errors.New("securestore key must be exactly 32 bytes")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Cipher{aead: aead}, nil
}

// Seal returns nonce || ciphertext || authentication tag.
func (c *Cipher) Seal(plaintext, aad []byte) ([]byte, error) {
	if c == nil || c.aead == nil {
		return nil, errors.New("securestore cipher is unavailable")
	}
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	sealed := c.aead.Seal(nonce, nonce, plaintext, aad)
	return sealed, nil
}

// Open authenticates and decrypts a value produced by Seal.
func (c *Cipher) Open(sealed, aad []byte) ([]byte, error) {
	if c == nil || c.aead == nil {
		return nil, errors.New("securestore cipher is unavailable")
	}
	nonceSize := c.aead.NonceSize()
	if len(sealed) < nonceSize+c.aead.Overhead() {
		return nil, errors.New("securestore ciphertext is truncated")
	}
	nonce, ciphertext := sealed[:nonceSize], sealed[nonceSize:]
	return c.aead.Open(nil, nonce, ciphertext, aad)
}
