package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/infercrane/infercrane/internal/modelapiproduct"
	"github.com/infercrane/infercrane/internal/modelapisupply"
	"github.com/infercrane/infercrane/internal/store"
)

const (
	productID       = "deepseek-v4-flash"
	offerID         = "deepseek-direct-v4-flash"
	offerVersion    = int64(1)
	supplyPlanID    = "supply-deepseek-v4-flash-production-r1"
	retailRateID    = "deepseek-v4-flash-rate-1"
	retailVersion   = 1
	productionTerms = "https://api-docs.deepseek.com/quick_start/pricing"
)

type qualificationManifest struct {
	OfferID      string                               `json:"offer_id"`
	OfferVersion int64                                `json:"offer_version"`
	Evidence     modelapisupply.QualificationEvidence `json:"evidence"`
}

func main() {
	var qualificationPath, credentialReference, operatorWorkspaceID, servingPlanID, customerWorkspaceID, outputDirectory string
	flag.StringVar(&qualificationPath, "qualification", "", "secret-free qualification manifest from model-api-qualifier")
	flag.StringVar(&credentialReference, "credential-reference", "", "tenant-scoped secret reference id")
	flag.StringVar(&operatorWorkspaceID, "operator-workspace", "", "platform operator workspace id")
	flag.StringVar(&servingPlanID, "serving-plan", "", "existing operator-owned serving plan id")
	flag.StringVar(&customerWorkspaceID, "customer-workspace", "", "customer workspace receiving the production canary entitlement")
	flag.StringVar(&outputDirectory, "output-directory", "", "directory for generated production manifests")
	flag.Parse()
	if qualificationPath == "" || credentialReference == "" || operatorWorkspaceID == "" || servingPlanID == "" || customerWorkspaceID == "" || outputDirectory == "" {
		fatal(errors.New("--qualification, --credential-reference, --operator-workspace, --serving-plan, --customer-workspace, and --output-directory are required"))
	}

	qualification := readQualification(qualificationPath)
	now := time.Now().UTC().Truncate(time.Microsecond)
	if err := validateQualification(now, qualification); err != nil {
		fatal(err)
	}
	if err := os.MkdirAll(outputDirectory, 0o700); err != nil {
		fatal(fmt.Errorf("create output directory: %w", err))
	}
	if err := os.Chmod(outputDirectory, 0o700); err != nil {
		fatal(fmt.Errorf("protect output directory: %w", err))
	}

	contextWindow := int64(1_000_000)
	evidenceUntil := qualification.Evidence.ValidUntil.UTC()
	product := modelapiproduct.Product{
		SchemaVersion: modelapiproduct.ProductSchemaVersion,
		ID:            productID,
		DisplayName:   "DeepSeek-V4-Flash",
		Publisher:     "DeepSeek",
		Description:   "Fast 1M-context model for coding, reasoning, and high-throughput chat.",
		Protocol:      "openai",
		Tasks:         []string{"chat", "coding", "reasoning"},
		Capabilities: []modelapiproduct.CapabilityClaim{
			{Name: "chat-completions", State: modelapiproduct.ClaimQualified, EvidenceID: qualification.Evidence.ID, EvidenceUntil: &evidenceUntil},
			{Name: "streaming", State: modelapiproduct.ClaimQualified, EvidenceID: qualification.Evidence.ID, EvidenceUntil: &evidenceUntil},
		},
		InputModalities:     []string{"text"},
		OutputModalities:    []string{"text"},
		ContextWindowTokens: &contextWindow,
		Availability:        modelapiproduct.AvailabilityCatalogOnly,
		SelfHostEligibility: modelapiproduct.SelfHostIneligible,
	}
	if err := product.Validate(); err != nil {
		fatal(err)
	}

	rate, err := modelapiproduct.NewRetailRate(modelapiproduct.RetailRateDraft{
		ID:                       retailRateID,
		ProductID:                productID,
		Version:                  retailVersion,
		InputMicrousdPerMillion:  550_000,
		OutputMicrousdPerMillion: 1_650_000,
		PublishedAt:              now.Add(-2 * time.Minute),
		ValidFrom:                now.Add(-time.Minute),
		ValidUntil:               now.Add(30 * 24 * time.Hour),
		PublicProvenance:         "InferCrane retail rate v1; supplier cost basis observed from " + productionTerms,
	})
	if err != nil {
		fatal(err)
	}

	costInput, costOutput := int64(440_000), int64(1_320_000)
	offer := modelapisupply.Offer{
		ID:                  offerID,
		Version:             offerVersion,
		OperatorTenantID:    operatorWorkspaceID,
		ProductID:           productID,
		Supplier:            "deepseek",
		Adapter:             "openai",
		SupplierModelID:     productID,
		Protocol:            "openai",
		TupleKey:            qualification.Evidence.TupleKey,
		Region:              "global",
		CredentialReference: credentialReference,
		State:               modelapisupply.OfferActive,
		Capabilities:        []string{"chat-completions", "streaming"},
		Access:              "ready",
		Availability:        "available",
		Health:              "healthy",
		ObservedAt:          qualification.Evidence.ObservedAt.UTC(),
		CostRate: modelapisupply.CostRate{
			Currency:              "USD",
			InputMicrousdPerMTok:  &costInput,
			OutputMicrousdPerMTok: &costOutput,
			Provenance:            "DeepSeek published peak pricing; " + productionTerms,
			ValidFrom:             now.Add(-time.Hour),
			ValidUntil:            evidenceUntil,
		},
		Commercial: modelapisupply.CommercialAuthorization{
			State:      modelapisupply.CommercialReady,
			TermsRef:   productionTerms,
			ValidUntil: evidenceUntil,
		},
	}
	if err := offer.Validate(); err != nil {
		fatal(err)
	}

	plan := store.SupplyPlanDraft{
		ID:               supplyPlanID,
		OperatorTenantID: operatorWorkspaceID,
		ProductID:        productID,
		Request: modelapisupply.Request{
			ModelID:                productID,
			Protocol:               "openai",
			Capabilities:           []string{"chat-completions", "streaming"},
			Region:                 "global",
			InputTokens:            1_000,
			OutputTokens:           1_000,
			MinimumGrossMarginBPS:  2_000,
			MaximumObservationAge:  24 * time.Hour,
			MaximumEvidenceAge:     24 * time.Hour,
			MinimumEvidenceSamples: qualification.Evidence.SampleCount,
			MaximumFallbacks:       0,
			At:                     now,
		},
		Candidates: []store.SupplyCandidateReference{{
			CandidateID:       "candidate-deepseek-direct-v4-flash-production-r1",
			OfferID:           offerID,
			OfferVersion:      offerVersion,
			QualificationID:   qualification.Evidence.ID,
			RetailRateVersion: retailVersion,
		}},
	}

	publication := modelapiproduct.OperatorPublication{
		SchemaVersion:       modelapiproduct.OperatorProjectionSchemaVersion,
		ProductID:           productID,
		OperatorWorkspaceID: operatorWorkspaceID,
		ServingPlanID:       servingPlanID,
		SupplyPlanID:        supplyPlanID,
		Qualification: modelapiproduct.RouteQualification{
			State:         modelapiproduct.QualificationQualified,
			EvidenceID:    qualification.Evidence.ID,
			EvidenceUntil: &evidenceUntil,
		},
		RetailRate: &rate,
	}

	availableProduct := product
	availableProduct.Availability = modelapiproduct.AvailabilityAvailable
	createdAt := now
	validFrom := now.Add(-time.Minute)
	validUntil := evidenceUntil
	entitlement := modelapiproduct.ProductEntitlement{
		SchemaVersion:       modelapiproduct.EntitlementSchemaVersion,
		ID:                  "entitlement-infercrane-production-canary-deepseek-v4-flash",
		CustomerWorkspaceID: customerWorkspaceID,
		ProductID:           productID,
		OperatorWorkspaceID: operatorWorkspaceID,
		ServingPlanID:       servingPlanID,
		RetailRateID:        retailRateID,
		RetailRateVersion:   retailVersion,
		State:               modelapiproduct.EntitlementActive,
		Limits:              modelapiproduct.CustomerLimits{MaxRequestMicrousd: int64Pointer(2_000_000)},
		ValidFrom:           validFrom,
		ValidUntil:          &validUntil,
		CreatedAt:           createdAt,
		UpdatedAt:           createdAt,
	}

	manifests := []struct {
		name  string
		value any
	}{
		{"01-product-catalog-only.json", product},
		{"02-retail-rate.json", rate},
		{"03-supplier-offer.json", offer},
		{"04-qualification.json", qualification},
		{"05-supply-plan.json", plan},
		{"06-publication.json", publication},
		{"07-product-available.json", availableProduct},
		{"08-canary-entitlement.json", entitlement},
	}
	for _, manifest := range manifests {
		if err := writeManifest(filepath.Join(outputDirectory, manifest.name), manifest.value); err != nil {
			fatal(err)
		}
	}
	fmt.Printf("generated %d protected manifests in %s\n", len(manifests), outputDirectory)
}

