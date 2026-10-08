// Package secretbox encrypts small secrets kept in the database (such as
// authenticator-app keys) with AES-256-GCM, so a copy of the database alone
// doesn't reveal them.
package secretbox

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"errors"
)

type Box struct{ aead cipher.AEAD }

// New derives the encryption key from keyMaterial (DATA_ENCRYPTION_KEY, or
// JWT_SECRET when that isn't set).
func New(keyMaterial []byte) *Box {
	key := sha256.Sum256(append([]byte("dos-bpas/secretbox/v1:"), keyMaterial...))
	block, err := aes.NewCipher(key[:])
	if err != nil {
		panic(err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		panic(err)
	}
	return &Box{aead: aead}
}

// Seal encrypts plaintext; the result starts with a random nonce.
func (b *Box) Seal(plaintext []byte) []byte {
	nonce := make([]byte, b.aead.NonceSize())
	_, _ = rand.Read(nonce)
	return b.aead.Seal(nonce, nonce, plaintext, nil)
}

var ErrDecrypt = errors.New("secretbox: cannot decrypt (wrong key or damaged data)")

func (b *Box) Open(sealed []byte) ([]byte, error) {
	n := b.aead.NonceSize()
	if len(sealed) < n {
		return nil, ErrDecrypt
	}
	out, err := b.aead.Open(nil, sealed[:n], sealed[n:], nil)
	if err != nil {
		return nil, ErrDecrypt
	}
	return out, nil
}
