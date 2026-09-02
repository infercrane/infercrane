package modelapiproduct

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestDefaultCatalogIsExactlyTheStableSixAndMakesNoServiceClaim(t *testing.T) {
	products := DefaultCatalog()
	if err := ValidateCatalog(products); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"deepseek-v4-flash-0731-fast",
		"glm-5.2",
		"glm-5.3",
		"glm-5.3-flash",
		"kimi-k2.6",
		"kimi-k3",
	}
	got := make([]string, 0, len(products))
	for _, product := range products {
		got = append(got, product.ID)
		if product.Availability != AvailabilityCatalogOnly || product.ContextWindowTokens != nil || product.SelfHostEligibility != SelfHostUnknown {
			t.Fatalf("default product fabricated operational evidence: %+v", product)
		}
		for _, claim := range product.Capabilities {
			if claim.State != ClaimCataloged || claim.EvidenceID != "" || claim.EvidenceUntil != nil {
				t.Fatalf("default product fabricated qualified capability evidence: %+v", claim)
			}
		}
		projection, err := PublicProjectionAt(product, nil, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		if projection.Callable || projection.RetailRate != nil {
			t.Fatalf("catalog-only product became callable: %+v", projection)
		}
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("stable public product ids changed: got %v want %v", got, want)
	}
}

func TestRetailRateIsVersionedCurrentAndTamperEvident(t *testing.T) {
	published := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	validFrom := published.Add(time.Hour)
	validUntil := validFrom.Add(24 * time.Hour)
	rate, err := NewRetailRate(RetailRateDraft{
		ID: "glm-5.3-rate-1", ProductID: "glm-5.3", Version: 1,
		InputMicrousdPerMillion:  250_000,
		OutputMicrousdPerMillion: 750_000, ValidFrom: validFrom, ValidUntil: validUntil,
		PublishedAt: published, PublicProvenance: "InferCrane retail rate card",
	})
	if err != nil {
		t.Fatal(err)
	}
	if rate.ContractDigest == "" || !rate.CurrentAt(validFrom) || rate.CurrentAt(validUntil) {
		t.Fatalf("unexpected rate validity: %+v", rate)
	}
	duplicate, err := NewRetailRate(RetailRateDraft{
		ID: "glm-5.3-rate-1", ProductID: "glm-5.3", Version: 1,
		InputMicrousdPerMillion:  250_000,
		OutputMicrousdPerMillion: 750_000, ValidFrom: validFrom, ValidUntil: validUntil,
		PublishedAt: published, PublicProvenance: "InferCrane retail rate card",
	})
	if err != nil || duplicate.ContractDigest != rate.ContractDigest {
		t.Fatalf("rate digest must be deterministic: %v %+v", err, duplicate)
	}
	tampered := rate
	tampered.OutputMicrousdPerMillion++
	if err := tampered.Validate(); err == nil || tampered.CurrentAt(validFrom) {
		t.Fatal("mutated rate contract must fail closed")
	}
}

func TestRetailRateRejectsUnknownOrInvalidPublicPricing(t *testing.T) {
	now := time.Now().UTC()
	cached := int64(1)
	for name, draft := range map[string]RetailRateDraft{
		"missing input":                          {ID: "rate-1", ProductID: "glm-5.3", Version: 1, OutputMicrousdPerMillion: 1, ValidFrom: now, ValidUntil: now.Add(time.Hour), PublishedAt: now, PublicProvenance: "retail"},
		"unordered validity":                     {ID: "rate-1", ProductID: "glm-5.3", Version: 1, InputMicrousdPerMillion: 1, OutputMicrousdPerMillion: 1, ValidFrom: now, ValidUntil: now, PublishedAt: now, PublicProvenance: "retail"},
		"late publication":                       {ID: "rate-1", ProductID: "glm-5.3", Version: 1, InputMicrousdPerMillion: 1, OutputMicrousdPerMillion: 1, ValidFrom: now, ValidUntil: now.Add(time.Hour), PublishedAt: now.Add(time.Minute), PublicProvenance: "retail"},
		"cached input before settlement support": {ID: "rate-1", ProductID: "glm-5.3", Version: 1, InputMicrousdPerMillion: 1, CachedInputMicrousdPerMillion: &cached, OutputMicrousdPerMillion: 1, ValidFrom: now, ValidUntil: now.Add(time.Hour), PublishedAt: now, PublicProvenance: "retail"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := NewRetailRate(draft); err == nil {
				t.Fatal("expected invalid public pricing to be rejected")
			}
		})
	}
}