func readQualification(path string) qualificationManifest {
	body, err := os.ReadFile(path)
	if err != nil {
		fatal(fmt.Errorf("read qualification: %w", err))
	}
	var manifest qualificationManifest
	if err := json.Unmarshal(body, &manifest); err != nil {
		fatal(fmt.Errorf("decode qualification: %w", err))
	}
	return manifest
}

func validateQualification(now time.Time, manifest qualificationManifest) error {
	if manifest.OfferID != offerID || manifest.OfferVersion != offerVersion {
		return errors.New("qualification does not match the immutable production offer revision")
	}
	if err := manifest.Evidence.Validate(); err != nil {
		return err
	}
	if manifest.Evidence.State != modelapisupply.QualificationQualified || manifest.Evidence.Protocol != "openai" || manifest.Evidence.Region != "global" {
		return errors.New("qualification is not a qualified global OpenAI contract")
	}
	if manifest.Evidence.TupleKey != "deepseek|deepseek-v4-flash|openai|global" {
		return errors.New("qualification tuple does not match the production DeepSeek route")
	}
	for _, capability := range []string{"chat-completions", "streaming"} {
		if !slices.Contains(manifest.Evidence.Capabilities, capability) {
			return fmt.Errorf("qualification is missing %s", capability)
		}
	}
	if !manifest.Evidence.ValidUntil.After(now.Add(time.Hour)) {
		return errors.New("qualification must remain current for at least one hour")
	}
	return nil
}

func writeManifest(path string, value any) error {
	body, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("encode %s: %w", path, err)
	}
	body = append(body, '\n')
	if err := os.WriteFile(path, body, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return os.Chmod(path, 0o600)
}

func int64Pointer(value int64) *int64 { return &value }

func fatal(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
