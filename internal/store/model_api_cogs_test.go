package store

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/modelapiproduct"
	"github.com/infercrane/infercrane/internal/modelapiqualification"
	"github.com/infercrane/infercrane/internal/modelapirouting"
	"github.com/infercrane/infercrane/internal/modelapisupply"
	"github.com/infercrane/infercrane/internal/modelapitarget"
	"github.com/infercrane/infercrane/internal/supplieradapter"
)

func TestSupplierRateForOfferPinsExactImmutableCost(t *testing.T) {
	now := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	input, cached, output := int64(150_000), int64(20_000), int64(600_000)
	offer := modelapisupply.Offer{
		ID: "offer-1", Version: 7, TupleKey: "tuple-1", Supplier: "runpod", SupplierModelID: "org/model",
		CostRate: modelapisupply.CostRate{
			Currency: "USD", InputMicrousdPerMTok: &input, CachedInputMicrousdPerMTok: &cached,
			OutputMicrousdPerMTok: &output, Provenance: "invoice-contract-1",
			ValidFrom: now, ValidUntil: now.Add(time.Hour),
		},
	}
	rate, err := supplierRateForOffer(offer)
	if err != nil {
		t.Fatal(err)
	}
	if rate.ID != "offer-1/supplier-cost" || rate.Version != offer.Version || rate.OfferID != offer.ID ||
		rate.TupleKey != offer.TupleKey || !rate.HasCachedInputRate || rate.CachedInputMicrousdPerMillion != cached || rate.Digest == "" {
		t.Fatalf("supplier rate=%+v", rate)
	}
	offer.CostRate.OutputMicrousdPerMTok = nil
	if _, err = supplierRateForOffer(offer); err == nil {
		t.Fatal("incomplete supplier cost was accepted")
	}
}

