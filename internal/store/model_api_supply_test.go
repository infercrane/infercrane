package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/modelapiproduct"
	"github.com/infercrane/infercrane/internal/modelapisupply"
)

func TestModelAPISupplierOfferInsertHasOneValuePerColumn(t *testing.T) {
	columnsStart := strings.Index(modelAPISupplierOfferInsert, "(")
	columnsEnd := strings.Index(modelAPISupplierOfferInsert, ") VALUES(")
	valuesEnd := strings.Index(modelAPISupplierOfferInsert, ") ON CONFLICT")
	if columnsStart < 0 || columnsEnd < 0 || valuesEnd < 0 {
		t.Fatal("supplier offer insert shape is not parseable")
	}
	columnCount := strings.Count(modelAPISupplierOfferInsert[columnsStart+1:columnsEnd], ",") + 1
	valueCount := strings.Count(modelAPISupplierOfferInsert[columnsEnd:valuesEnd], "?")
	if columnCount != valueCount {
		t.Fatalf("supplier offer insert columns=%d placeholders=%d", columnCount, valueCount)
	}
}

func TestModelAPISupplyWritersCompileOnlyExactImmutableEvidence(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, ctx)
	suffix := strings.ReplaceAll(time.Now().UTC().Format("150405.000000000"), ".", "-")
	operator := "supply-operator-" + suffix
	if err := s.CreateTenant(ctx, operator, operator); err != nil {
		t.Fatal(err)
	}
	secret, err := s.CreateSecretReference(ctx, operator, "deepseek-"+suffix, "env", "DEEPSEEK_API_KEY")
	if err != nil {
		t.Fatal(err)
	}
	product, err := s.ManagedModelAPIProduct(ctx, "glm-5.2")
	if err != nil {
		t.Fatal(err)
	}
	var version int
	if err = s.QueryRowContext(ctx, `SELECT COALESCE(MAX(version),0)+1 FROM model_api_retail_rate_cards WHERE product_id=?`, product.ID).Scan(&version); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	rate, err := modelapiproduct.NewRetailRate(modelapiproduct.RetailRateDraft{
		ID: product.ID + "-supply-rate-" + suffix, ProductID: product.ID, Version: version,
		InputMicrousdPerMillion: 200_000, OutputMicrousdPerMillion: 600_000,
		PublishedAt: now.Add(-2 * time.Minute), ValidFrom: now.Add(-time.Minute), ValidUntil: now.Add(2 * time.Hour),
		PublicProvenance: "test retail contract",
	})
	if err != nil {
		t.Fatal(err)
	}
	if rate, err = s.PublishModelAPIRetailRate(ctx, rate); err != nil {
		t.Fatal(err)
	}
	costInput, costOutput := int64(100_000), int64(300_000)
	offer := modelapisupply.Offer{
		ID: "offer-" + suffix, Version: 1, OperatorTenantID: operator, ProductID: product.ID,
		Supplier: "fixture", Adapter: "openai", SupplierModelID: "fixture/model", Protocol: "openai",
		TupleKey: "sha256:" + strings.Repeat("a", 64), Region: "global", CredentialReference: secret.ID,
		State: modelapisupply.OfferActive, Capabilities: []string{"streaming", "chat-completions", "chat-completions"},
		Access: "ready", Availability: "available", Health: "healthy", ObservedAt: now,
		CostRate:   modelapisupply.CostRate{Currency: "USD", InputMicrousdPerMTok: &costInput, OutputMicrousdPerMTok: &costOutput, Provenance: "fixture contract", ValidFrom: now.Add(-time.Hour), ValidUntil: now.Add(time.Hour)},
		Commercial: modelapisupply.CommercialAuthorization{State: modelapisupply.CommercialReady, TermsRef: "contract://fixture", ValidUntil: now.Add(time.Hour)},
	}
	storedOffer, err := s.PublishModelAPISupplierOffer(ctx, operator, offer)
	if err != nil || len(storedOffer.Capabilities) != 2 {
		t.Fatalf("offer=%+v err=%v", storedOffer, err)
	}
	if _, err = s.PublishModelAPISupplierOffer(ctx, operator, offer); err != nil {
		t.Fatalf("exact offer replay failed: %v", err)
	}
	changed := offer
	changed.SupplierModelID = "fixture/changed"
	if _, err = s.PublishModelAPISupplierOffer(ctx, operator, changed); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflicting immutable offer error=%v", err)
	}
	evidence := modelapisupply.QualificationEvidence{
		ID: "qualification-" + suffix, State: modelapisupply.QualificationQualified,
		TupleKey: offer.TupleKey, Protocol: offer.Protocol, Region: offer.Region,
		Capabilities: []string{"chat-completions", "streaming"}, Scope: "chat buffered and streaming",
		EvidenceRef: "artifact://qualification/" + suffix, EvidenceDigest: "sha256:" + strings.Repeat("b", 64),
		ObservedAt: now, ValidUntil: now.Add(time.Hour), SampleCount: 8,
	}
	if _, err = s.PublishModelAPISupplyQualification(ctx, operator, offer.ID, offer.Version, evidence); err != nil {
		t.Fatal(err)
	}
	draft := SupplyPlanDraft{
		ID: "plan-" + suffix, OperatorTenantID: operator, ProductID: product.ID,
		Request: modelapisupply.Request{
			ModelID: product.ID, Protocol: "openai", Capabilities: []string{"chat-completions", "streaming"}, Region: "global",
			InputTokens: 1_000, OutputTokens: 500, MinimumGrossMarginBPS: modelapisupply.MinimumSafeGrossMarginBPS,
			MaximumObservationAge: time.Hour, MaximumFallbacks: 0, At: now,
		},
		Candidates: []SupplyCandidateReference{{CandidateID: "candidate-" + suffix, OfferID: offer.ID, OfferVersion: offer.Version, QualificationID: evidence.ID, RetailRateVersion: rate.Version}},
	}
	plan, err := s.CompileAndPublishModelAPISupplyPlan(ctx, draft)
	if err != nil || plan.Status != modelapisupply.StatusReady || plan.Primary == nil || plan.Primary.OfferID != offer.ID {
		t.Fatalf("plan=%+v err=%v", plan, err)
	}
	replayed, err := s.CompileAndPublishModelAPISupplyPlan(ctx, draft)
	if err != nil || replayed.Digest != plan.Digest {
		t.Fatalf("plan replay=%+v err=%v", replayed, err)
	}
	var candidateCount int
	if err = s.QueryRowContext(ctx, `SELECT COUNT(*) FROM model_api_supply_plan_candidates WHERE plan_id=?`, draft.ID).Scan(&candidateCount); err != nil || candidateCount != 1 {
		t.Fatalf("candidate count=%d err=%v", candidateCount, err)
	}
}
