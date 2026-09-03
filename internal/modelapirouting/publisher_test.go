package modelapirouting

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/modelapitarget"
	"github.com/infercrane/infercrane/internal/supplieradapter"
)

type sourceStoreFake struct {
	sources []RouteSource
	err     error
}

type referenceStoreFake struct {
	tenant, id string
	err        error
}

func (s *referenceStoreFake) SecretReferenceForTenant(_ context.Context, tenant, id string) (domain.SecretReference, error) {
	s.tenant, s.id = tenant, id
	if s.err != nil {
		return domain.SecretReference{}, s.err
	}
	return domain.SecretReference{ID: id, TenantID: tenant, Resolver: "env", Reference: "SUPPLIER_KEY"}, nil
}

type secretValueFake struct{}

func (secretValueFake) Resolve(context.Context, domain.SecretReference) (string, error) {
	return "secret", nil
}

type secretValueFailIfResolved struct{ calls int }

func (s *secretValueFailIfResolved) Resolve(context.Context, domain.SecretReference) (string, error) {
	s.calls++
	return "", errors.New("strict credentials must not resolve while publishing")
}

func (s *sourceStoreFake) PublishedModelAPIRoutes(context.Context, time.Time) ([]RouteSource, error) {
	return s.sources, s.err
}

type targetResolverFake struct {
	target ResolvedTarget
	err    error
	seen   []string
}

func (r *targetResolverFake) ResolveHostedModelTarget(_ context.Context, operator, supplier, adapter, reference string) (ResolvedTarget, error) {
	r.seen = append(r.seen, operator+"/"+supplier+"/"+adapter+"/"+reference)
	return r.target, r.err
}

func sourceFixture(now time.Time, tenant string) RouteSource {
	route := routeFixture(now, tenant)
	candidate := route.Candidates[0]
	candidate.Endpoint, candidate.Credential = "", ""
	return RouteSource{
		Entitlement: route.Entitlement,
		Publication: route.Publication,
		Rate:        route.Rate,
		Candidates:  []CandidateSource{{Candidate: candidate, Adapter: "openai", CredentialReference: "vault://hosted/glm"}},
	}
}

func TestPublisherResolvesSecretsOnlyAtTrustedBoundaryAndPublishesAtomically(t *testing.T) {
	now := time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC)
	directory := NewDirectory()
	directory.now = func() time.Time { return now }
	store := &sourceStoreFake{sources: []RouteSource{sourceFixture(now, "customer")}}
	resolver := &targetResolverFake{target: ResolvedTarget{Endpoint: "https://supplier.example/v1", Credential: "resolved-secret"}}
	publisher := &Publisher{Store: store, Resolver: resolver, Directory: directory, Now: func() time.Time { return now }}
	if err := publisher.PublishOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	lease, err := directory.Acquire("customer", "glm-5.3")
	if err != nil {
		t.Fatal(err)
	}
	if len(resolver.seen) != 1 || resolver.seen[0] != "operator/supplier/openai/vault://hosted/glm" || lease.Candidates[0].Credential != "resolved-secret" {
		t.Fatalf("resolver=%#v lease=%#v", resolver.seen, lease)
	}
}

func TestRegistryTargetResolverUsesConfiguredHTTPSAndTenantScopedSecretID(t *testing.T) {
	references := &referenceStoreFake{}
	resolver, err := NewRegistryTargetResolver(map[string]string{"supplier/openrouter": "https://openrouter.example/v1/"}, references, secretValueFake{})
	if err != nil {
		t.Fatal(err)
	}
	target, err := resolver.ResolveHostedModelTarget(context.Background(), "operator", "supplier", "openrouter", "secret-id")
	if err != nil || target.Endpoint != "https://openrouter.example/v1" || target.Credential != "secret" || references.tenant != "operator" || references.id != "secret-id" {
		t.Fatalf("target=%#v reference=%s/%s err=%v", target, references.tenant, references.id, err)
	}
	if _, err = NewRegistryTargetResolver(map[string]string{"supplier/bad": "http://plain.example/v1"}, references, secretValueFake{}); err == nil {
		t.Fatal("plaintext hosted endpoint was accepted")
	}
	if _, err = NewRegistryTargetResolver(map[string]string{"supplier/bad": ":// malformed"}, references, secretValueFake{}); err == nil {
		t.Fatal("malformed hosted endpoint was accepted")
	}
	if _, err = resolver.ResolveHostedModelTarget(context.Background(), "operator", "other-supplier", "openrouter", "secret-id"); err == nil {
		t.Fatal("supplier/adapter endpoint mismatch was accepted")
	}
}

