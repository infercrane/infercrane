package modelapisupply

import (
	"reflect"
	"testing"
	"time"
)

func TestCompileSelectsCheapestExactProfitableTupleAndDiverseFallback(t *testing.T) {
	at := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	request := baseRequest(at)
	request.InputTokens = 10_000
	request.OutputTokens = 2_000
	candidates := []Candidate{
		candidate("z-secondary", "supplier-a", 120_000, 480_000, at),
		candidate("a-primary", "supplier-a", 100_000, 400_000, at),
		candidate("b-diverse", "supplier-b", 110_000, 440_000, at),
	}
	plan, err := Compile(request, candidates)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status != StatusReady || plan.Primary == nil || plan.Primary.CandidateID != "a-primary" {
		t.Fatalf("unexpected primary: %+v", plan)
	}
	if len(plan.Fallbacks) != 2 || plan.Fallbacks[0].CandidateID != "b-diverse" || plan.Fallbacks[1].CandidateID != "z-secondary" {
		t.Fatalf("expected supplier-diverse fallback first: %+v", plan.Fallbacks)
	}
	if plan.RankingBasis != "estimated_customer_cost_for_declared_workload" || plan.Primary.EstimatedRetailMicrousd == nil || plan.Digest == "" {
		t.Fatalf("plan omitted cost basis or digest: %+v", plan)
	}
	if plan.ValidUntil.IsZero() || !plan.ValidUntil.After(at) {
		t.Fatalf("ready plan omitted its evidence expiry: %+v", plan)
	}
	if !plan.HasCanonicalDigest() {
		t.Fatal("compiled plan did not verify against its canonical digest")
	}

	plan.RankingBasis = "tampered"
	if plan.HasCanonicalDigest() {
		t.Fatal("mutated plan retained a valid canonical digest")
	}
}

func TestCompileFailsClosedWithoutComparableEvidence(t *testing.T) {
	at := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	request := baseRequest(at)
	maximumTTFT := 500.0
	request.MaximumTTFTP95MS = &maximumTTFT
	request.MaximumEvidenceAge = time.Hour
	request.MinimumEvidenceSamples = 30
	candidate := candidate("only", "supplier-a", 100_000, 400_000, at)
	plan, err := Compile(request, []Candidate{candidate})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status != StatusInsufficient || plan.Primary != nil || len(plan.Rejections) != 1 || !reflect.DeepEqual(plan.Rejections[0].Reasons, []string{ReasonCapacityEvidenceAbsent}) {
		t.Fatalf("missing evidence did not fail closed: %+v", plan)
	}

	value := 400.0
	candidate.Evidence = &CapacityEvidence{TupleKey: "different-tuple", ObservedAt: at.Add(-time.Minute), SampleCount: 100, TTFTP95MS: &value}
	plan, err = Compile(request, []Candidate{candidate})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(plan.Rejections[0].Reasons, []string{ReasonCapacityEvidenceMismatched}) {
		t.Fatalf("tuple mismatch was not explicit: %+v", plan.Rejections)
	}
}

func TestCompileRejectsSilentSubstitutionStaleHealthAndBadMargin(t *testing.T) {
	at := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	request := baseRequest(at)
	wrongModel := candidate("wrong-model", "supplier-a", 100_000, 400_000, at)
	wrongModel.ModelID = "different-model"
	stale := candidate("stale", "supplier-b", 100_000, 400_000, at)
	stale.ObservedAt = at.Add(-2 * time.Hour)
	badMargin := candidate("bad-margin", "supplier-c", 100_000, 400_000, at)
	badMargin.CostInputMicrousdPerMTok = int64Pointer(95_000)
	badMargin.CostOutputMicrousdPerMTok = int64Pointer(390_000)

	plan, err := Compile(request, []Candidate{stale, wrongModel, badMargin})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status != StatusInsufficient || len(plan.Rejections) != 3 {
		t.Fatalf("unsafe candidates became routable: %+v", plan)
	}
	want := map[string]string{"bad-margin": ReasonMarginBelowFloor, "stale": ReasonObservationStale, "wrong-model": ReasonModelMismatch}
	for _, rejection := range plan.Rejections {
		if !contains(rejection.Reasons, want[rejection.CandidateID]) {
			t.Fatalf("missing rejection reason for %s: %+v", rejection.CandidateID, rejection.Reasons)
		}
	}
}

