package controlapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/acceleratorlab"
	"github.com/infercrane/infercrane/internal/authz"
	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/kernelplanner"
	"github.com/infercrane/infercrane/internal/kernelresearch"
	"github.com/infercrane/infercrane/internal/optimizationcampaign"
)

type acceleratorLabRunQuery struct {
	tenant, kind, resourceType, resourceName string
	limit                                    int
}

// fakeAcceleratorLabRunStore filters like the PostgreSQL store and records
// every query. With leak set it returns every row, as a defective store would.
type fakeAcceleratorLabRunStore struct {
	*fakeStore
	mu      sync.Mutex
	rows    []domain.Operation
	queries []acceleratorLabRunQuery
	leak    bool
}

func (f *fakeAcceleratorLabRunStore) OperationsOfKind(_ context.Context, tenant, kind, resourceType, resourceName string, limit int) ([]domain.Operation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queries = append(f.queries, acceleratorLabRunQuery{tenant, kind, resourceType, resourceName, limit})
	out := make([]domain.Operation, 0, len(f.rows))
	for _, row := range f.rows {
		if f.leak || (row.TenantID == tenant && row.Kind == kind && row.ResourceType == resourceType && (resourceName == "" || row.ResourceName == resourceName)) {
			out = append(out, row)
		}
	}
	slices.SortFunc(out, func(left, right domain.Operation) int {
		if order := right.CreatedAt.Compare(left.CreatedAt); order != 0 {
			return order
		}
		return strings.Compare(right.ID, left.ID)
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (f *fakeAcceleratorLabRunStore) lastQuery(t *testing.T) acceleratorLabRunQuery {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.queries) == 0 {
		t.Fatal("store was not queried")
	}
	return f.queries[len(f.queries)-1]
}

func labDigest(fill string) string { return "sha256:" + strings.Repeat(fill, 64) }

func labRequestJSON(t *testing.T, tenant, campaign, candidate string) string {
	t.Helper()
	request := acceleratorlab.Request{
		SchemaVersion: acceleratorlab.RequestSchemaV1, TenantID: tenant, CampaignID: campaign, CandidateID: candidate,
		Model:    kernelplanner.ModelIdentity{Repository: "Qwen/Qwen3-0.6B", Revision: strings.Repeat("a", 40)},
		Runtime:  kernelplanner.RuntimeIdentity{Name: "vllm", Version: "0.22.1", ImageDigest: labDigest("1")},
		Hardware: kernelplanner.HardwareIdentity{Vendor: "nvidia", Accelerator: "H100"},
		Topology: acceleratorlab.Topology{Mode: "aggregated", Nodes: 1, Accelerators: 1, TensorParallel: 1, PipelineParallel: 1, ExpertParallel: 1},
		Workload: acceleratorlab.Workload{Digest: labDigest("2"), Modality: acceleratorlab.ModalityText, Phase: kernelplanner.PhaseDecode, BatchSize: 1, Concurrency: 4, InputTokens: 128, OutputTokens: 32, QualitySuite: "quality@" + labDigest("3"), Replay: "aiperf@" + labDigest("4")},
		Policy:   acceleratorlab.Policy{MaxCandidates: 2, MaxCostUSD: 5, MinHotspotFraction: .05, MinAmdahlCeiling: 1.05, TargetEndToEndSpeedup: 1.02, MinKernelSpeedup: 1.01, MaxMemoryRegressionPct: 2, RequireSanitizer: true, RequireHiddenShapes: true, RequireServingReplay: true, AllowGeneratedKernels: true, AuthorizedUntil: time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)},
		Sources:  []acceleratorlab.SourcePin{{ImplementationID: "infercrane-residual-rmsnorm-triton", Revision: strings.Repeat("b", 40), License: "Apache-2.0", Artifacts: []acceleratorlab.Artifact{{Kind: "source", URI: "s3://request-sources/kernel.tar.zst", SHA256: labDigest("5"), Size: 1024}}}},
	}
	encoded, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func labResultJSON(t *testing.T) string {
	t.Helper()
	measured := time.Date(2026, 11, 17, 10, 30, 0, 0, time.UTC)
	result := acceleratorlab.Result{
		SchemaVersion: acceleratorlab.ResultSchemaV1, InputDigest: labDigest("6"), State: "qualified", EvidenceState: "qualified", WinnerID: "candidate-rmsnorm", CostUSD: 1.25,
		Profile: acceleratorlab.ProfileEvidence{
			InputDigest: labDigest("6"), Tool: "torch-profiler", ToolVersion: "2.9.0", RuntimeImageDigest: labDigest("1"), WorkloadDigest: labDigest("2"),
			CapturedAt: measured.Add(-time.Hour), ArtifactDigest: labDigest("7"), CostUSD: .4,
			Hotspots: []kernelplanner.Hotspot{
				{ID: "hotspot-rmsnorm", Name: "fused_add_rms_norm", Family: kernelplanner.ResidualRMSNorm, DeviceTimeFraction: .18, MeanDurationUS: 12.5, Invocations: 64, Shapes: []string{"1x4096"}, DTypes: []string{"bf16"}},
				{ID: "hotspot-gemm", Name: "cutlass_gemm", Family: kernelplanner.QuantizedLinear, DeviceTimeFraction: .52},
			},
		},
		Plan: kernelplanner.Plan{
			AlgorithmVersion: "kernel-planner/v3", RegistryVersion: "registry/v2", SelectionBoundary: "profile-backed candidates only",
			Candidates: []kernelplanner.Candidate{{
				ID: "candidate-rmsnorm", Rank: 1, Status: "search", HotspotID: "hotspot-rmsnorm", OperatorFamily: kernelplanner.ResidualRMSNorm, DeviceTimeFraction: .18, MaxEndToEndSpeedup: 1.21,
				ExistingImplementations: []kernelplanner.KernelMatch{{ImplementationID: "infercrane-residual-rmsnorm-triton", Name: "InferCrane fused residual RMSNorm", Kind: "internal-experiment"}},
			}},
		},
		Experiments: []acceleratorlab.ExperimentResult{
			{
				Candidate: kernelplanner.Candidate{ID: "candidate-rmsnorm", OperatorFamily: kernelplanner.ResidualRMSNorm},
				Source:    acceleratorlab.SourcePin{ImplementationID: "generated-rmsnorm", Revision: strings.Repeat("c", 40), License: "Apache-2.0", Backend: "triton", Artifacts: []acceleratorlab.Artifact{{Kind: "source", URI: "s3://lab-artifacts/generated/source.tar.zst", SHA256: labDigest("8"), Size: 4096}}},
				Generation: &acceleratorlab.GenerationEvidence{
					InputDigest: labDigest("6"), CandidateID: "candidate-rmsnorm", Generator: "infercrane-kernel-adapter", GeneratorVersion: "0.4.0", GeneratedAt: measured.Add(-30 * time.Minute),
					Source:     acceleratorlab.SourcePin{ImplementationID: "generated-rmsnorm", Revision: strings.Repeat("c", 40), License: "Apache-2.0", Artifacts: []acceleratorlab.Artifact{{Kind: "source", URI: "s3://lab-artifacts/generated/source.tar.zst", SHA256: labDigest("8"), Size: 4096}}},
					Literature: []acceleratorlab.LiteratureReference{{Source: "arxiv", ID: "2501.01234", Title: "Fused normalization on modern GPUs", URL: "https://arxiv.org/abs/2501.01234"}},
					Research: &kernelresearch.Receipt{
						State: "converged", BestSourceRevision: strings.Repeat("c", 40), BestKernelSpeedup: 1.4, BestEndToEndSpeedup: 1.03, ReceiptDigest: labDigest("9"),
						Observations: []kernelresearch.Observation{{
							Attempt:       kernelresearch.Attempt{Iteration: 1, SourceRevision: strings.Repeat("c", 40), Backend: "triton", ArtifactDigest: labDigest("a"), Correctness: kernelresearch.Correctness{Smoke: true, ShapeSweep: true, NumericalStability: true, Determinism: true, EdgeCases: true, Sanitizer: true}, BaselineMedianUS: 10, CandidateMedianUS: 7.1, CompletedAt: measured.Add(-20 * time.Minute)},
							KernelSpeedup: 1.4, EndToEndSpeedup: 1.03, Decision: "keep", Reason: "correct and materially faster",
						}},
					},
				},
				Build: &acceleratorlab.BuildEvidence{
					InputDigest: labDigest("6"), Builder: "brezel", BuilderVersion: "1.2.0", Environment: "env-2026-11", SourceDigest: labDigest("8"), Status: "passed", Checks: []string{"compile", "import"},
					Artifacts: []acceleratorlab.Artifact{{Kind: "wheel", URI: "https://artifacts.example.internal/builds/kernel.whl", SHA256: labDigest("b"), Size: 8192}}, ReceiptDigest: labDigest("c"), CostUSD: .2,
				},
				Qualification: &acceleratorlab.QualificationEvidence{
					InputDigest: labDigest("6"), Hardware: kernelplanner.HardwareIdentity{Vendor: "nvidia", Accelerator: "H100", ComputeCapability: "sm90"}, RuntimeImageDigest: labDigest("1"), WorkloadDigest: labDigest("2"),
					Modality: acceleratorlab.ModalityText, Replay: "aiperf@" + labDigest("4"), QualitySuite: "quality@" + labDigest("3"), BuildArtifactDigest: labDigest("b"),
					CorrectnessPassed: true, HiddenShapesPassed: true, SanitizerPassed: true, ServingReplayMeasured: true,
					KernelSpeedup: 1.4, EndToEndSpeedup: 1.03, MemoryRegressionPct: .5, ProfilerArtifactDigest: labDigest("d"), BenchmarkEvidenceID: "benchmark-1", CostUSD: .65, MeasuredAt: measured,
				},
				State: "qualified",
			},
			{
				Candidate:   kernelplanner.Candidate{ID: "candidate-gemm", OperatorFamily: kernelplanner.QuantizedLinear},
				State:       "inconclusive",
				FailureCode: "kernel_generation_failed",
				Reasons:     []string{`call accelerator worker: Post "s3://lab-artifacts/private/generate": EOF`},
			},
		},
		Limitations: []string{"A qualified experiment is still a release candidate."},
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func acceleratorLabRunFixture(t *testing.T) []domain.Operation {
	t.Helper()
	base := time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC)
	completed := base.Add(3 * time.Hour)
	lab := func(id, tenant, campaign string, created time.Time) domain.Operation {
		return domain.Operation{
			ID: id, TenantID: tenant, Kind: acceleratorlab.ExecuteKind, ResourceType: acceleratorLabResourceType, ResourceName: campaign,
			IdempotencyKey: "accelerator-lab:" + labDigest("e"), RequestJSON: labRequestJSON(t, tenant, campaign, "candidate-"+id),
			ResultJSON: "{}", Status: "pending", LeaseOwner: "worker-7", LeaseGeneration: 3, MaxAttempts: 20, CreatedAt: created, UpdatedAt: created,
		}
	}
	recorded := lab("op-recorded", "tenant-a", "campaign-1", base.Add(2*time.Hour))
	recorded.Status, recorded.Progress, recorded.Message, recorded.ResultJSON, recorded.CompletedAt = "succeeded", 100, "A measured candidate is ready for Release Guard review.", labResultJSON(t), &completed
	pending := lab("op-pending", "tenant-a", "campaign-2", base.Add(time.Hour))
	pending.Status, pending.Progress, pending.Message = "running", 25, "Building candidate 1 of 2 in Brezel."
	unreadable := lab("op-unreadable", "tenant-a", "campaign-1", base)
	unreadable.Status, unreadable.ErrorCode, unreadable.Message = "failed", "accelerator_lab_execution_failed", `call accelerator worker: Post "https://worker.internal.example/v1/qualify": EOF`
	unreadable.RequestJSON, unreadable.ResultJSON = `{"schema_version":"infercrane.accelerator-lab-request/v0"}`, `{"schema_version":"infercrane.accelerator-lab-result/v0","state":"qualified"}`
	foreign := lab("op-foreign", "tenant-b", "campaign-1", base.Add(4*time.Hour))
	campaign := domain.Operation{ID: "op-campaign", TenantID: "tenant-a", Kind: optimizationcampaign.ExecuteKind, ResourceType: acceleratorLabResourceType, ResourceName: "campaign-1", Status: "running", ResultJSON: "{}", CreatedAt: base.Add(5 * time.Hour), UpdatedAt: base.Add(5 * time.Hour)}
	return []domain.Operation{unreadable, foreign, recorded, campaign, pending}
}

func newAcceleratorLabRunAPI(t *testing.T) (*fakeAcceleratorLabRunStore, func(string) *httptest.ResponseRecorder) {
	t.Helper()
	store := &fakeAcceleratorLabRunStore{
		fakeStore: &fakeStore{principal: domain.Principal{ID: "viewer-a", TenantID: "tenant-a", Role: string(authz.Viewer), Kind: "api_key", Scopes: authz.DefaultScopeNames(authz.Viewer)}},
		rows:      acceleratorLabRunFixture(t),
	}
	handler := (API{Store: store, Authenticator: store}).Handler()
	return store, func(target string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodGet, target, nil)
		request.Header.Set("Authorization", "Bearer viewer-token")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}
}

type acceleratorLabRunEnvelope struct {
	Data     []acceleratorLabRun `json:"data"`
	Boundary string              `json:"boundary"`
}

func decodeAcceleratorLabRuns(t *testing.T, response *httptest.ResponseRecorder) acceleratorLabRunEnvelope {
	t.Helper()
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var envelope acceleratorLabRunEnvelope
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	return envelope
}

func runIDs(runs []acceleratorLabRun) []string {
	ids := make([]string, 0, len(runs))
	for _, run := range runs {
		ids = append(ids, run.ID)
	}
	return ids
}

func TestAcceleratorLabRunsAreTenantScopedTypedAndDocumented(t *testing.T) {
	store, get := newAcceleratorLabRunAPI(t)
	response := get("/api/v1/optimization/accelerator-lab/runs")
	envelope := decodeAcceleratorLabRuns(t, response)
	if got := runIDs(envelope.Data); !slices.Equal(got, []string{"op-recorded", "op-pending", "op-unreadable"}) {
		t.Fatalf("runs=%v, want only tenant-a lab runs newest first", got)
	}
	if envelope.Boundary != acceleratorLabRunBoundary {
		t.Fatalf("boundary=%q", envelope.Boundary)
	}
	if query := store.lastQuery(t); query != (acceleratorLabRunQuery{"tenant-a", acceleratorlab.ExecuteKind, acceleratorLabResourceType, "", defaultAcceleratorLabRunLimit}) {
		t.Fatalf("query=%+v", query)
	}

	recorded, pending, unreadable := envelope.Data[0], envelope.Data[1], envelope.Data[2]
	if recorded.ResultState != acceleratorLabResultRecorded || recorded.Result == nil || pending.ResultState != acceleratorLabResultPending || pending.Result != nil || unreadable.ResultState != acceleratorLabResultUnreadable || unreadable.Result != nil {
		t.Fatalf("result states = %q %q %q", recorded.ResultState, pending.ResultState, unreadable.ResultState)
	}
	if recorded.CampaignID != "campaign-1" || recorded.CandidateID != "candidate-op-recorded" || recorded.Status != "succeeded" || recorded.Progress != 100 || recorded.ErrorCode != nil || recorded.CompletedAt == nil {
		t.Fatalf("recorded run = %+v", recorded)
	}
	if target := recorded.Target; target == nil || target.Model.Repository != "Qwen/Qwen3-0.6B" || target.Runtime.ImageDigest != labDigest("1") || target.Hardware.Accelerator != "H100" || target.Topology.TensorParallel != 1 || target.Workload.Concurrency != 4 || target.Workload.OutputTokens != 32 {
		t.Fatalf("target = %+v", recorded.Target)
	}
	if policy := recorded.Policy; policy == nil || policy.MaxCostUSD != 5 || policy.MinAmdahlCeiling != 1.05 || !policy.RequireSanitizer || policy.RequireQuality || !policy.AllowGeneratedKernels {
		t.Fatalf("policy = %+v", recorded.Policy)
	}
	result := recorded.Result
	if result.WinnerID == nil || *result.WinnerID != "candidate-rmsnorm" || result.CostUSD != 1.25 || result.Profile.ArtifactDigest != labDigest("7") || len(result.Plan.Candidates) != 1 || len(result.Experiments) != 2 {
		t.Fatalf("result = %+v", result)
	}
	measuredHotspot, sparseHotspot := result.Profile.Hotspots[0], result.Profile.Hotspots[1]
	if measuredHotspot.MeanDurationUS == nil || *measuredHotspot.MeanDurationUS != 12.5 || measuredHotspot.Invocations == nil || *measuredHotspot.Invocations != 64 {
		t.Fatalf("measured hotspot = %+v", measuredHotspot)
	}
	if sparseHotspot.MeanDurationUS != nil || sparseHotspot.Invocations != nil {
		t.Fatalf("unrecorded hotspot measurements became values: %+v", sparseHotspot)
	}
	qualified, failed := result.Experiments[0], result.Experiments[1]
	if !qualified.Generated || qualified.FailureCode != nil || qualified.Source.Backend != "triton" || qualified.Build == nil || !slices.Equal(qualified.Build.ArtifactDigests, []string{labDigest("b")}) {
		t.Fatalf("qualified experiment = %+v", qualified)
	}
	if evidence := qualified.Qualification; evidence == nil || evidence.KernelSpeedup != 1.4 || evidence.EndToEndSpeedup != 1.03 || evidence.CostRegressionPct != 0 || evidence.Hardware.ComputeCapability != "sm90" || evidence.BuildArtifactDigest != labDigest("b") {
		t.Fatalf("qualification = %+v", qualified.Qualification)
	}
	if research := qualified.Research; research == nil || research.State != "converged" || len(research.Attempts) != 1 || research.Attempts[0].CandidateMedianUS != 7.1 || !research.Attempts[0].Correctness.Passed() {
		t.Fatalf("research = %+v", qualified.Research)
	}
	if failed.Generated || failed.FailureCode == nil || *failed.FailureCode != "kernel_generation_failed" || failed.Build != nil || failed.Qualification != nil || failed.Research != nil {
		t.Fatalf("failed experiment = %+v", failed)
	}
	if pending.Target == nil || pending.Policy == nil || pending.CandidateID != "candidate-op-pending" || pending.CompletedAt != nil {
		t.Fatalf("pending run = %+v", pending)
	}
	if unreadable.Target != nil || unreadable.Policy != nil || unreadable.CandidateID != "" || unreadable.ErrorCode == nil || *unreadable.ErrorCode != "accelerator_lab_execution_failed" {
		t.Fatalf("unreadable run = %+v", unreadable)
	}

	body := response.Body.String()
	for _, hidden := range []string{
		"s3://", "artifacts.example.internal", "worker.internal.example", `"uri"`, `"size"`, // artifact and worker locations
		"arxiv", "Fused normalization on modern GPUs", `"literature"`, // literature
		"tenant-a", "tenant-b", `"tenant_id"`, "worker-7", `"lease_owner"`, "accelerator-lab:sha256", `"idempotency_key"`,
		"quality@", "aiperf@", `"authorized_until"`, `"environment_revision"`,
	} {
		if strings.Contains(body, hidden) {
			t.Errorf("response exposes %q: %s", hidden, body)
		}
	}
	for _, empty := range []string{`"rejected":[]`, `"dtypes":[]`, `"shapes":[]`, `"reasons":[]`, `"compiler_choices":[]`} {
		if !strings.Contains(body, empty) {
			t.Errorf("response is missing empty list %s", empty)
		}
	}
	for _, list := range []string{"data", "hotspots", "shapes", "dtypes", "candidates", "rejected", "experiments", "reasons", "checks", "artifact_digests", "attempts", "limitations", "existing_implementations"} {
		if strings.Contains(body, `"`+list+`":null`) {
			t.Errorf("response has a null %s list", list)
		}
	}
	if failed.Reasons[0] != `call accelerator worker: Post "[uri redacted]": EOF` || unreadable.Message != `call accelerator worker: Post "[uri redacted]": EOF` {
		t.Fatalf("URIs in free text were not redacted: %q / %q", failed.Reasons, unreadable.Message)
	}

	var raw struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	var top map[string]any
	_ = json.Unmarshal(response.Body.Bytes(), &top)
	assertMatchesDocumentedSchema(t, "AcceleratorLabRunList", top)
	for _, run := range raw.Data {
		assertMatchesDocumentedSchema(t, "AcceleratorLabRun", run)
	}
	documented := raw.Data[0]
	assertMatchesDocumentedSchema(t, "AcceleratorLabRunTarget", documented["target"].(map[string]any))
	assertMatchesDocumentedSchema(t, "AcceleratorLabRunPolicy", documented["policy"].(map[string]any))
	resultObject := documented["result"].(map[string]any)
	assertMatchesDocumentedSchema(t, "AcceleratorLabRunResult", resultObject)
	for _, item := range resultObject["experiments"].([]any) {
		experiment := item.(map[string]any)
		assertMatchesDocumentedSchema(t, "AcceleratorLabRunExperiment", experiment)
		if qualification, ok := experiment["qualification"].(map[string]any); ok {
			assertMatchesDocumentedSchema(t, "AcceleratorLabRunQualification", qualification)
		}
		if research, ok := experiment["research"].(map[string]any); ok {
			assertMatchesDocumentedSchema(t, "AcceleratorLabRunResearch", research)
		}
	}

	// Another tenant sees only its own run, and the store is never asked for
	// a tenant other than the caller's.
	store.principal.TenantID = "tenant-b"
	if got := runIDs(decodeAcceleratorLabRuns(t, get("/api/v1/optimization/accelerator-lab/runs")).Data); !slices.Equal(got, []string{"op-foreign"}) {
		t.Fatalf("tenant-b runs=%v", got)
	}
	for _, query := range store.queries {
		if query.tenant != "tenant-a" && query.tenant != "tenant-b" || query.kind != acceleratorlab.ExecuteKind || query.resourceType != acceleratorLabResourceType {
			t.Fatalf("unexpected store query %+v", query)
		}
	}
	store.principal.ID = ""
	if response = get("/api/v1/optimization/accelerator-lab/runs"); response.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status=%d", response.Code)
	}
}

func TestAcceleratorLabRunsFilterByCampaignAndBoundTheLimit(t *testing.T) {
	store, get := newAcceleratorLabRunAPI(t)
	if got := runIDs(decodeAcceleratorLabRuns(t, get("/api/v1/optimization/accelerator-lab/runs?campaign_id=campaign-1")).Data); !slices.Equal(got, []string{"op-recorded", "op-unreadable"}) {
		t.Fatalf("campaign-1 runs=%v", got)
	}
	if query := store.lastQuery(t); query.resourceName != "campaign-1" {
		t.Fatalf("campaign filter was not passed to the store: %+v", query)
	}
	if got := runIDs(decodeAcceleratorLabRuns(t, get("/api/v1/optimization/accelerator-lab/runs?campaign_id=missing")).Data); len(got) != 0 {
		t.Fatalf("unknown campaign runs=%v", got)
	}
	if response := get("/api/v1/optimization/accelerator-lab/runs?campaign_id=missing"); !strings.Contains(response.Body.String(), `"data":[]`) {
		t.Fatalf("empty list is not []: %s", response.Body.String())
	}
	if got := runIDs(decodeAcceleratorLabRuns(t, get("/api/v1/optimization/accelerator-lab/runs?limit=1")).Data); !slices.Equal(got, []string{"op-recorded"}) {
		t.Fatalf("limit=1 runs=%v", got)
	}
	if query := store.lastQuery(t); query.limit != 1 {
		t.Fatalf("limit was not passed to the store: %+v", query)
	}
	decodeAcceleratorLabRuns(t, get("/api/v1/optimization/accelerator-lab/runs?limit=100"))
	if query := store.lastQuery(t); query.limit != maxAcceleratorLabRunLimit {
		t.Fatalf("limit=100 query=%+v", query)
	}
	before := len(store.queries)
	for _, target := range []string{"?limit=0", "?limit=101", "?limit=-1", "?limit=abc", "?campaign_id=" + strings.Repeat("c", maxAcceleratorLabCampaignID+1)} {
		response := get("/api/v1/optimization/accelerator-lab/runs" + target)
		if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), `"code":"invalid_query"`) {
			t.Fatalf("%s status=%d body=%s", target, response.Code, response.Body.String())
		}
	}
	if len(store.queries) != before {
		t.Fatal("an invalid query reached the store")
	}
}

