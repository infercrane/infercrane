package artifact

import (
	"context"
	"fmt"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/secrets"
)

type SecretStore interface {
	SecretReferenceForTenant(context.Context, string, string) (domain.SecretReference, error)
}

// TenantHuggingFace resolves a tenant-scoped secret reference at the final
// artifact-discovery boundary. Public model resolution remains credentialless.
type TenantHuggingFace struct {
	Public  HuggingFace
	Store   SecretStore
	Secrets secrets.Resolver
}

func (r TenantHuggingFace) Resolve(ctx context.Context, repository, revision string) (domain.ModelArtifact, error) {
	return r.Public.Resolve(ctx, repository, revision)
}

func (r TenantHuggingFace) ResolveAuthenticated(ctx context.Context, tenant, secretReferenceID, repository, revision string) (domain.ModelArtifact, error) {
	if r.Store == nil || r.Secrets == nil {
		return domain.ModelArtifact{}, fmt.Errorf("authenticated Hugging Face resolution is not configured")
	}
	reference, err := r.Store.SecretReferenceForTenant(ctx, tenant, secretReferenceID)
	if err != nil {
		return domain.ModelArtifact{}, fmt.Errorf("load Hugging Face credential reference: %w", err)
	}
	token, err := r.Secrets.Resolve(ctx, reference)
	if err != nil {
		return domain.ModelArtifact{}, fmt.Errorf("resolve Hugging Face credential: %w", err)
	}
	return r.Public.ResolveWithToken(ctx, repository, revision, token)
}
