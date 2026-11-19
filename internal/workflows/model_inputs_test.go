package workflows

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/operations"
	"github.com/infercrane/infercrane/internal/provision"
)

const testDigest = "sha256:2222222222222222222222222222222222222222222222222222222222222222"

type importingCloudStore struct {
	*fakeCloudStore
	imports map[string]domain.ModelImport
}

func (s *importingCloudStore) ModelImportForTenant(_ context.Context, tenant, uri, digest string) (domain.ModelImport, error) {
	item, ok := s.imports[tenant+"|"+uri+"|"+digest]
	if !ok {
		return domain.ModelImport{}, domain.ErrNotFound
	}
	return item, nil
}

type credentialResolver struct {
	reference domain.SecretReference
	opened    *int
}

func (credentialResolver) Resolve(_ context.Context, repository, revision string) (domain.ModelArtifact, error) {
	return domain.ModelArtifact{Source: "huggingface", Repository: repository, ImmutableRevision: revision, ModelIdentity: repository + "@" + revision}, nil
}

func (r credentialResolver) ModelCredential(_ context.Context, tenant, id string) (domain.SecretReference, func(context.Context) (string, error), error) {
	if tenant != r.reference.TenantID || id != r.reference.ID {
		return domain.SecretReference{}, nil, domain.ErrNotFound
	}
	if r.reference.Resolver != "sealed" {
		return r.reference, nil, nil
	}
	return r.reference, func(context.Context) (string, error) { *r.opened++; return "hf_value", nil }, nil
}

type modelsInspector struct{ models []string }

func (i modelsInspector) Inspect(context.Context, string) (bool, map[string]struct{}) {
	out := map[string]struct{}{}
	for _, name := range i.models {
		out[name] = struct{}{}
	}
	return true, out
}

func adapterImport(uri string, rank int) domain.ModelImport {
	return domain.ModelImport{Kind: "adapter", Source: "s3", URI: uri, ManifestDigest: testDigest, Files: []domain.ModelFile{{Path: "adapter_config.json", Size: 10, SHA256: strings.Repeat("a", 64)}, {Path: "adapter_model.safetensors", Size: 20, SHA256: strings.Repeat("b", 64)}}, Checks: domain.ModelImportCheck{AdapterRank: rank}}
}

func TestConvergeDeliversCredentialFilesAndAdapterToTheReplica(t *testing.T) {
	store := &importingCloudStore{
		fakeCloudStore: &fakeCloudStore{deployment: domain.Deployment{ID: "deployment-1", Name: "support", Model: "s3://acme-models/run-42", RoutingStrategy: "round-robin", MinReplicas: 1, MaxReplicas: 1}},
		imports: map[string]domain.ModelImport{
			"acme|s3://acme-models/run-42|" + testDigest: {Kind: "weights", Source: "s3", URI: "s3://acme-models/run-42", ManifestDigest: testDigest, Files: []domain.ModelFile{{Path: "config.json", Size: 2, SHA256: strings.Repeat("c", 64)}}, Checks: domain.ModelImportCheck{TotalBytes: 2}},
			"acme|s3://acme-adapters/support|" + testDigest: adapterImport("s3://acme-adapters/support", 64),
		},
	}
	provider := &fakeReplicaProvider{observation: provision.Observation{Exists: true, State: "ready", Endpoint: "http://gpu:8000", Details: `{}`}}
	backends, err := NewReplicaBackends(ReplicaBackend{Name: "kubernetes", Cloud: "kubernetes", Runtime: "vllm", Profile: testElasticProfile("kubernetes", "kubernetes"), Provider: provider})
	if err != nil {
		t.Fatal(err)
	}
	resolver := credentialResolver{reference: domain.SecretReference{ID: "secret-1", TenantID: "acme", Resolver: "kubernetes", Reference: "hf-token/HF_TOKEN"}}
	request := `{"deployment_id":"deployment-1","name":"support","model":"s3://acme-models/run-42","model_revision":"` + testDigest + `","model_secret_reference_id":"secret-1","model_adapters":[{"name":"support-bot","repository":"s3://acme-adapters/support","revision":"` + testDigest + `"}],"cloud":"kubernetes","gpu":"L4","tenant_id":"acme","min_replicas":1,"max_replicas":1}`
	handlers := CloudHandlersWithBackends(store, backends, testRuntimeBackends(t, modelsInspector{models: []string{"s3://acme-models/run-42", "support-bot"}}), resolver)
	if _, err = handlers[ConvergeKind](context.Background(), domain.Operation{ID: "op-1", LeaseOwner: "worker", LeaseGeneration: 1, RequestJSON: request}); err != nil {
		t.Fatal(err)
	}
	spec := provider.lastSpec
	if spec.ModelObjects == nil || spec.ModelObjects.Digest != testDigest || len(spec.ModelObjects.Files) != 1 || spec.ModelObjects.Files[0].Path != "config.json" {
		t.Fatalf("object store files not delivered: %#v", spec.ModelObjects)
	}
	if spec.ModelCredential == nil || spec.ModelCredential.Resolver != "kubernetes" || spec.ModelCredential.Reference != "hf-token/HF_TOKEN" || spec.ModelCredential.Open != nil {
		t.Fatalf("worker-held credential not passed as a reference: %#v", spec.ModelCredential)
	}
	if len(spec.Adapters) != 1 || spec.Adapters[0].Rank != 64 || spec.Adapters[0].Objects == nil || spec.Adapters[0].Source != "s3" {
		t.Fatalf("adapter not resolved from its import: %#v", spec.Adapters)
	}
	if store.target.UpstreamModel != "support-bot" {
		t.Fatalf("the endpoint must answer with the adapter, got %q", store.target.UpstreamModel)
	}
}

