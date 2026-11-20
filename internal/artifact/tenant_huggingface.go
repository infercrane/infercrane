package artifact

import (
	"context"
	"errors"
	"fmt"
	"regexp"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/secrets"
)

type SecretStore interface {
	SecretReferenceForTenant(context.Context, string, string) (domain.SecretReference, error)
}

var fullCommit = regexp.MustCompile(`^[0-9a-f]{40}$`)

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

// ResolveAuthenticated pins a private or gated repository with the
// workspace's token. A token that stays in the customer's own secret store
// cannot be read here, so the deployment must already name a full commit,
// which is pinned as given and checked by the worker's download.
func (r TenantHuggingFace) ResolveAuthenticated(ctx context.Context, tenant, secretReferenceID, repository, revision string) (domain.ModelArtifact, error) {
	if r.Store == nil || r.Secrets == nil {
		return domain.ModelArtifact{}, fmt.Errorf("authenticated Hugging Face resolution is not configured")
	}
	reference, err := r.Store.SecretReferenceForTenant(ctx, tenant, secretReferenceID)
	if err != nil {
		return domain.ModelArtifact{}, fmt.Errorf("load Hugging Face credential reference: %w", err)
	}
	if secrets.WorkerHeld(reference.Resolver) {
		if !fullCommit.MatchString(revision) {
			return domain.ModelArtifact{}, errors.New("this Hugging Face token stays in your own secret store, so InferCrane cannot pin a branch or tag; set model_revision to the full 40-character commit")
		}
		return domain.ModelArtifact{Source: "huggingface", Repository: repository, RequestedRevision: revision, ImmutableRevision: revision, ModelIdentity: repository + "@" + revision, CacheState: "unknown", RuntimeCompatibilityJSON: "{}"}, nil
	}
	token, err := r.Secrets.Resolve(ctx, reference)
	if err != nil {
		return domain.ModelArtifact{}, fmt.Errorf("resolve Hugging Face credential: %w", err)
	}
	return r.Public.ResolveWithToken(ctx, repository, revision, token)
}

// ModelCredential loads the workspace's credential reference for a worker.
// open is nil when only the worker can read the value.
func (r TenantHuggingFace) ModelCredential(ctx context.Context, tenant, referenceID string) (domain.SecretReference, func(context.Context) (string, error), error) {
	if r.Store == nil || r.Secrets == nil {
		return domain.SecretReference{}, nil, errors.New("model credentials are not configured")
	}
	reference, err := r.Store.SecretReferenceForTenant(ctx, tenant, referenceID)
	if err != nil {
		return domain.SecretReference{}, nil, err
	}
	if secrets.WorkerHeld(reference.Resolver) {
		return reference, nil, nil
	}
	return reference, func(ctx context.Context) (string, error) { return r.Secrets.Resolve(ctx, reference) }, nil
}
