package main

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/infercrane/infercrane/internal/modelapiproduct"
	"github.com/infercrane/infercrane/internal/modelapisupply"
	"github.com/infercrane/infercrane/internal/store"
	"github.com/infercrane/infercrane/internal/supplieradapter"
)

const (
	productID       = supplieradapter.DeepSeekV4FlashModelID
	offerID         = "deepseek-direct-v4-flash"
	productionTerms = "https://api-docs.deepseek.com/quick_start/pricing"
)

type releaseConfig struct {
	QualificationPath   string
	CredentialReference string
	OperatorWorkspaceID string
	ServingPlanID       string
	CustomerWorkspaceID string
	OutputDirectory     string
	ReleaseVersion      int
}

type qualificationManifest struct {
	OfferID      string                               `json:"offer_id"`
	OfferVersion int64                                `json:"offer_version"`
	Evidence     modelapisupply.QualificationEvidence `json:"evidence"`
}

func main() {
	var qualificationPath, credentialReference, operatorWorkspaceID, servingPlanID, customerWorkspaceID, outputDirectory string
	var releaseVersion int
	flag.StringVar(&qualificationPath, "qualification", "", "secret-free qualification manifest from model-api-qualifier")
	flag.StringVar(&credentialReference, "credential-reference", "", "tenant-scoped secret reference id")
	flag.StringVar(&operatorWorkspaceID, "operator-workspace", "", "platform operator workspace id")
	flag.StringVar(&servingPlanID, "serving-plan", "", "existing operator-owned serving plan id")
	flag.StringVar(&customerWorkspaceID, "customer-workspace", "", "customer workspace receiving the production canary entitlement")
	flag.StringVar(&outputDirectory, "output-directory", "", "directory for generated production manifests")
	flag.IntVar(&releaseVersion, "release-version", 0, "positive immutable offer and retail-rate revision")
	flag.Parse()
	if qualificationPath == "" || credentialReference == "" || operatorWorkspaceID == "" || servingPlanID == "" || customerWorkspaceID == "" || outputDirectory == "" || releaseVersion <= 0 {
		fatal(errors.New("--qualification, --credential-reference, --operator-workspace, --serving-plan, --customer-workspace, --output-directory, and a positive --release-version are required"))
	}
	if err := runRelease(releaseConfig{
		QualificationPath: qualificationPath, CredentialReference: credentialReference,
		OperatorWorkspaceID: operatorWorkspaceID, ServingPlanID: servingPlanID,
		CustomerWorkspaceID: customerWorkspaceID, OutputDirectory: outputDirectory,
		ReleaseVersion: releaseVersion,
	}, time.Now().UTC().Truncate(time.Microsecond)); err != nil {
		fatal(err)
	}
}