func TestPublicProjectionRequiresCurrentRateAndEvidenceAndRedactsOperatorData(t *testing.T) {
	now := time.Date(2030, 2, 1, 0, 0, 0, 0, time.UTC)
	product := productByID(t, "glm-5.3-flash")
	product.Availability = AvailabilityAvailable
	qualifyCapability(t, &product, "chat-completions", now.Add(time.Hour))
	qualifyCapability(t, &product, "streaming", now.Add(time.Hour))
	rate := testRate(t, product.ID, now.Add(-time.Hour), now.Add(time.Hour))
	evidenceUntil := now.Add(time.Hour)
	publication := OperatorPublication{
		SchemaVersion:       OperatorProjectionSchemaVersion,
		ProductID:           product.ID,
		OperatorWorkspaceID: "operator-secret-workspace",
		ServingPlanID:       "operator-secret-serving-plan",
		SupplyPlanID:        "operator-secret-supply-plan",
		Qualification:       RouteQualification{State: QualificationQualified, EvidenceID: "operator-secret-evidence", EvidenceUntil: &evidenceUntil},
		RetailRate:          &rate,
		UpdatedAt:           now,
	}
	unqualified := product
	unqualified.Capabilities = append([]CapabilityClaim(nil), product.Capabilities...)
	for index := range unqualified.Capabilities {
		unqualified.Capabilities[index].State = ClaimCataloged
		unqualified.Capabilities[index].EvidenceID = ""
		unqualified.Capabilities[index].EvidenceUntil = nil
	}
	if err := publication.ValidateAt(unqualified, now); err == nil {
		t.Fatal("route-level qualification made a product callable without per-operation evidence")
	}
	unqualifiedProjection, err := PublicProjectionAt(unqualified, &publication, now)
	if err != nil {
		t.Fatal(err)
	}
	if unqualifiedProjection.Callable {
		t.Fatal("public projection became callable without per-operation evidence")
	}
	missingStreaming := product
	missingStreaming.Capabilities = append([]CapabilityClaim(nil), product.Capabilities...)
	for index := range missingStreaming.Capabilities {
		if missingStreaming.Capabilities[index].Name == "streaming" {
			missingStreaming.Capabilities[index] = CapabilityClaim{Name: "streaming", State: ClaimCataloged}
		}
	}
	if err := publication.ValidateAt(missingStreaming, now); err == nil {
		t.Fatal("stream-capable product launched without streaming evidence")
	}
	missingStreamingProjection, err := PublicProjectionAt(missingStreaming, &publication, now)
	if err != nil {
		t.Fatal(err)
	}
	if missingStreamingProjection.Callable {
		t.Fatal("public projection became callable without streaming evidence")
	}
	if err := publication.ValidateAt(product, now); err != nil {
		t.Fatal(err)
	}
	projection, err := PublicProjectionAt(product, &publication, now)
	if err != nil {
		t.Fatal(err)
	}
	if !projection.Callable || projection.RetailRate == nil || projection.Qualification != QualificationQualified {
		t.Fatalf("qualified publication should be callable: %+v", projection)
	}
	body, err := json.Marshal(projection)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"operator-secret-workspace", "operator-secret-serving-plan", "operator-secret-supply-plan", "operator-secret-evidence"} {
		if strings.Contains(string(body), secret) {
			t.Fatalf("public projection leaked operator-private value %q: %s", secret, body)
		}
	}

	expiredEvidence := now
	publication.Qualification.EvidenceUntil = &expiredEvidence
	projection, err = PublicProjectionAt(product, &publication, now)
	if err != nil {
		t.Fatal(err)
	}
	if projection.Callable || projection.Availability != AvailabilityUnavailable || projection.Qualification != QualificationStale || projection.EvidenceValidUntil != nil {
		t.Fatalf("expired evidence must fail closed: %+v", projection)
	}
}

