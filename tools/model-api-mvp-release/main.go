// model-api-mvp-release converts fresh, secret-free qualification evidence
// into a reviewed sequence of immutable operator manifests. It is deliberately
// limited to the four launch profiles that InferCrane can operate directly.
//
// MVP note: hosted GLMs use direct Z.ai capacity and Qwen uses one provisional
// RunPod Serverless recipe because InferCrane does not yet have the capital to
// keep every catalog model warm on owned capacity. The stable customer model
// IDs and routing interface let those private targets change later.
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
	"github.com/infercrane/infercrane/internal/modelapitarget"
	"github.com/infercrane/infercrane/internal/store"
	"github.com/infercrane/infercrane/internal/supplieradapter"
)

const qwenRevision = "017b9c7af6b5689d5dd426a76e0bc077eb5ca20a"

type profile struct {
	Name, ProductID, DisplayName, Publisher, Description string
	Supplier, Adapter, SupplierModelID, OfferID          string
	Region, ExpectedTuple, ExpectedRevision              string
	EndpointReference, DefaultEndpoint                   string
	ContextWindow, CostInput, CostOutput, CostCached     int64
	Kind                                                 modelapitarget.Kind
	SelfHost                                             modelapiproduct.SelfHostEligibility
	Tasks                                                []string
}

type qualificationManifest struct {
	OfferID      string                               `json:"offer_id"`
	OfferVersion int64                                `json:"offer_version"`
	Evidence     modelapisupply.QualificationEvidence `json:"evidence"`
}

type config struct {
	Profile, QualificationPath, CredentialReference  string
	OperatorWorkspaceID, ServingPlanID               string
	CustomerWorkspaceID, OutputDirectory             string
	Endpoint, CommercialTermsRef                     string
	CostInput, CostOutput, RetailInput, RetailOutput int64
}

