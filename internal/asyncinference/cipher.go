// Package asyncinference provides content-safe primitives for bounded durable
// inference jobs. InferCrane never persists async prompt or result content in
// plaintext.
package asyncinference

import "github.com/infercrane/infercrane/internal/secretcipher"

type Cipher = secretcipher.Cipher

func NewCipher(secret string) (Cipher, error) {
	return secretcipher.New(secret)
}