func TestConvergeOpensSealedCredentialOnlyThroughTheProvider(t *testing.T) {
	store := &fakeCloudStore{deployment: domain.Deployment{ID: "deployment-1", Name: "private", Model: "acme/private", RoutingStrategy: "round-robin", MinReplicas: 1, MaxReplicas: 1}}
	provider := &fakeReplicaProvider{observation: provision.Observation{Exists: true, State: "ready", Endpoint: "http://gpu:8000", Details: `{}`}}
	backends, err := NewReplicaBackends(ReplicaBackend{Name: "runpod-pods", Cloud: "runpod", Runtime: "vllm", Profile: testElasticProfile("runpod-pods", "runpod"), Provider: provider})
	if err != nil {
		t.Fatal(err)
	}
	opened := 0
	resolver := credentialResolver{reference: domain.SecretReference{ID: "secret-1", TenantID: "acme", Resolver: "sealed"}, opened: &opened}
	request := `{"deployment_id":"deployment-1","name":"private","model":"acme/private","model_secret_reference_id":"secret-1","cloud":"runpod","gpu":"H100","tenant_id":"acme","min_replicas":1,"max_replicas":1}`
	handlers := CloudHandlersWithBackends(store, backends, testRuntimeBackends(t, modelsInspector{models: []string{"acme/private"}}), resolver)
	if _, err = handlers[ConvergeKind](context.Background(), domain.Operation{ID: "op-1", LeaseOwner: "worker", LeaseGeneration: 1, RequestJSON: request}); err != nil {
		t.Fatal(err)
	}
	credential := provider.lastSpec.ModelCredential
	if credential == nil || credential.Resolver != "sealed" || credential.Reference != "" || opened != 0 {
		t.Fatalf("the workflow must hand the provider a way to open the value, not the value: %#v opened=%d", credential, opened)
	}
	if value, openErr := credential.Open(context.Background()); openErr != nil || value != "hf_value" || opened != 1 {
		t.Fatalf("provider could not open the sealed credential: %q %v", value, openErr)
	}
}