func TestAcceleratorLabRunsDropRowsTheStoreShouldNotHaveReturned(t *testing.T) {
	store, get := newAcceleratorLabRunAPI(t)
	store.leak = true
	if got := runIDs(decodeAcceleratorLabRuns(t, get("/api/v1/optimization/accelerator-lab/runs")).Data); !slices.Equal(got, []string{"op-recorded", "op-pending", "op-unreadable"}) {
		t.Fatalf("runs=%v, want foreign tenant and other kinds dropped", got)
	}
	if got := runIDs(decodeAcceleratorLabRuns(t, get("/api/v1/optimization/accelerator-lab/runs?campaign_id=campaign-2")).Data); !slices.Equal(got, []string{"op-pending"}) {
		t.Fatalf("campaign-2 runs=%v", got)
	}
}

func TestAcceleratorLabRunsNeedACapableStore(t *testing.T) {
	handler := (API{Store: &fakeStore{}, APIKey: "secret"}).Handler()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/optimization/accelerator-lab/runs", nil)
	request.Header.Set("Authorization", "Bearer secret")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNotImplemented || !strings.Contains(response.Body.String(), `"code":"capability_unavailable"`) {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestProjectAcceleratorLabResultStates(t *testing.T) {
	for name, test := range map[string]struct {
		raw, state string
	}{
		"column default":        {`{}`, acceleratorLabResultPending},
		"empty":                 {``, acceleratorLabResultPending},
		"json null":             {`null`, acceleratorLabResultPending},
		"not json":              {`{"schema_version":`, acceleratorLabResultUnreadable},
		"another schema":        {`{"schema_version":"infercrane.accelerator-lab-result/v2"}`, acceleratorLabResultUnreadable},
		"wrong field type":      {`{"schema_version":"infercrane.accelerator-lab-result/v1","cost_usd":"free"}`, acceleratorLabResultUnreadable},
		"wrong hotspot type":    {`{"schema_version":"infercrane.accelerator-lab-result/v1","profile":{"hotspots":[{"invocations":"many"}]}}`, acceleratorLabResultUnreadable},
		"minimal current shape": {`{"schema_version":"infercrane.accelerator-lab-result/v1","state":"profiling"}`, acceleratorLabResultRecorded},
	} {
		t.Run(name, func(t *testing.T) {
			state, result := projectAcceleratorLabResult(test.raw)
			if state != test.state || (result != nil) != (state == acceleratorLabResultRecorded) {
				t.Fatalf("state=%q result=%+v, want %q", state, result, test.state)
			}
		})
	}
	_, minimal := projectAcceleratorLabResult(`{"schema_version":"infercrane.accelerator-lab-result/v1","state":"profiling","profile":{"hotspots":[{"id":"h","mean_duration_us":0}]}}`)
	encoded, _ := json.Marshal(minimal)
	for _, want := range []string{`"winner_id":null`, `"experiments":[]`, `"limitations":[]`, `"candidates":[]`, `"rejected":[]`, `"mean_duration_us":0`, `"invocations":null`} {
		if !strings.Contains(string(encoded), want) {
			t.Errorf("minimal result %s is missing %s", encoded, want)
		}
	}
}

func TestProjectAcceleratorLabRunWithUnreadableRequest(t *testing.T) {
	for _, raw := range []string{`not json`, `{}`, `{"schema_version":"infercrane.accelerator-lab-request/v2","candidate_id":"c"}`} {
		run := projectAcceleratorLabRun(domain.Operation{ID: "op", ResourceName: "campaign", RequestJSON: raw, ResultJSON: "{}", Status: "pending"})
		if run.CandidateID != "" || run.Target != nil || run.Policy != nil || run.CampaignID != "campaign" || run.ResultState != acceleratorLabResultPending {
			t.Fatalf("request %s projected to %+v", raw, run)
		}
		encoded, _ := json.Marshal(run)
		for _, want := range []string{`"candidate_id":""`, `"target":null`, `"policy":null`, `"error_code":null`, `"completed_at":null`, `"result":null`} {
			if !strings.Contains(string(encoded), want) {
				t.Errorf("run %s is missing %s", encoded, want)
			}
		}
	}
}
