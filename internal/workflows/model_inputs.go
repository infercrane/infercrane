package workflows

import (
	"context"
	"errors"
	"fmt"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/modelimport"
	"github.com/infercrane/infercrane/internal/operations"
	"github.com/infercrane/infercrane/internal/provision"
	"github.com/infercrane/infercrane/internal/secrets"
)

// replicaModelInputs is what a replica needs beyond a public Hugging Face
// model: a credential delivered natively by the provider, object store files
// pinned by a manifest, and LoRA adapters pinned by commit or digest.
type replicaModelInputs struct {
	credential *provision.ModelCredential
	objects    *provision.ObjectSource
	adapters   []provision.Adapter
}

// ModelCredentialSource loads a workspace's model credential reference for a
// worker. open is nil for a reference only the worker can read; otherwise it
// returns the value in memory for an adapter that stores it in the provider
// account's own secret store.
type ModelCredentialSource interface {
	ModelCredential(ctx context.Context, tenant, referenceID string) (reference domain.SecretReference, open func(context.Context) (string, error), err error)
}

// modelImportReader reads imported object store manifests.
type modelImportReader interface {
	ModelImportForTenant(context.Context, string, string, string) (domain.ModelImport, error)
}

// ServedModel is the model name the replica answers to and the gateway sends:
// the LoRA adapter when one is served, otherwise the model itself.
func (r CloudRequest) ServedModel() string {
	if len(r.ModelAdapters) > 0 {
		return r.ModelAdapters[0].Name
	}
	return r.Model
}

func modelImport(ctx context.Context, store CloudStore, tenant, uri, digest, kind string) (domain.ModelImport, error) {
	reader, ok := store.(modelImportReader)
	if !ok {
		return domain.ModelImport{}, operations.Permanent("model_import_unavailable", errors.New("object store imports are not configured on this control plane"))
	}
	item, err := reader.ModelImportForTenant(ctx, tenant, uri, digest)
	if errors.Is(err, domain.ErrNotFound) {
		return domain.ModelImport{}, operations.Permanent("model_import_not_found", fmt.Errorf("no import of %s with manifest %s in this workspace; import it first", uri, digest))
	}
	if err != nil {
		return domain.ModelImport{}, operations.Retryable("model_import_lookup_failed", err)
	}
	if item.Kind != kind {
		return domain.ModelImport{}, operations.Permanent("model_import_kind_mismatch", fmt.Errorf("%s was imported as %s, not %s", uri, item.Kind, kind))
	}
	return item, nil
}

func objectSource(item domain.ModelImport) *provision.ObjectSource {
	files := make([]provision.ObjectFile, len(item.Files))
	for index, file := range item.Files {
		files[index] = provision.ObjectFile{Path: file.Path, Size: file.Size, SHA256: file.SHA256}
	}
	return &provision.ObjectSource{URI: item.URI, Digest: item.ManifestDigest, Files: files}
}

// resolveObjectArtifact pins object store weights to their imported manifest.
// Their size is the manifest's total; the worker cache state is unknown.
func resolveObjectArtifact(ctx context.Context, store CloudStore, request CloudRequest) (domain.ModelArtifact, error) {
	item, err := modelImport(ctx, store, request.TenantID, request.Model, request.ModelRevision, modelimport.KindWeights)
	if err != nil {
		return domain.ModelArtifact{}, err
	}
	size := item.Checks.TotalBytes
	return domain.ModelArtifact{Source: item.Source, Repository: item.URI, RequestedRevision: item.ManifestDigest, ImmutableRevision: item.ManifestDigest, ModelIdentity: item.URI + "@" + item.ManifestDigest, ApproximateSizeBytes: &size, CacheState: "unknown", RuntimeCompatibilityJSON: "{}"}, nil
}

// attachModelInputs resolves the request's credential, object store files, and
// adapters once per operation. Nothing here is persisted or logged.
func (r *CloudRequest) attachModelInputs(ctx context.Context, store CloudStore, resolver ArtifactResolver) error {
	var inputs replicaModelInputs
	if r.ModelSecretReferenceID != "" {
		source, ok := resolver.(ModelCredentialSource)
		if !ok {
			return operations.Permanent("model_credential_unavailable", errors.New("model credentials cannot be delivered to workers on this control plane"))
		}
		reference, open, err := source.ModelCredential(ctx, r.TenantID, r.ModelSecretReferenceID)
		if errors.Is(err, domain.ErrNotFound) {
			return operations.Permanent("secret_reference_not_found", errors.New("the model credential reference was deleted"))
		}
		if err != nil {
			return operations.Retryable("model_credential_lookup_failed", err)
		}
		credential := &provision.ModelCredential{ReferenceID: reference.ID, Resolver: reference.Resolver, Open: open}
		if secrets.WorkerHeld(reference.Resolver) {
			credential.Reference = reference.Reference
		}
		inputs.credential = credential
	}
	if modelimport.IsObjectModel(r.Model) {
		item, err := modelImport(ctx, store, r.TenantID, r.Model, r.ModelRevision, modelimport.KindWeights)
		if err != nil {
			return err
		}
		inputs.objects = objectSource(item)
	}
	for _, adapter := range r.ModelAdapters {
		resolved := provision.Adapter{Name: adapter.Name, Source: modelimport.AdapterSource(adapter), Revision: adapter.Revision, Rank: adapter.Rank}
		if resolved.Source == "huggingface" {
			resolved.Repository = adapter.Repository
		} else {
			item, err := modelImport(ctx, store, r.TenantID, adapter.Repository, adapter.Revision, modelimport.KindAdapter)
			if err != nil {
				return err
			}
			resolved.Objects = objectSource(item)
			if resolved.Rank == 0 {
				resolved.Rank = item.Checks.AdapterRank
			}
		}
		inputs.adapters = append(inputs.adapters, resolved)
	}
	r.modelInputs = inputs
	return nil
}
