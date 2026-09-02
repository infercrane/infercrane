package modelapisupply

import (
	"reflect"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/modelapiproduct"
)

func TestMaterializeCandidateKeepsHuggingFaceMetadataAdvisory(t *testing.T) {
	at := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	offer := completeOffer(at)
	offer.Capabilities = []string{"streaming", "tool-calling"}
	offer.HuggingFace = &HuggingFaceProvenance{
		RepositoryID: "unreviewed/repository", Revision: "candidate", License: "unknown",
		SourceURL: "https://huggingface.co/unreviewed/repository", ObservedAt: at,
	}
	candidate, err := MaterializeCandidate(CandidateMaterialization{
		CandidateID: "offer-a-v1", Offer: offer,
		RetailRate: testRetailRate(t, at, at.Add(2*time.Hour)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if candidate.ModelID != offer.ProductID || candidate.SupplierModelID != offer.SupplierModelID {
		t.Fatalf("Hugging Face metadata changed exact identity: %+v", candidate)
	}
	if candidate.RetailRateID != "retail-rate" || candidate.RetailRateVersion != 1 {
		t.Fatalf("canonical retail rate identity was not materialized: %+v", candidate)
	}
	if !reflect.DeepEqual(candidate.Capabilities, []string{"streaming", "tool-calling"}) {
		t.Fatalf("Hugging Face metadata changed capabilities: %+v", candidate.Capabilities)
	}
	if candidate.RateValidUntil != at.Add(2*time.Hour) {
		t.Fatalf("candidate did not use the earliest private validity boundary: %v", candidate.RateValidUntil)
	}
}

func TestMaterializeCandidateDoesNotInventQualification(t *testing.T) {
	at := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	offer := completeOffer(at)
	offer.Qualification = nil
	candidate, err := MaterializeCandidate(CandidateMaterialization{
		CandidateID: "offer-a-v1", Offer: offer,
		RetailRate: testRetailRate(t, at, at.Add(time.Hour)),
	})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := Compile(baseRequest(at), []Candidate{candidate})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Rejections) != 1 || !contains(plan.Rejections[0].Reasons, ReasonQualificationAbsent) {
		t.Fatalf("unqualified offer became eligible: %+v", plan)
	}
}

func TestIncompleteSupplyMaterializesToExplicitPlannerRejections(t *testing.T) {
	at := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	offer := completeOffer(at)
	offer.CostRate = CostRate{Currency: "USD"}
	offer.Commercial = CommercialAuthorization{State: CommercialPending}
	offer.Qualification.State = QualificationPending
	offer.Qualification.EvidenceRef = ""
	offer.Qualification.EvidenceDigest = ""
	offer.Qualification.ObservedAt = time.Time{}
	offer.Qualification.ValidUntil = time.Time{}
	candidate, err := MaterializeCandidate(CandidateMaterialization{
		CandidateID: "pending-offer", Offer: offer,
		RetailRate: testRetailRate(t, at, at.Add(time.Hour)),
	})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := Compile(baseRequest(at), []Candidate{candidate})
	if err != nil {
		t.Fatal(err)
	}
	for _, reason := range []string{ReasonRateExpired, ReasonCommercialAuthorization, ReasonCostBasisAbsent, ReasonQualificationNotReady} {
		if !contains(plan.Rejections[0].Reasons, reason) {
			t.Fatalf("pending offer omitted %q: %+v", reason, plan.Rejections[0])
		}
	}
}

func testRetailRate(t *testing.T, at, until time.Time) modelapiproduct.RetailRate {
	t.Helper()
	rate, err := modelapiproduct.NewRetailRate(modelapiproduct.RetailRateDraft{
		ID: "retail-rate", ProductID: "logical-model", Version: 1,
		InputMicrousdPerMillion: 120_000, OutputMicrousdPerMillion: 480_000,
		PublishedAt: at.Add(-2 * time.Hour), ValidFrom: at.Add(-time.Hour), ValidUntil: until,
		PublicProvenance: "published contract fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	return rate
}

func TestOfferValidationRejectsRawOrIncompleteCommercialData(t *testing.T) {
	at := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	offer := completeOffer(at)
	offer.CostRate.OutputMicrousdPerMTok = nil
	if err := offer.Validate(); err == nil {
		t.Fatal("cost without provenance was accepted")
	}
	offer = completeOffer(at)
	offer.Commercial.TermsRef = ""
	if err := offer.Validate(); err == nil {
		t.Fatal("commercial authorization without terms reference was accepted")
	}
}

func completeOffer(at time.Time) Offer {
	costInput, costOutput := int64(80_000), int64(320_000)
	return Offer{
		ID: "offer-a", Version: 1, OperatorTenantID: "operator", ProductID: "logical-model",
		Supplier: "supplier-a", Adapter: "supplier-a-openai", SupplierModelID: "supplier-a/model",
		Protocol: "openai", TupleKey: "model@revision|supplier-a|eu", Region: "eu",
		CredentialReference: "secret://supplier-a", State: OfferActive,
		Capabilities: []string{"tool-calling", "streaming"}, Access: "ready", Availability: "available", Health: "healthy", ObservedAt: at.Add(-time.Minute),
		CostRate:   CostRate{Currency: "USD", InputMicrousdPerMTok: &costInput, OutputMicrousdPerMTok: &costOutput, Provenance: "contract-2026-09", ValidFrom: at.Add(-time.Hour), ValidUntil: at.Add(3 * time.Hour)},
		Commercial: CommercialAuthorization{State: CommercialReady, TermsRef: "msa-1", ValidUntil: at.Add(90 * time.Minute)},
		Qualification: &QualificationEvidence{
			ID: "qualification-a", State: QualificationQualified, TupleKey: "model@revision|supplier-a|eu", Protocol: "openai", Region: "eu",
			Capabilities: []string{"streaming", "tool-calling"}, Scope: "buffered, streaming, tools",
			EvidenceRef: "evidence://qualification-a", EvidenceDigest: "sha256:qualification", ObservedAt: at.Add(-time.Hour), ValidUntil: at.Add(time.Hour), SampleCount: 100,
		},
	}
}
