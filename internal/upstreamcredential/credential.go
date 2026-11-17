// Package upstreamcredential seals the optional bearer credential of a model
// server that the control plane reaches directly (infercrane connect
// --token-env). The plaintext is write-only at the API boundary, encrypted
// with the tenant credential key, and bound to its tenant and endpoint so a
// ciphertext moved to another row does not open.
package upstreamcredential

import (
	"context"
	"errors"
	"strings"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/secretcipher"
)

// MaxBytes bounds one credential.
const MaxBytes = 4096

// Store reads a sealed credential by target.
type Store interface {
	AdoptedCredential(context.Context, string, string) (domain.AdoptedCredential, error)
}

// Sealer seals and opens server credentials.
type Sealer struct {
	Cipher secretcipher.Cipher
	Store  Store
}

func associatedData(tenant, endpoint string) []byte {
	return []byte("infercrane.upstream-credential/v1\x00" + tenant + "\x00" + endpoint)
}

// Validate rejects an empty, oversized, or multi-token credential without
// echoing it.
func Validate(token string) error {
	if token == "" || len(token) > MaxBytes {
		return errors.New("the server credential must be 1..4096 bytes")
	}
	if strings.ContainsAny(token, " \t\r\n") {
		return errors.New("the server credential must be a single bearer token without whitespace")
	}
	for _, r := range token {
		if r < 0x21 || r == 0x7f {
			return errors.New("the server credential contains a control character")
		}
	}
	return nil
}

// Seal encrypts a credential for one tenant endpoint.
func (s Sealer) Seal(tenant, endpoint, token string) (domain.SealedCredential, error) {
	if err := Validate(token); err != nil {
		return domain.SealedCredential{}, err
	}
	ciphertext, nonce, err := s.Cipher.Encrypt([]byte(token), associatedData(tenant, endpoint))
	if err != nil {
		return domain.SealedCredential{}, errors.New("the server credential could not be sealed")
	}
	return domain.SealedCredential{Ciphertext: ciphertext, Nonce: nonce}, nil
}

// Resolve opens the credential stored for a target. It returns
// domain.ErrNotFound when the target has none.
func (s Sealer) Resolve(ctx context.Context, tenant, targetID string) (string, error) {
	if s.Store == nil {
		return "", domain.ErrNotFound
	}
	stored, err := s.Store.AdoptedCredential(ctx, tenant, targetID)
	if err != nil {
		return "", err
	}
	plaintext, err := s.Cipher.Decrypt(stored.Sealed.Ciphertext, stored.Sealed.Nonce, associatedData(stored.TenantID, stored.EndpointName))
	if err != nil {
		return "", errors.New("the stored server credential could not be opened")
	}
	return string(plaintext), nil
}