func TestProductEntitlementIsSharedSupplyContractWithSafeCustomerProjection(t *testing.T) {
	now := time.Date(2030, 3, 1, 0, 0, 0, 0, time.UTC)
	rpm, monthly, maximum := int64(60), int64(10_000_000), int64(100_000)
	entitlement := ProductEntitlement{
		SchemaVersion: EntitlementSchemaVersion,
		ID:            "entitlement-1", CustomerWorkspaceID: "customer-workspace", ProductID: "kimi-k3",
		OperatorWorkspaceID: "operator-private-workspace", ServingPlanID: "operator-private-plan",
		RetailRateID: "kimi-k3-rate-4", RetailRateVersion: 4, State: EntitlementActive,
		Limits:    CustomerLimits{MaxRequestMicrousd: &maximum},
		ValidFrom: now.Add(-time.Hour), CreatedAt: now.Add(-2 * time.Hour), UpdatedAt: now.Add(-time.Hour),
	}
	if !entitlement.ActiveAt(now) {
		t.Fatal("valid active entitlement should authorize its product")
	}
	projection, err := entitlement.CustomerProjection()
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(projection)
	if err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{"operator-private-workspace", "operator-private-plan", "kimi-k3-rate-4", "customer-workspace"} {
		if strings.Contains(string(body), private) {
			t.Fatalf("customer projection leaked private binding %q: %s", private, body)
		}
	}
	if projection.ProductID != entitlement.ProductID || projection.RetailRateVersion != entitlement.RetailRateVersion {
		t.Fatalf("customer projection lost public entitlement contract: %+v", projection)
	}
	entitlement.Limits.RequestsPerMinute = &rpm
	entitlement.Limits.MonthlySpendMicrousd = &monthly
	if entitlement.Limits.SupportedForAdmission() {
		t.Fatal("unsupported entitlement limits were marked enforceable")
	}
	projection, err = entitlement.CustomerProjection()
	if err != nil {
		t.Fatal(err)
	}
	*projection.Limits.RequestsPerMinute = 999
	if *entitlement.Limits.RequestsPerMinute == 999 {
		t.Fatal("customer projection shares mutable limit storage with trusted entitlement")
	}

	entitlement.OperatorWorkspaceID = entitlement.CustomerWorkspaceID
	if err := entitlement.Validate(); err == nil {
		t.Fatal("shared hosted entitlement must not collapse operator and customer tenancy")
	}
}

func qualifyCapability(t *testing.T, product *Product, name string, until time.Time) {
	t.Helper()
	for index := range product.Capabilities {
		if product.Capabilities[index].Name == name {
			product.Capabilities[index].State = ClaimQualified
			product.Capabilities[index].EvidenceID = "evidence-" + name
			product.Capabilities[index].EvidenceUntil = &until
			return
		}
	}
	t.Fatalf("missing capability %q", name)
}

func productByID(t *testing.T, id string) Product {
	t.Helper()
	for _, product := range DefaultCatalog() {
		if product.ID == id {
			return product
		}
	}
	t.Fatalf("missing default product %q", id)
	return Product{}
}

func testRate(t *testing.T, productID string, from, until time.Time) RetailRate {
	t.Helper()
	rate, err := NewRetailRate(RetailRateDraft{
		ID: productID + "-rate-1", ProductID: productID, Version: 1,
		InputMicrousdPerMillion: 100_000, OutputMicrousdPerMillion: 400_000,
		ValidFrom: from, ValidUntil: until, PublishedAt: from.Add(-time.Hour),
		PublicProvenance: "InferCrane retail rate card",
	})
	if err != nil {
		t.Fatal(err)
	}
	return rate
}