func main() {
	var cfg config
	flag.StringVar(&cfg.Profile, "profile", "", "glm-5.2, glm-5.3, glm-5.3-flash, or qwen3.8-27b-runpod")
	flag.StringVar(&cfg.QualificationPath, "qualification", "", "secret-free qualification manifest from model-api-qualifier")
	flag.StringVar(&cfg.CredentialReference, "credential-reference", "", "tenant-scoped secret reference id")
	flag.StringVar(&cfg.OperatorWorkspaceID, "operator-workspace", "", "platform operator workspace id")
	flag.StringVar(&cfg.ServingPlanID, "serving-plan", "", "existing operator-owned serving plan id")
	flag.StringVar(&cfg.CustomerWorkspaceID, "customer-workspace", "", "customer workspace receiving the canary entitlement")
	flag.StringVar(&cfg.Endpoint, "endpoint", "", "exact HTTPS endpoint origin; required for RunPod")
	flag.StringVar(&cfg.CommercialTermsRef, "commercial-terms-ref", "", "reviewed authorization or commercial terms reference")
	flag.StringVar(&cfg.OutputDirectory, "output-directory", "", "directory for generated manifests")
	flag.Int64Var(&cfg.CostInput, "cost-input-microusd-per-million", 0, "RunPod input COGS per million tokens")
	flag.Int64Var(&cfg.CostOutput, "cost-output-microusd-per-million", 0, "RunPod output COGS per million tokens")
	flag.Int64Var(&cfg.RetailInput, "retail-input-microusd-per-million", 0, "RunPod launch retail input rate")
	flag.Int64Var(&cfg.RetailOutput, "retail-output-microusd-per-million", 0, "RunPod launch retail output rate")
	flag.Parse()
	if err := run(cfg, time.Now().UTC().Truncate(time.Microsecond)); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(cfg config, now time.Time) error {
	p, err := launchProfile(cfg.Profile)
	if err != nil {
		return err
	}
	if strings.TrimSpace(cfg.QualificationPath) == "" || strings.TrimSpace(cfg.CredentialReference) == "" || strings.TrimSpace(cfg.OperatorWorkspaceID) == "" || strings.TrimSpace(cfg.ServingPlanID) == "" || strings.TrimSpace(cfg.CustomerWorkspaceID) == "" || strings.TrimSpace(cfg.OutputDirectory) == "" || strings.TrimSpace(cfg.CommercialTermsRef) == "" {
		return errors.New("qualification, credential reference, operator workspace, serving plan, customer workspace, commercial terms reference, and output directory are required")
	}
	endpoint := p.DefaultEndpoint
	if cfg.Endpoint != "" {
		endpoint = cfg.Endpoint
	}
	if p.Kind == modelapitarget.KindServerlessGPU && cfg.Endpoint == "" {
		return errors.New("RunPod release requires the exact qualified --endpoint origin")
	}
	if p.Kind == modelapitarget.KindServerlessGPU {
		if cfg.CostInput <= 0 || cfg.CostOutput <= 0 || cfg.RetailInput <= 0 || cfg.RetailOutput <= 0 {
			return errors.New("RunPod release requires positive measured COGS and reviewed retail rates")
		}
		p.CostInput, p.CostOutput, p.CostCached = cfg.CostInput, cfg.CostOutput, 0
	} else {
		if cfg.CostInput != 0 || cfg.CostOutput != 0 || cfg.RetailInput != 0 || cfg.RetailOutput != 0 {
			return errors.New("Z.ai launch pricing is pinned by profile; RunPod-only pricing flags are not accepted")
		}
		cfg.RetailInput, cfg.RetailOutput = p.CostInput, p.CostOutput
	}
	qualification, err := readQualification(cfg.QualificationPath)
	if err != nil {
		return err
	}
	if err = validateQualification(now, p, endpoint, qualification); err != nil {
		return err
	}
	if err = os.MkdirAll(cfg.OutputDirectory, 0o700); err != nil {
		return fmt.Errorf("create output directory: %w", err)
	}
	if err = os.Chmod(cfg.OutputDirectory, 0o700); err != nil {
		return fmt.Errorf("protect output directory: %w", err)
	}

	evidenceUntil := qualification.Evidence.ValidUntil.UTC()
	validUntil := minTime(evidenceUntil, now.Add(7*24*time.Hour))
	product := modelapiproduct.Product{
		SchemaVersion: modelapiproduct.ProductSchemaVersion, ID: p.ProductID, DisplayName: p.DisplayName,
		Publisher: p.Publisher, Description: p.Description, Protocol: "openai", Tasks: p.Tasks,
		Capabilities: []modelapiproduct.CapabilityClaim{
			{Name: "chat-completions", State: modelapiproduct.ClaimQualified, EvidenceID: qualification.Evidence.ID, EvidenceUntil: &evidenceUntil},
			{Name: "streaming", State: modelapiproduct.ClaimQualified, EvidenceID: qualification.Evidence.ID, EvidenceUntil: &evidenceUntil},
		},
		InputModalities: []string{"text"}, OutputModalities: []string{"text"}, ContextWindowTokens: &p.ContextWindow,
		Availability: modelapiproduct.AvailabilityCatalogOnly, SelfHostEligibility: p.SelfHost,
	}
	if err = product.Validate(); err != nil {
		return err
	}
	rateID := p.ProductID + "-launch-rate-1"
	rate, err := modelapiproduct.NewRetailRate(modelapiproduct.RetailRateDraft{
		ID: rateID, ProductID: p.ProductID, Version: 1,
		InputMicrousdPerMillion: cfg.RetailInput, OutputMicrousdPerMillion: cfg.RetailOutput,
		PublishedAt: now.Add(-2 * time.Minute), ValidFrom: now.Add(-time.Minute), ValidUntil: validUntil,
		PublicProvenance: modelapiproduct.CustomerRetailRateProvenance,
	})
	if err != nil {
		return err
	}
	costInput, costOutput := p.CostInput, p.CostOutput
	var cached *int64
	if p.CostCached > 0 {
		cached = int64Pointer(p.CostCached)
	}
	offer := modelapisupply.Offer{
		ID: p.OfferID, Version: 1, OperatorTenantID: cfg.OperatorWorkspaceID, ProductID: p.ProductID,
		Supplier: p.Supplier, Adapter: p.Adapter, SupplierModelID: p.SupplierModelID, Protocol: "openai",
		TupleKey: p.ExpectedTuple, Region: p.Region, CredentialReference: cfg.CredentialReference,
		State: modelapisupply.OfferActive, Capabilities: []string{"chat-completions", "streaming"},
		Access: "ready", Availability: "available", Health: "healthy", ObservedAt: qualification.Evidence.ObservedAt.UTC(),
		CostRate: modelapisupply.CostRate{Currency: "USD", InputMicrousdPerMTok: &costInput, OutputMicrousdPerMTok: &costOutput,
			CachedInputMicrousdPerMTok: cached, Provenance: "operator-reviewed MVP supplier cost contract", ValidFrom: now.Add(-time.Hour), ValidUntil: validUntil},
		Commercial: modelapisupply.CommercialAuthorization{State: modelapisupply.CommercialReady, TermsRef: cfg.CommercialTermsRef, ValidUntil: validUntil},
	}
	if err = offer.Validate(); err != nil {
		return err
	}
	digest, err := modelapitarget.EndpointConfigDigest(p.EndpointReference, endpoint)
	if err != nil {
		return err
	}
	binding, err := modelapitarget.NewBinding(modelapitarget.Draft{
		ID: "target-" + p.ProductID + "-r1", OperatorTenantID: cfg.OperatorWorkspaceID, ProductID: p.ProductID,
		Kind: p.Kind, OfferID: p.OfferID, OfferVersion: 1, Adapter: p.Adapter, SupplierModelID: p.SupplierModelID,
		EndpointReference: p.EndpointReference, EndpointConfigDigest: digest, Region: p.Region,
		CreatedAt: now.Add(-2 * time.Minute), ValidFrom: now.Add(-time.Minute), ValidUntil: validUntil,
	})
	if err != nil {
		return err
	}
	planID := "supply-" + p.ProductID + "-launch-r1"
	plan := store.SupplyPlanDraft{
		ID: planID, OperatorTenantID: cfg.OperatorWorkspaceID, ProductID: p.ProductID,
		Request: modelapisupply.Request{ModelID: p.ProductID, Protocol: "openai", Capabilities: []string{"chat-completions", "streaming"}, Region: p.Region,
			InputTokens: 1_000, OutputTokens: 1_000, MinimumGrossMarginBPS: 0, PricingPolicy: modelapisupply.PricingPolicyLaunchParity,
			MaximumObservationAge: 24 * time.Hour, MaximumEvidenceAge: 24 * time.Hour,
			MinimumEvidenceSamples: qualification.Evidence.SampleCount, MaximumFallbacks: 0, At: now},
		Candidates: []store.SupplyCandidateReference{{CandidateID: "candidate-" + p.ProductID + "-launch-r1", OfferID: p.OfferID, OfferVersion: 1,
			QualificationID: qualification.Evidence.ID, RetailRateVersion: 1, TrafficWeightBPS: 10_000}},
	}
	publication := modelapiproduct.OperatorPublication{
		SchemaVersion: modelapiproduct.OperatorProjectionSchemaVersion, ProductID: p.ProductID,
		OperatorWorkspaceID: cfg.OperatorWorkspaceID, ServingPlanID: cfg.ServingPlanID, SupplyPlanID: planID,
		Qualification: modelapiproduct.RouteQualification{State: modelapiproduct.QualificationQualified, EvidenceID: qualification.Evidence.ID, EvidenceUntil: &evidenceUntil},
		RetailRate:    &rate, UpdatedAt: now,
	}
	availableProduct := product
	availableProduct.Availability = modelapiproduct.AvailabilityAvailable
	entitlement := modelapiproduct.ProductEntitlement{
		SchemaVersion: modelapiproduct.EntitlementSchemaVersion, ID: "entitlement-" + cfg.CustomerWorkspaceID + "-" + p.ProductID,
		CustomerWorkspaceID: cfg.CustomerWorkspaceID, ProductID: p.ProductID, OperatorWorkspaceID: cfg.OperatorWorkspaceID,
		ServingPlanID: cfg.ServingPlanID, RetailRateID: rateID, RetailRateVersion: 1, State: modelapiproduct.EntitlementActive,
		Limits:    modelapiproduct.CustomerLimits{MaxRequestMicrousd: int64Pointer(2_000_000)},
		ValidFrom: now.Add(-time.Minute), ValidUntil: &validUntil, CreatedAt: now, UpdatedAt: now,
	}
	if err = entitlement.Validate(); err != nil {
		return err
	}
	if err = publication.ValidateAt(availableProduct, now); err != nil {
		return err
	}
	manifests := []struct {
		name string
		body any
	}{
		{"01-product-catalog-only.json", product}, {"02-retail-rate.json", rate}, {"03-supplier-offer.json", offer},
		{"04-qualification.json", qualification}, {"05-target-binding.json", binding}, {"06-supply-plan.json", plan},
		{"07-publication.json", publication}, {"08-product-available.json", availableProduct}, {"09-canary-entitlement.json", entitlement},
	}
	for _, manifest := range manifests {
		if err = writeManifest(filepath.Join(cfg.OutputDirectory, manifest.name), manifest.body); err != nil {
			return err
		}
	}
	return nil
}

func launchProfile(name string) (profile, error) {
	zai := func(productID, display, model, description string, costIn, costOut, costCached int64, tasks []string) profile {
		return profile{Name: productID, ProductID: productID, DisplayName: display, Publisher: "Z.ai", Description: description,
			Supplier: supplieradapter.ZAISupplier, Adapter: supplieradapter.ZAIAdapterName, SupplierModelID: model,
			OfferID: "zai-" + strings.ReplaceAll(productID, ".", "-"), Region: "global",
			ExpectedTuple: strings.Join([]string{supplieradapter.ZAISupplier, model, "openai", "global"}, "|"), ExpectedRevision: model,
			EndpointReference: supplieradapter.ZAISupplier + "/" + supplieradapter.ZAIAdapterName, DefaultEndpoint: supplieradapter.ZAIBaseURL,
			ContextWindow: 1_000_000, CostInput: costIn, CostOutput: costOut, CostCached: costCached,
			Kind: modelapitarget.KindUpstream, SelfHost: modelapiproduct.SelfHostIneligible, Tasks: tasks}
	}
	switch name {
	case "glm-5.2":
		return zai(name, "GLM-5.2", supplieradapter.ZAIGLM52ModelID, "Long-context model for coding, reasoning, and bilingual chat.", 1_400_000, 4_400_000, 260_000, []string{"coding", "reasoning", "chat"}), nil
	case "glm-5.3":
		return zai(name, "GLM-5.3", supplieradapter.ZAIGLM53ModelID, "Long-context model for reasoning and complex workloads.", 1_400_000, 4_400_000, 260_000, []string{"reasoning", "long-context", "chat"}), nil
	case "glm-5.3-flash":
		return zai(name, "GLM-5.3-Flash", supplieradapter.ZAIGLM53FlashModelID, "Fast model for text chat and coding workloads.", 150_000, 500_000, 30_000, []string{"chat", "coding"}), nil
	case "qwen3.8-27b-runpod":
		return profile{Name: name, ProductID: "qwen3.8-27b", DisplayName: "Qwen3.8 27B", Publisher: "Qwen", Description: "InferCrane-optimized Qwen model for fast text chat.",
			Supplier: supplieradapter.RunPodSupplier, Adapter: supplieradapter.RunPodSGLangLBAdapterName, SupplierModelID: supplieradapter.RunPodQwen38SupplierModelID,
			OfferID: "runpod-qwen38-sglang", Region: "EU-NL-1", ExpectedTuple: strings.Join([]string{supplieradapter.RunPodSupplier, supplieradapter.RunPodQwen38SupplierModelID, "openai", "EU-NL-1"}, "|"),
			ExpectedRevision: qwenRevision, EndpointReference: supplieradapter.RunPodSupplier + "/" + supplieradapter.RunPodSGLangLBAdapterName,
			ContextWindow: supplieradapter.RunPodQwen38QualifiedContext, Kind: modelapitarget.KindServerlessGPU,
			SelfHost: modelapiproduct.SelfHostEligible, Tasks: []string{"chat", "coding"}}, nil
	default:
		return profile{}, fmt.Errorf("unknown launch profile %q", name)
	}
}

func readQualification(path string) (qualificationManifest, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return qualificationManifest{}, fmt.Errorf("read qualification: %w", err)
	}
	var manifest qualificationManifest
	if err = json.Unmarshal(body, &manifest); err != nil {
		return qualificationManifest{}, fmt.Errorf("decode qualification: %w", err)
	}
	return manifest, nil
}

