// Package asyncinference provides content-safe primitives for bounded durable
// inference jobs. InferCrane never persists async prompt or result content in
// plaintext.
package asyncinference

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"io"
)

type Cipher struct{ key [32]byte }

func NewCipher(secret string) (Cipher, error) {
	if len(secret) < 32 {
		return Cipher{}, errors.New("async encryption secret must contain at least 32 bytes")
	}
	return Cipher{key: sha256.Sum256([]byte(secret))}, nil
}

func (c Cipher) Encrypt(plaintext, associatedData []byte) ([]byte, []byte, error) {
	block, err := aes.NewCipher(c.key[:])
	if err != nil {
		return nil, nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err = io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, nil, err
	}
	return aead.Seal(nil, nonce, plaintext, associatedData), nonce, nil
}

func (c Cipher) Decrypt(ciphertext, nonce, associatedData []byte) ([]byte, error) {
	block, err := aes.NewCipher(c.key[:])
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return aead.Open(nil, nonce, ciphertext, associatedData)
}
