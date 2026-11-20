package store

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/workflows"
)

// A sealed value is stored only as ciphertext; the row reads back with it for
// the control plane and never serializes it.
func TestSealedSecretReferenceStoresCiphertextAndRefusesDuplicateNames(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, ctx)
	tenant := "sealed-secrets"
	if err := s.CreateTenant(ctx, tenant, "Sealed Secrets"); err != nil {
		t.Fatal(err)
	}
	sealed := domain.SealedCredential{Ciphertext: []byte("ciphertext-bytes"), Nonce: []byte("nonce-bytes-")}
	created, err := s.CreateSealedSecretReference(ctx, tenant, "hf-token", sealed, "environment:KEY")
	if err != nil || created.Resolver != "sealed" || created.Reference != "" {
		t.Fatalf("created=%#v err=%v", created, err)
	}
	read, err := s.SecretReferenceForTenant(ctx, tenant, created.ID)
	if err != nil || !bytes.Equal(read.Sealed.Ciphertext, sealed.Ciphertext) || !bytes.Equal(read.Sealed.Nonce, sealed.Nonce) || read.KeyReference != "environment:KEY" {
		t.Fatalf("read=%#v err=%v", read, err)
	}
	if _, err = s.CreateSealedSecretReference(ctx, tenant, "hf-token", sealed, "environment:KEY"); !errors.Is(err, ErrConflict) {
		t.Fatalf("a duplicate name replaced a sealed value: %v", err)
	}
	if _, err = s.CreateSecretReference(ctx, tenant, "hf-token-2", "sealed", "x"); err == nil {
		t.Fatal("a sealed secret was created without a value")
	}
	worker, err := s.CreateSecretReference(ctx, tenant, "cluster-token", "kubernetes", "hf-token/token")
	if err != nil || worker.Resolver != "kubernetes" || len(worker.Sealed.Ciphertext) != 0 {
		t.Fatalf("worker-held reference: %#v err=%v", worker, err)
	}
	listed, err := s.SecretReferencesForTenant(ctx, tenant)
	if err != nil || len(listed) != 2 {
		t.Fatalf("listed=%#v err=%v", listed, err)
	}
	// The database refuses a sealed row without ciphertext.
	if _, err = s.ExecContext(ctx, `INSERT INTO secret_references(id,tenant_id,name,resolver,reference,created_at,updated_at) VALUES('bad-sealed',?,'bad','sealed','',now(),now())`, tenant); err == nil {
		t.Fatal("a sealed row without ciphertext was stored")
	}
}

func TestModelImportIsPinnedPerWorkspaceAndRetrySafe(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, ctx)
	for _, tenant := range []string{"imports-a", "imports-b"} {
		if err := s.CreateTenant(ctx, tenant, tenant); err != nil {
			t.Fatal(err)
		}
	}
	digest := "sha256:" + strings.Repeat("4", 64)
	item := domain.ModelImport{TenantID: "imports-a", Kind: "weights", Source: "s3", URI: "s3://acme-models/run-42", ManifestDigest: digest, Files: []domain.ModelFile{{Path: "config.json", Size: 2, SHA256: strings.Repeat("a", 64)}}, Checks: domain.ModelImportCheck{Verified: []string{"manifest"}, Unknown: []string{}, Failed: []string{}, TotalBytes: 2, Architecture: "Qwen3ForCausalLM"}}
	first, err := s.CreateModelImport(ctx, item)
	if err != nil || first.ID == "" || first.Checks.Architecture != "Qwen3ForCausalLM" || len(first.Files) != 1 {
		t.Fatalf("first=%#v err=%v", first, err)
	}
	again, err := s.CreateModelImport(ctx, item)
	if err != nil || again.ID != first.ID {
		t.Fatalf("a retried import created another row: %#v err=%v", again, err)
	}
	if _, err = s.ModelImportForTenant(ctx, "imports-b", item.URI, digest); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another workspace read the import: %v", err)
	}
	listed, err := s.ModelImportsForTenant(ctx, "imports-a", 10)
	if err != nil || len(listed) != 1 {
		t.Fatalf("listed=%#v err=%v", listed, err)
	}
	item.ManifestDigest = "sha256:not-a-digest"
	if _, err = s.CreateModelImport(ctx, item); err == nil {
		t.Fatal("an invalid digest was stored")
	}
}

func TestObjectStoreArtifactAttachesToARevision(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, ctx)
	name := "object-store-artifact"
	deployment, _, _, err := s.SubmitCloudDeployment(ctx,
		domain.Deployment{Name: name, Model: "s3://acme-models/run-42", MinReplicas: 1, MaxReplicas: 1},
		domain.Operation{Kind: workflows.ConvergeKind, IdempotencyKey: "object-store-artifact", RequestJSON: `{"name":"` + name + `","model":"s3://acme-models/run-42","cloud":"kubernetes","gpu":"L4","tenant_id":"global"}`},
	)
	if err != nil {
		t.Fatal(err)
	}
	digest := "sha256:" + strings.Repeat("5", 64)
	size := int64(42)
	artifact, err := s.AttachModelArtifact(ctx, "global", deployment.ActiveRevisionID, domain.ModelArtifact{Source: "s3", Repository: "s3://acme-models/run-42", RequestedRevision: digest, ImmutableRevision: digest, ModelIdentity: "s3://acme-models/run-42@" + digest, ApproximateSizeBytes: &size, CacheState: "unknown"})
	if err != nil || artifact.Source != "s3" || artifact.ImmutableRevision != digest {
		t.Fatalf("artifact=%#v err=%v", artifact, err)
	}
	if _, err = s.AttachModelArtifact(ctx, "global", deployment.ActiveRevisionID, domain.ModelArtifact{Source: "http", Repository: "x", ImmutableRevision: "y", ModelIdentity: "z"}); err == nil {
		t.Fatal("an unknown artifact source was attached")
	}
}