func TestRegistryTargetResolverRechecksCredentialReferenceForEveryRequest(t *testing.T) {
	references := &referenceStoreFake{}
	resolver, err := NewRegistryTargetResolver(map[string]string{"supplier/adapter": "https://supplier.example/v1"}, references, secretValueFake{})
	if err != nil {
		t.Fatal(err)
	}
	credential, err := resolver.ResolveHostedModelCredential(context.Background(), "operator", "secret-id")
	if err != nil || string(credential) != "secret" {
		t.Fatalf("credential=%q err=%v", credential, err)
	}
	references.err = errors.New("credential reference deleted")
	if _, err = resolver.ResolveHostedModelCredential(context.Background(), "operator", "secret-id"); err == nil {
		t.Fatal("deleted credential reference remained callable from an in-memory route")
	}
}

func TestPublisherKeepsStrictCredentialReferenceSecretFree(t *testing.T) {
	now := time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC)
	directory := NewDirectory()
	directory.now = func() time.Time { return now }
	source := sourceFixture(now, "customer")
	source.Candidates[0].Candidate.Supplier = supplieradapter.DeepSeekSupplier
	source.Candidates[0].Candidate.SupplierModelID = supplieradapter.DeepSeekV4FlashModelID
	source.Candidates[0].Adapter = supplieradapter.DeepSeekAdapterName
	source.Candidates[0].CredentialReference = "strict-reference"
	references := &referenceStoreFake{}
	secrets := &secretValueFailIfResolved{}
	resolver, err := NewRegistryTargetResolver(map[string]string{
		supplieradapter.DeepSeekSupplier + "/" + supplieradapter.DeepSeekAdapterName: supplieradapter.DeepSeekBaseURL,
	}, references, secrets)
	if err != nil {
		t.Fatal(err)
	}
	publisher := &Publisher{
		Store: &sourceStoreFake{sources: []RouteSource{source}}, Resolver: resolver,
		Adapters: supplieradapter.DefaultRegistry(), Directory: directory, Now: func() time.Time { return now },
	}
	if err = publisher.PublishOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	lease, err := directory.Acquire("customer", "glm-5.3")
	if err != nil {
		t.Fatal(err)
	}
	candidate := lease.Candidates[0]
	if candidate.Endpoint != supplieradapter.DeepSeekBaseURL || candidate.Adapter != supplieradapter.DeepSeekAdapterName || candidate.CredentialReference != "strict-reference" || candidate.Credential != "" {
		t.Fatalf("strict route snapshot=%#v", candidate)
	}
	if secrets.calls != 0 {
		t.Fatalf("strict secret resolved %d times during publication", secrets.calls)
	}
}

func TestPublisherRequiresAndVerifiesRunPodTargetBinding(t *testing.T) {
	now := time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC)
	source := sourceFixture(now, "customer")
	candidateSource := &source.Candidates[0]
	candidateSource.Candidate.Supplier = supplieradapter.RunPodSupplier
	candidateSource.Candidate.SupplierModelID = "org/exact-model"
	candidateSource.Adapter = supplieradapter.RunPodVLLMAdapterName
	candidateSource.CredentialReference = "runpod-reference"
	endpointReference := supplieradapter.RunPodSupplier + "/" + supplieradapter.RunPodVLLMAdapterName
	endpoint := "https://api.runpod.ai/v2/abc123/openai"
	digest, err := modelapitarget.EndpointConfigDigest(endpointReference, endpoint)
	if err != nil {
		t.Fatal(err)
	}
	resolver, err := NewRegistryTargetResolver(map[string]string{endpointReference: endpoint}, &referenceStoreFake{}, &secretValueFailIfResolved{})
	if err != nil {
		t.Fatal(err)
	}
	directory := NewDirectory()
	directory.now = func() time.Time { return now }
	publisher := &Publisher{Store: &sourceStoreFake{sources: []RouteSource{source}}, Resolver: resolver, Adapters: supplieradapter.DefaultRegistry(), Directory: directory, Now: func() time.Time { return now }}
	if err = publisher.PublishOnce(context.Background()); err == nil {
		t.Fatal("RunPod route without an immutable target binding was published")
	}
	candidateSource.Candidate.TargetBindingID = "binding-1"
	candidateSource.Candidate.TargetBindingDigest = "sha256:" + strings.Repeat("a", 64)
	candidateSource.EndpointReference = endpointReference
	candidateSource.EndpointConfigDigest = digest
	if err = publisher.PublishOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	lease, err := directory.Acquire("customer", "glm-5.3")
	if err != nil || lease.Candidates[0].Endpoint != endpoint || lease.Candidates[0].TargetBindingID != "binding-1" {
		t.Fatalf("bound RunPod route=%#v err=%v", lease, err)
	}
	candidateSource.EndpointConfigDigest = "sha256:" + strings.Repeat("b", 64)
	if err = publisher.PublishOnce(context.Background()); err == nil {
		t.Fatal("RunPod route with a mismatched endpoint config digest was published")
	}
}