func TestModelAPISettlementPersistsPinnedSupplierCOGS(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, ctx)
	suffix := strings.ReplaceAll(time.Now().UTC().Format("150405.000000000"), ".", "-")
	operator, customer := "cogs-operator-"+suffix, "cogs-customer-"+suffix
	for _, tenant := range []string{operator, customer} {
		if err := s.CreateTenant(ctx, tenant, tenant); err != nil {
			t.Fatal(err)
		}
	}
	secret, err := s.CreateSecretReference(ctx, operator, "cogs-secret-"+suffix, "env", "RUNPOD_API_KEY")
	if err != nil {
		t.Fatal(err)
	}

	product, err := s.ManagedModelAPIProduct(ctx, "glm-5.2")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	validUntil := now.Add(time.Hour)
	product.ID, product.DisplayName = "cogs-model-"+suffix, "COGS model"
	product.Availability = modelapiproduct.AvailabilityCatalogOnly
	for index := range product.Capabilities {
		if product.Capabilities[index].Name == "chat-completions" || product.Capabilities[index].Name == "streaming" {
			product.Capabilities[index].State = modelapiproduct.ClaimQualified
			product.Capabilities[index].EvidenceID = "capability-" + product.Capabilities[index].Name + "-" + suffix
			product.Capabilities[index].EvidenceUntil = &validUntil
		}
	}
	if product, err = s.SaveManagedModelAPIProduct(ctx, product); err != nil {
		t.Fatal(err)
	}

	target, err := s.AddTargetForTenant(ctx, operator, domain.Target{
		Name: "cogs-target-" + suffix, URL: "http://cogs-" + suffix + ":8000",
		Provider: "existing", Runtime: "vllm", UpstreamModel: product.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	deployment, err := s.ApplyDeploymentForTenant(ctx, operator, domain.Deployment{Name: "cogs-deployment-" + suffix, Model: product.ID}, []string{target.Name})
	if err != nil {
		t.Fatal(err)
	}
	var servingPlanID string
	if err = s.QueryRowContext(ctx, `SELECT active_serving_plan_id FROM endpoints WHERE tenant_id=? AND id=?`, operator, deployment.ID+"-endpoint").Scan(&servingPlanID); err != nil {
		t.Fatal(err)
	}

	rate, err := modelapiproduct.NewRetailRate(modelapiproduct.RetailRateDraft{
		ID: product.ID + "-rate", ProductID: product.ID, Version: 1,
		InputMicrousdPerMillion: 200_000, OutputMicrousdPerMillion: 600_000,
		PublishedAt: now.Add(-2 * time.Minute), ValidFrom: now.Add(-time.Minute), ValidUntil: validUntil,
		PublicProvenance: "integration retail rate",
	})
	if err != nil {
		t.Fatal(err)
	}
	if rate, err = s.PublishModelAPIRetailRate(ctx, rate); err != nil {
		t.Fatal(err)
	}

	inputCost, cachedCost, outputCost := int64(100_000), int64(20_000), int64(300_000)
	offer := modelapisupply.Offer{
		ID: "cogs-offer-" + suffix, Version: 1, OperatorTenantID: operator, ProductID: product.ID,
		Supplier: supplieradapter.RunPodSupplier, Adapter: supplieradapter.RunPodVLLMAdapterName,
		SupplierModelID: "org/cogs-model", Protocol: "openai", TupleKey: "sha256:" + strings.Repeat("c", 64),
		Region: "global", CredentialReference: secret.ID, State: modelapisupply.OfferActive,
		Capabilities: []string{"chat-completions", "streaming"}, Access: "ready", Availability: "available", Health: "healthy", ObservedAt: now,
		CostRate: modelapisupply.CostRate{
			Currency: "USD", InputMicrousdPerMTok: &inputCost, CachedInputMicrousdPerMTok: &cachedCost,
			OutputMicrousdPerMTok: &outputCost, Provenance: "integration supplier contract",
			ValidFrom: now.Add(-time.Minute), ValidUntil: validUntil,
		},
		Commercial: modelapisupply.CommercialAuthorization{State: modelapisupply.CommercialReady, TermsRef: "contract://runpod-test", ValidUntil: validUntil},
	}
	if _, err = s.PublishModelAPISupplierOffer(ctx, operator, offer); err != nil {
		t.Fatal(err)
	}
	endpointReference := offer.Supplier + "/" + offer.Adapter
	endpoint := "https://api.runpod.ai/v2/cogs-endpoint/openai"
	endpointDigest, err := modelapitarget.EndpointConfigDigest(endpointReference, endpoint)
	if err != nil {
		t.Fatal(err)
	}
	binding, err := modelapitarget.NewBinding(modelapitarget.Draft{
		ID: "cogs-binding-" + suffix, OperatorTenantID: operator, ProductID: product.ID,
		Kind: modelapitarget.KindServerlessGPU, OfferID: offer.ID, OfferVersion: offer.Version,
		Adapter: offer.Adapter, SupplierModelID: offer.SupplierModelID, EndpointReference: endpointReference,
		EndpointConfigDigest: endpointDigest, Region: offer.Region, CreatedAt: now.Add(-2 * time.Minute),
		ValidFrom: now.Add(-time.Minute), ValidUntil: validUntil,
	})
	if err != nil {
		t.Fatal(err)
	}
	if binding, err = s.PublishModelAPITargetBinding(ctx, operator, binding); err != nil {
		t.Fatal(err)
	}

	measurement, err := modelapiqualification.Measure(modelapiqualification.Target{
		TupleKey: offer.TupleKey, Supplier: offer.Supplier, Adapter: offer.Adapter, SupplierModelID: offer.SupplierModelID,
		Operation: "chat-completions", Protocol: offer.Protocol, Region: offer.Region, Capabilities: offer.Capabilities,
	}, []modelapiqualification.Sample{{
		RequestID: "cogs-qualification", StartedAt: now.Add(-3 * time.Second), FirstTokenAt: now.Add(-2 * time.Second),
		CompletedAt: now.Add(-time.Second), InputTokens: 1_000, OutputTokens: 500,
	}}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	evidence, err := s.PublishMeasuredModelAPISupplyQualification(ctx, operator, offer.ID, offer.Version, "cogs-qualification-"+suffix, "buffered and streaming", "artifact://cogs-test", measurement)
	if err != nil {
		t.Fatal(err)
	}
	planDraft := SupplyPlanDraft{
		ID: "cogs-plan-" + suffix, OperatorTenantID: operator, ProductID: product.ID,
		Request: modelapisupply.Request{
			ModelID: product.ID, Protocol: offer.Protocol, Capabilities: offer.Capabilities, Region: offer.Region,
			InputTokens: 1_000, OutputTokens: 500, MinimumGrossMarginBPS: modelapisupply.MinimumSafeGrossMarginBPS,
			MaximumObservationAge: time.Hour, MaximumEvidenceAge: time.Hour, MinimumEvidenceSamples: 1,
			MaximumFallbacks: 0, At: now,
		},
		Candidates: []SupplyCandidateReference{{
			CandidateID: "cogs-candidate-" + suffix, OfferID: offer.ID, OfferVersion: offer.Version,
			QualificationID: evidence.ID, RetailRateVersion: rate.Version, TrafficWeightBPS: 10_000,
		}},
	}
	plan, err := s.CompileAndPublishModelAPISupplyPlan(ctx, planDraft)
	if err != nil || plan.Primary == nil {
		t.Fatalf("plan=%+v err=%v", plan, err)
	}
	publication := modelapiproduct.OperatorPublication{
		SchemaVersion: modelapiproduct.OperatorProjectionSchemaVersion,
		ProductID:     product.ID, OperatorWorkspaceID: operator, ServingPlanID: servingPlanID, SupplyPlanID: planDraft.ID,
		Qualification: modelapiproduct.RouteQualification{State: modelapiproduct.QualificationQualified, EvidenceID: evidence.ID, EvidenceUntil: &validUntil},
		RetailRate:    &rate,
	}
	if _, err = s.SaveModelAPIOperatorPublication(ctx, operator, publication); err != nil {
		t.Fatal(err)
	}
	product.Availability = modelapiproduct.AvailabilityAvailable
	if _, err = s.SaveManagedModelAPIProduct(ctx, product); err != nil {
		t.Fatal(err)
	}
	maxRequest := int64(10_000)
	entitlement := modelapiproduct.ProductEntitlement{
		SchemaVersion:       modelapiproduct.EntitlementSchemaVersion,
		CustomerWorkspaceID: customer, ProductID: product.ID, OperatorWorkspaceID: operator,
		ServingPlanID: servingPlanID, RetailRateID: rate.ID, RetailRateVersion: rate.Version,
		State: modelapiproduct.EntitlementActive, Limits: modelapiproduct.CustomerLimits{MaxRequestMicrousd: &maxRequest},
		ValidFrom: now.Add(-time.Minute),
	}
	if entitlement, err = s.SaveModelAPIProductEntitlement(ctx, customer, entitlement); err != nil {
		t.Fatal(err)
	}
	if _, err = s.CreditManagedWallet(ctx, customer, "cogs-credit-"+suffix, "integration credit", 100_000); err != nil {
		t.Fatal(err)
	}

	reservation, err := s.ReserveModelAPIUsage(ctx, modelapirouting.ReservationRequest{
		ID: "cogs-reservation-" + suffix, TenantID: customer, ProductID: product.ID,
		EntitlementID: entitlement.ID, OperatorTenantID: operator, ServingPlanID: servingPlanID,
		SupplyPlanID: planDraft.ID, CandidateID: plan.Primary.CandidateID, OfferID: offer.ID, OfferVersion: offer.Version,
		Supplier: offer.Supplier, SupplierModelID: offer.SupplierModelID, TargetBindingID: binding.ID, TargetBindingDigest: binding.ContractDigest,
		RetailRate: modelapirouting.RetailRate{
			ID: rate.ID, ProductID: rate.ProductID, Version: rate.Version, ContractDigest: rate.ContractDigest,
			InputMicrousdPerMillion: rate.InputMicrousdPerMillion, OutputMicrousdPerMillion: rate.OutputMicrousdPerMillion,
			ValidFrom: rate.ValidFrom, ValidUntil: rate.ValidUntil,
		},
		MaxRequestMicrousd: maxRequest, CreatedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if reservation.SupplierRateDigest == "" || reservation.TargetBindingDigest != binding.ContractDigest {
		t.Fatalf("reservation did not pin supplier and target contracts: %+v", reservation)
	}
	if err = s.MarkModelAPIUsageTransmitted(ctx, customer, reservation.ID, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	inputTokens, cachedTokens, outputTokens := 1_000, 400, 500
	settled, err := s.SettleModelAPIUsage(ctx, customer, reservation.ID, modelapirouting.Usage{
		StatusCode: 200, InputTokens: &inputTokens, CachedInputTokens: &cachedTokens, OutputTokens: &outputTokens,
	})
	if err != nil || settled.State != "settled" {
		t.Fatalf("settled=%+v err=%v", settled, err)
	}
	cogs, err := s.ModelAPISupplierCOGS(ctx, operator, reservation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if cogs.SupplierRateDigest != reservation.SupplierRateDigest || cogs.CachedInputTokens != int64(cachedTokens) ||
		cogs.SupplierCOGSMicrousd <= 0 || cogs.RetailMicrousd != settled.ActualMicrousd || !cogs.GrossMarginDefined {
		t.Fatalf("supplier COGS does not reconcile the settled reservation: %+v", cogs)
	}
	if _, err = s.ModelAPISupplierCOGS(ctx, customer, reservation.ID); err == nil {
		t.Fatal("customer tenant read private supplier COGS")
	}
}
