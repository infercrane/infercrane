// Package computeconnection owns the write-only credential boundary for
// tenant-scoped infrastructure accounts.
package computeconnection

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/secretcipher"
)

var connectionNamePattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9._-]{0,62}[a-z0-9])?$`)

type Store interface {
	CreateComputeConnection(context.Context, string, domain.ComputeConnection) (domain.ComputeConnection, error)
	ComputeConnectionForTenant(context.Context, string, string) (domain.ComputeConnection, error)
	ComputeConnectionsForTenant(context.Context, string) ([]domain.ComputeConnection, error)
	DeleteComputeConnectionForTenant(context.Context, string, string) error
}

type CredentialVerifier interface {
	VerifyCredential(context.Context, string) error
}

type Service struct {
	Store        Store
	Cipher       secretcipher.Cipher
	KeyReference string
	Verifiers    map[string]CredentialVerifier
	Now          func() time.Time
}

type CreateRequest struct {
	Name       string `json:"name"`
	Provider   string `json:"provider"`
	Credential string `json:"credential"`
}

func (s Service) Create(ctx context.Context, tenant string, request CreateRequest) (domain.ComputeConnection, error) {
	request.Name = strings.TrimSpace(strings.ToLower(request.Name))
	request.Provider = strings.TrimSpace(strings.ToLower(request.Provider))
	request.Credential = strings.TrimSpace(request.Credential)
	if tenant == "" || !connectionNamePattern.MatchString(request.Name) {
		return domain.ComputeConnection{}, errors.New("tenant and a valid connection name are required")
	}
	if len(request.Credential) < 16 || len(request.Credential) > 4096 {
		return domain.ComputeConnection{}, errors.New("provider credential length is invalid")
	}
	if request.Provider != "runpod" {
		return domain.ComputeConnection{}, errors.New("only RunPod Pods is currently qualified for self-serve BYOC")
	}
	verifier := s.Verifiers[request.Provider]
	if verifier == nil {
		return domain.ComputeConnection{}, fmt.Errorf("provider %q does not support self-serve compute connections", request.Provider)
	}
	if s.Store == nil || s.KeyReference == "" {
		return domain.ComputeConnection{}, errors.New("compute credential storage is not configured")
	}
	if err := verifier.VerifyCredential(ctx, request.Credential); err != nil {
		return domain.ComputeConnection{}, fmt.Errorf("provider credential verification failed: %w", err)
	}
	ciphertext, nonce, err := s.Cipher.Encrypt([]byte(request.Credential), associatedData(tenant, request.Provider, request.Name))
	if err != nil {
		return domain.ComputeConnection{}, errors.New("provider credential encryption failed")
	}
	now := time.Now().UTC()
	if s.Now != nil {
		now = s.Now().UTC()
	}
	return s.Store.CreateComputeConnection(ctx, tenant, domain.ComputeConnection{
		Name: request.Name, Provider: request.Provider, Adapter: "runpod-pods", Status: "verified",
		CredentialCiphertext: ciphertext, CredentialNonce: nonce,
		CredentialKeyReference: s.KeyReference, VerifiedAt: now,
	})
}

func (s Service) Resolve(ctx context.Context, tenant, id, provider string) (domain.ComputeConnection, string, error) {
	if s.Store == nil || tenant == "" || id == "" || provider == "" {
		return domain.ComputeConnection{}, "", errors.New("compute connection identity is required")
	}
	item, err := s.Store.ComputeConnectionForTenant(ctx, tenant, id)
	if err != nil {
		return domain.ComputeConnection{}, "", err
	}
	if item.Status != "verified" || item.Provider != provider {
		return domain.ComputeConnection{}, "", errors.New("compute connection does not match the requested provider")
	}
	if item.CredentialKeyReference != s.KeyReference {
		return domain.ComputeConnection{}, "", errors.New("compute connection uses an unavailable encryption key")
	}
	plaintext, err := s.Cipher.Decrypt(item.CredentialCiphertext, item.CredentialNonce, associatedData(item.TenantID, item.Provider, item.Name))
	if err != nil {
		return domain.ComputeConnection{}, "", errors.New("compute credential decryption failed")
	}
	return item, string(plaintext), nil
}

func (s Service) Get(ctx context.Context, tenant, id string) (domain.ComputeConnection, error) {
	if s.Store == nil || tenant == "" || id == "" {
		return domain.ComputeConnection{}, errors.New("compute connection identity is required")
	}
	return s.Store.ComputeConnectionForTenant(ctx, tenant, id)
}

func (s Service) List(ctx context.Context, tenant string) ([]domain.ComputeConnection, error) {
	if s.Store == nil || tenant == "" {
		return nil, errors.New("tenant is required")
	}
	items, err := s.Store.ComputeConnectionsForTenant(ctx, tenant)
	if items == nil && err == nil {
		items = make([]domain.ComputeConnection, 0)
	}
	return items, err
}

func (s Service) Delete(ctx context.Context, tenant, id string) error {
	if s.Store == nil || tenant == "" || id == "" {
		return errors.New("compute connection identity is required")
	}
	return s.Store.DeleteComputeConnectionForTenant(ctx, tenant, id)
}

func associatedData(tenant, provider, name string) []byte {
	return []byte("infercrane.compute-credential.v1\x00" + tenant + "\x00" + provider + "\x00" + name)
}
