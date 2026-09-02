package modelapirouting

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestHostedCandidateNeverSerializesCredentialOrEndpoint(t *testing.T) {
	body, err := json.Marshal(CandidateSource{
		Candidate:           Candidate{ID: "candidate", Endpoint: "https://private.example", Credential: "supplier-secret", Adapter: "strict-adapter", CredentialReference: "strict-reference"},
		Adapter:             "private-adapter",
		CredentialReference: "private-reference",
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "private.example") || strings.Contains(string(body), "supplier-secret") || strings.Contains(string(body), "private-adapter") || strings.Contains(string(body), "private-reference") || strings.Contains(string(body), "strict-adapter") || strings.Contains(string(body), "strict-reference") {
		t.Fatalf("private supply leaked through JSON: %s", body)
	}
}

func routeFixture(now time.Time, tenant string) PublishedRoute {
	until := now.Add(time.Hour)
	return PublishedRoute{
		Entitlement: Entitlement{
			ID: "entitlement", CustomerTenantID: tenant, ProductID: "glm-5.3", OperatorTenantID: "operator",
			ServingPlanID: "serving-plan", RetailRateID: "rate", RetailRateVersion: 1, State: "active",
			MaxRequestMicrousd: 10_000, ValidFrom: now.Add(-time.Hour), ValidUntil: &until,
		},
		Publication: Publication{
			ProductID: "glm-5.3", OperatorTenantID: "operator", ServingPlanID: "serving-plan", SupplyPlanID: "supply-plan",
			CompatibilityKey: "glm-5.3/revision/openai", EvidenceID: "evidence", EvidenceValidUntil: until, ValidUntil: until,
		},
		Rate: RetailRate{
			ID: "rate", ProductID: "glm-5.3", Version: 1, ContractDigest: "sha256:rate-one",
			InputMicrousdPerMillion: 1_400_000, OutputMicrousdPerMillion: 4_400_000,
			ValidFrom: now.Add(-time.Hour), ValidUntil: until,
		},
		Candidates: []Candidate{{
			ID: "primary", ProductID: "glm-5.3", OperatorTenantID: "operator", ServingPlanID: "serving-plan", SupplyPlanID: "supply-plan",
			OfferID: "offer", OfferVersion: 1, QualificationEvidenceID: "evidence", CompatibilityKey: "glm-5.3/revision/openai",
			Protocol: "openai", Operations: []string{"chat"}, Supplier: "supplier", SupplierModelID: "supplier/glm",
			Endpoint: "https://supplier.example/v1", Credential: "secret", Qualified: true, Available: true, ValidUntil: until,
		}},
	}
}

func TestDirectoryIsolatesTenantAndFailsClosedOnExpiry(t *testing.T) {
	now := time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC)
	directory := NewDirectory()
	directory.now = func() time.Time { return now }
	if err := directory.Publish([]PublishedRoute{routeFixture(now, "customer-a")}); err != nil {
		t.Fatal(err)
	}
	if _, err := directory.Acquire("customer-b", "glm-5.3"); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("cross-tenant acquire error = %v", err)
	}
	if _, err := directory.Acquire("customer-a", "glm-5.3"); err != nil {
		t.Fatalf("current route unavailable: %v", err)
	}
	directory.now = func() time.Time { return now.Add(2 * time.Hour) }
	if _, err := directory.Acquire("customer-a", "glm-5.3"); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("expired entitlement error = %v", err)
	}
}

func TestDirectoryRejectsServingPlanMismatchWithoutReplacingGeneration(t *testing.T) {
	now := time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC)
	directory := NewDirectory()
	directory.now = func() time.Time { return now }
	good := routeFixture(now, "customer")
	if err := directory.Publish([]PublishedRoute{good}); err != nil {
		t.Fatal(err)
	}
	bad := routeFixture(now, "customer")
	bad.Publication.ServingPlanID = "other-plan"
	if err := directory.Publish([]PublishedRoute{bad}); err == nil {
		t.Fatal("mismatched serving plan was published")
	}
	lease, err := directory.Acquire("customer", "glm-5.3")
	if err != nil || lease.Publication.ServingPlanID != "serving-plan" {
		t.Fatalf("known-good generation was lost: lease=%#v err=%v", lease, err)
	}
}

func TestDirectoryAllowsOnlyExactQualifiedFallbacks(t *testing.T) {
	now := time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC)
	directory := NewDirectory()
	directory.now = func() time.Time { return now }
	route := routeFixture(now, "customer")
	exact := route.Candidates[0]
	exact.ID, exact.OfferID = "exact-fallback", "offer-two"
	unsafe := exact
	unsafe.ID, unsafe.OfferID, unsafe.CompatibilityKey = "unsafe-fallback", "offer-three", "other-revision"
	unqualified := exact
	unqualified.ID, unqualified.OfferID, unqualified.Qualified = "unqualified", "offer-four", false
	route.Candidates = append(route.Candidates, exact, unsafe, unqualified)
	if err := directory.Publish([]PublishedRoute{route}); err != nil {
		t.Fatal(err)
	}
	lease, err := directory.Acquire("customer", "glm-5.3")
	if err != nil {
		t.Fatal(err)
	}
	if len(lease.Candidates) != 2 || lease.Candidates[0].ID != "primary" || lease.Candidates[1].ID != "exact-fallback" {
		t.Fatalf("safe candidates = %#v", lease.Candidates)
	}
}