func TestPublisherRequiresImmutableBindingsForEveryMVPTarget(t *testing.T) {
	now := time.Date(2026, 9, 3, 10, 0, 0, 0, time.UTC)
	tests := []struct {
		name, supplier, adapter, model, endpoint string
	}{
		{"zai", supplieradapter.ZAISupplier, supplieradapter.ZAIAdapterName, supplieradapter.ZAIGLM53ModelID, supplieradapter.ZAIBaseURL},
		{"runpod-load-balanced", supplieradapter.RunPodSupplier, supplieradapter.RunPodSGLangLBAdapterName, supplieradapter.RunPodQwen38SupplierModelID, "https://qwen38pilot.api.runpod.ai"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			source := sourceFixture(now, "customer")
			source.Candidates[0].Candidate.Supplier = test.supplier
			source.Candidates[0].Candidate.SupplierModelID = test.model
			source.Candidates[0].Adapter = test.adapter
			source.Candidates[0].CredentialReference = "strict-reference"
			resolver, err := NewRegistryTargetResolver(map[string]string{test.supplier + "/" + test.adapter: test.endpoint}, &referenceStoreFake{}, &secretValueFailIfResolved{})
			if err != nil {
				t.Fatal(err)
			}
			publisher := &Publisher{
				Store: &sourceStoreFake{sources: []RouteSource{source}}, Resolver: resolver,
				Adapters: supplieradapter.DefaultRegistry(), Directory: NewDirectory(), Now: func() time.Time { return now },
			}
			if err = publisher.PublishOnce(context.Background()); err == nil || !strings.Contains(err.Error(), "immutable target binding") {
				t.Fatalf("unbound %s route error=%v", test.name, err)
			}
		})
	}
}

func TestPublisherRetainsPreviousGenerationOnSourceOrSecretFailure(t *testing.T) {
	now := time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC)
	directory := NewDirectory()
	directory.now = func() time.Time { return now }
	store := &sourceStoreFake{sources: []RouteSource{sourceFixture(now, "customer")}}
	resolver := &targetResolverFake{target: ResolvedTarget{Endpoint: "https://supplier.example/v1", Credential: "first-secret"}}
	publisher := &Publisher{Store: store, Resolver: resolver, Directory: directory, Now: func() time.Time { return now }}
	if err := publisher.PublishOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	store.err = errors.New("invalid durable generation")
	if err := publisher.PublishOnce(context.Background()); err == nil {
		t.Fatal("invalid source generation was accepted")
	}
	lease, err := directory.Acquire("customer", "glm-5.3")
	if err != nil || lease.Candidates[0].Credential != "first-secret" {
		t.Fatalf("known-good route lost after source failure: lease=%#v err=%v", lease, err)
	}
	store.err = nil
	resolver.err = errors.New("secret unavailable")
	if err := publisher.PublishOnce(context.Background()); err == nil {
		t.Fatal("unresolved secret generation was accepted")
	}
	lease, err = directory.Acquire("customer", "glm-5.3")
	if err != nil || lease.Candidates[0].Credential != "first-secret" {
		t.Fatalf("known-good route lost after resolver failure: lease=%#v err=%v", lease, err)
	}
	resolver.err = nil
	resolver.target.Endpoint = "http://plaintext.example/v1"
	if err := publisher.PublishOnce(context.Background()); err == nil {
		t.Fatal("non-HTTPS resolved endpoint was accepted")
	}
	lease, err = directory.Acquire("customer", "glm-5.3")
	if err != nil || lease.Candidates[0].Endpoint != "https://supplier.example/v1" {
		t.Fatalf("known-good route lost after endpoint validation failure: lease=%#v err=%v", lease, err)
	}
	resolver.target.Endpoint = "https://supplier.example/v1"
	store.sources = []RouteSource{sourceFixture(now.Add(-2*time.Hour), "customer")}
	if err := publisher.PublishOnce(context.Background()); err == nil {
		t.Fatal("expired generation was accepted")
	}
	lease, err = directory.Acquire("customer", "glm-5.3")
	if err != nil || lease.Candidates[0].Credential != "first-secret" {
		t.Fatalf("known-good route lost after expiry failure: lease=%#v err=%v", lease, err)
	}
}
