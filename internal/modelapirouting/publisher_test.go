package modelapirouting

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
)

type sourceStoreFake struct {
	sources []RouteSource
	err     error
}

type referenceStoreFake struct{ tenant, id string }

func (s *referenceStoreFake) SecretReferenceForTenant(_ context.Context, tenant, id string) (domain.SecretReference, error) {
	s.tenant, s.id = tenant, id
	return domain.SecretReference{ID: id, TenantID: tenant, Resolver: "env", Reference: "SUPPLIER_KEY"}, nil
}

type secretValueFake struct{}

func (secretValueFake) Resolve(context.Context, domain.SecretReference) (string, error) {
	return "secret", nil
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