func TestConvergeRefusesMissingImportsAndDeletedCredentials(t *testing.T) {
	for name, request := range map[string]string{
		"missing import":     `{"deployment_id":"deployment-1","name":"x","model":"s3://acme-models/gone","model_revision":"` + testDigest + `","cloud":"kubernetes","gpu":"L4","tenant_id":"acme","min_replicas":1,"max_replicas":1}`,
		"deleted credential": `{"deployment_id":"deployment-1","name":"x","model":"acme/private","model_secret_reference_id":"gone","cloud":"kubernetes","gpu":"L4","tenant_id":"acme","min_replicas":1,"max_replicas":1}`,
	} {
		t.Run(name, func(t *testing.T) {
			store := &importingCloudStore{fakeCloudStore: &fakeCloudStore{deployment: domain.Deployment{ID: "deployment-1", Name: "x", MinReplicas: 1, MaxReplicas: 1}}, imports: map[string]domain.ModelImport{}}
			provider := &fakeReplicaProvider{}
			backends, _ := NewReplicaBackends(ReplicaBackend{Name: "kubernetes", Cloud: "kubernetes", Runtime: "vllm", Profile: testElasticProfile("kubernetes", "kubernetes"), Provider: provider})
			resolver := credentialResolver{reference: domain.SecretReference{ID: "secret-1", TenantID: "acme", Resolver: "kubernetes", Reference: "a/b"}}
			_, err := CloudHandlersWithBackends(store, backends, testRuntimeBackends(t, modelsInspector{}), resolver)[ConvergeKind](context.Background(), domain.Operation{ID: "op-1", LeaseOwner: "worker", LeaseGeneration: 1, RequestJSON: request})
			var failure operations.Failure
			if !errors.As(err, &failure) || failure.Retryable || provider.ensureCalls != 0 {
				t.Fatalf("expected a permanent failure before provisioning: %v calls=%d", err, provider.ensureCalls)
			}
		})
	}
}

func TestResolveObjectArtifactPinsTheManifest(t *testing.T) {
	store := &importingCloudStore{fakeCloudStore: &fakeCloudStore{}, imports: map[string]domain.ModelImport{
		"acme|gs://acme-models/run|" + testDigest: {Kind: "weights", Source: "gs", URI: "gs://acme-models/run", ManifestDigest: testDigest, Checks: domain.ModelImportCheck{TotalBytes: 42}},
	}}
	artifact, err := resolveRequestedArtifact(context.Background(), store, nil, CloudRequest{TenantID: "acme", Model: "gs://acme-models/run", ModelRevision: testDigest})
	if err != nil || artifact.Source != "gs" || artifact.ImmutableRevision != testDigest || artifact.ModelIdentity != "gs://acme-models/run@"+testDigest || *artifact.ApproximateSizeBytes != 42 || artifact.CacheState != "unknown" {
		t.Fatalf("artifact=%#v err=%v", artifact, err)
	}
	if _, err = resolveRequestedArtifact(context.Background(), store, nil, CloudRequest{TenantID: "other", Model: "gs://acme-models/run", ModelRevision: testDigest}); err == nil {
		t.Fatal("another workspace resolved this import")
	}
}

func TestCloudRequestValidatesModelSources(t *testing.T) {
	valid := CloudRequest{Name: "x", Model: "Qwen/Qwen3-8B", Cloud: "kubernetes", GPU: "L4", ModelAdapters: []domain.ModelAdapter{{Name: "bot", Repository: "acme/bot-lora", Revision: strings.Repeat("a", 40)}}}
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, request := range map[string]CloudRequest{
		"unpinned weights":   {Name: "x", Model: "s3://acme-models/run", ModelRevision: "latest", Cloud: "kubernetes", GPU: "L4"},
		"serverless weights": {Name: "x", Model: "s3://acme-models/run", ModelRevision: testDigest, Cloud: "runpod", GPU: "L4", ComputeMode: "serverless"},
		"two adapters":       {Name: "x", Model: "a/b", Cloud: "kubernetes", GPU: "L4", ModelAdapters: append(valid.ModelAdapters, valid.ModelAdapters...)},
		"lora runtime flag":  {Name: "x", Model: "a/b", Cloud: "kubernetes", GPU: "L4", ModelAdapters: valid.ModelAdapters, RuntimeArgs: []string{"--enable-lora"}},
	} {
		if err := request.Validate(); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
}