func TestCompileIsDeterministicAndDoesNotInventCostWithoutWorkloadShape(t *testing.T) {
	at := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	request := baseRequest(at)
	first := candidate("a", "supplier-a", 100_000, 400_000, at)
	second := candidate("b", "supplier-b", 90_000, 360_000, at)
	left, err := Compile(request, []Candidate{second, first})
	if err != nil {
		t.Fatal(err)
	}
	right, err := Compile(request, []Candidate{first, second})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(left, right) || left.Digest != right.Digest {
		t.Fatalf("plan changed with input ordering:\nleft=%+v\nright=%+v", left, right)
	}
	if left.Primary == nil || left.Primary.CandidateID != "a" || left.Primary.EstimatedRetailMicrousd != nil || left.RankingBasis != "stable_candidate_identity" {
		t.Fatalf("planner fabricated an undeclared workload estimate: %+v", left)
	}
}

func TestCompileRequiresCurrentExactQualificationWithoutAnSLO(t *testing.T) {
	at := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	request := baseRequest(at)
	mismatched := candidate("mismatched", "supplier-a", 100_000, 400_000, at)
	mismatched.Qualification.TupleKey = "another-tuple"
	stale := candidate("stale-evidence", "supplier-b", 100_000, 400_000, at)
	stale.Qualification.ValidUntil = at

	plan, err := Compile(request, []Candidate{mismatched, stale})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"mismatched": ReasonQualificationMismatched, "stale-evidence": ReasonQualificationStale}
	for _, rejection := range plan.Rejections {
		if !contains(rejection.Reasons, want[rejection.CandidateID]) {
			t.Fatalf("qualification rejection lost for %s: %+v", rejection.CandidateID, rejection)
		}
	}
}

func baseRequest(at time.Time) Request {
	return Request{
		ModelID: "logical-model", Protocol: "openai", Capabilities: []string{"streaming", "tool-calling"}, Region: "eu",
		MinimumGrossMarginBPS: 2000, MaximumObservationAge: time.Hour, MaximumFallbacks: 2, At: at,
	}
}

func candidate(id, supplier string, retailInput, retailOutput int64, at time.Time) Candidate {
	return Candidate{
		ID: id, OfferID: "offer-" + id, OfferVersion: 1, RetailRateID: "retail-rate", RetailRateVersion: 1, Supplier: supplier, ModelID: "logical-model", SupplierModelID: supplier + "/model", TupleKey: "model@commit|runtime@digest|gpu|" + id,
		Protocol: "openai", Capabilities: []string{"streaming", "tool-calling", "structured-output"}, Regions: []string{"eu"},
		OfferState: OfferActive, Access: "ready", Availability: "available", Health: "healthy", ObservedAt: at.Add(-time.Minute), RateValidUntil: at.Add(time.Hour),
		CommercialState: CommercialReady, CommercialValidUntil: at.Add(time.Hour),
		RetailInputMicrousdPerMTok: int64Pointer(retailInput), RetailOutputMicrousdPerMTok: int64Pointer(retailOutput),
		CostInputMicrousdPerMTok: int64Pointer(retailInput * 3 / 4), CostOutputMicrousdPerMTok: int64Pointer(retailOutput * 3 / 4), CostBasisProvenance: "contract fixture",
		Qualification: &QualificationEvidence{ID: "qualification-" + id, State: QualificationQualified, TupleKey: "model@commit|runtime@digest|gpu|" + id, Protocol: "openai", Region: "eu", Capabilities: []string{"streaming", "tool-calling", "structured-output"}, Scope: "fixture", EvidenceRef: "fixture://" + id, EvidenceDigest: "sha256:" + id, ObservedAt: at.Add(-time.Minute), ValidUntil: at.Add(time.Hour)},
	}
}

func int64Pointer(value int64) *int64 { return &value }