func runRelease(cfg releaseConfig, now time.Time) error {
	qualification, err := readQualification(cfg.QualificationPath)
	if err != nil {
		return err
	}
	if err := validateQualification(now, int64(cfg.ReleaseVersion), qualification); err != nil {
		return err
	}
	if err := os.MkdirAll(cfg.OutputDirectory, 0o700); err != nil {
		return fmt.Errorf("create output directory: %w", err)
	}
	if err := os.Chmod(cfg.OutputDirectory, 0o700); err != nil {
		return fmt.Errorf("protect output directory: %w", err)
	}

	releaseSuffix := fmt.Sprintf("r%d", cfg.ReleaseVersion)
	retailRateID := fmt.Sprintf("%s-rate-%d", productID, cfg.ReleaseVersion)

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
		Version:                  cfg.ReleaseVersion,
		InputMicrousdPerMillion:  550_000,
		OutputMicrousdPerMillion: 1_650_000,
		PublishedAt:              now.Add(-2 * time.Minute),
		ValidFrom:                now.Add(-time.Minute),
		ValidUntil:               now.Add(30 * 24 * time.Hour),
		PublicProvenance:         modelapiproduct.CustomerRetailRateProvenance,
	})
	if err != nil {
		return err
	}

	costInput, costOutput := int64(440_000), int64(1_320_000)
	offer := modelapisupply.Offer{
		ID:                  offerID,
		Version:             int64(cfg.ReleaseVersion),
		OperatorTenantID:    cfg.OperatorWorkspaceID,
		ProductID:           productID,
		Supplier:            supplieradapter.DeepSeekSupplier,
		Adapter:             supplieradapter.DeepSeekAdapterName,
		SupplierModelID:     supplieradapter.DeepSeekV4FlashModelID,
		Protocol:            "openai",
		TupleKey:            qualification.Evidence.TupleKey,
		Region:              "global",
		CredentialReference: cfg.CredentialReference,
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
		return err
	}

	plan := store.SupplyPlanDraft{
		ID:               "supply-deepseek-v4-flash-production-" + releaseSuffix,
		OperatorTenantID: cfg.OperatorWorkspaceID,
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
			CandidateID:       "candidate-deepseek-direct-v4-flash-production-" + releaseSuffix,
			OfferID:           offerID,
			OfferVersion:      int64(cfg.ReleaseVersion),
			QualificationID:   qualification.Evidence.ID,
			RetailRateVersion: cfg.ReleaseVersion,
		}},
	}

	publication := modelapiproduct.OperatorPublication{
		SchemaVersion:       modelapiproduct.OperatorProjectionSchemaVersion,
		ProductID:           productID,
		OperatorWorkspaceID: cfg.OperatorWorkspaceID,
		ServingPlanID:       cfg.ServingPlanID,
		SupplyPlanID:        plan.ID,
		Qualification: modelapiproduct.RouteQualification{
			State:         modelapiproduct.QualificationQualified,
			EvidenceID:    qualification.Evidence.ID,
			EvidenceUntil: &evidenceUntil,
		},
		RetailRate: &rate,
		UpdatedAt:  now,
	}

	availableProduct := product
	availableProduct.Availability = modelapiproduct.AvailabilityAvailable
	if err := availableProduct.Validate(); err != nil {
		return err
	}
	if err := publication.ValidateAt(availableProduct, now); err != nil {
		return err
	}
	createdAt := now
	validFrom := now.Add(-time.Minute)
	validUntil := evidenceUntil
	entitlement := modelapiproduct.ProductEntitlement{
		SchemaVersion:       modelapiproduct.EntitlementSchemaVersion,
		ID:                  "entitlement-" + cfg.CustomerWorkspaceID + "-" + productID,
		CustomerWorkspaceID: cfg.CustomerWorkspaceID,
		ProductID:           productID,
		OperatorWorkspaceID: cfg.OperatorWorkspaceID,
		ServingPlanID:       cfg.ServingPlanID,
		RetailRateID:        retailRateID,
		RetailRateVersion:   cfg.ReleaseVersion,
		State:               modelapiproduct.EntitlementActive,
		Limits:              modelapiproduct.CustomerLimits{MaxRequestMicrousd: int64Pointer(2_000_000)},
		ValidFrom:           validFrom,
		ValidUntil:          &validUntil,
		CreatedAt:           createdAt,
		UpdatedAt:           createdAt,
	}
	if err := entitlement.Validate(); err != nil {
		return err
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
		if err := writeManifest(filepath.Join(cfg.OutputDirectory, manifest.name), manifest.value); err != nil {
			return err
		}
	}
	fmt.Printf("generated %d protected manifests in %s\n", len(manifests), cfg.OutputDirectory)
	return nil
}

func readQualification(path string) (qualificationManifest, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return qualificationManifest{}, fmt.Errorf("read qualification: %w", err)
	}
	var manifest qualificationManifest
	if err := json.Unmarshal(body, &manifest); err != nil {
		return qualificationManifest{}, fmt.Errorf("decode qualification: %w", err)
	}
	return manifest, nil
}

func validateQualification(now time.Time, releaseVersion int64, manifest qualificationManifest) error {
	if manifest.OfferID != offerID || manifest.OfferVersion != releaseVersion {
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
	expectedOrigin := fmt.Sprintf("%x", sha256.Sum256([]byte(supplieradapter.DeepSeekBaseURL)))
	for _, pin := range []string{
		"revision=" + supplieradapter.DeepSeekV4FlashRevision,
		"target_origin_sha256=" + expectedOrigin,
	} {
		if !slices.Contains(splitScope(manifest.Evidence.Scope), pin) {
			return fmt.Errorf("qualification scope is missing exact %s pin", pin)
		}
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
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	if _, err = file.Write(body); err != nil {
		_ = file.Close()
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err = file.Close(); err != nil {
		return fmt.Errorf("close %s: %w", path, err)
	}
	return os.Chmod(path, 0o600)
}

func splitScope(scope string) []string {
	return slices.DeleteFunc(strings.Split(scope, ";"), func(value string) bool { return value == "" })
}

func int64Pointer(value int64) *int64 { return &value }

func fatal(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