func validateQualification(now time.Time, p profile, endpoint string, manifest qualificationManifest) error {
	if manifest.OfferID != p.OfferID || manifest.OfferVersion != 1 {
		return errors.New("qualification does not match the immutable launch offer revision")
	}
	if err := manifest.Evidence.Validate(); err != nil {
		return err
	}
	if manifest.Evidence.State != modelapisupply.QualificationQualified || manifest.Evidence.Protocol != "openai" || manifest.Evidence.Region != p.Region || manifest.Evidence.TupleKey != p.ExpectedTuple {
		return errors.New("qualification does not match the exact launch tuple")
	}
	for _, capability := range []string{"chat-completions", "streaming"} {
		if !slices.Contains(manifest.Evidence.Capabilities, capability) {
			return fmt.Errorf("qualification is missing %s", capability)
		}
	}
	if !manifest.Evidence.ValidUntil.After(now.Add(time.Hour)) {
		return errors.New("qualification must remain current for at least one hour")
	}
	if !strings.Contains(manifest.Evidence.Scope, "revision="+p.ExpectedRevision+";") {
		return errors.New("qualification scope does not pin the expected model revision")
	}
	if !strings.Contains(manifest.Evidence.Scope, "target_origin_sha256="+digestString(endpoint)) {
		return errors.New("qualification scope does not bind the expected supplier origin")
	}
	return nil
}

func writeManifest(path string, value any) error {
	body, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("encode %s: %w", path, err)
	}
	body = append(body, '\n')
	if err = os.WriteFile(path, body, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return os.Chmod(path, 0o600)
}

func int64Pointer(value int64) *int64 { return &value }

func digestString(value string) string {
	sum := sha256.Sum256([]byte(value))
	return fmt.Sprintf("%x", sum[:])
}

func minTime(left, right time.Time) time.Time {
	if left.Before(right) {
		return left
	}
	return right
}
