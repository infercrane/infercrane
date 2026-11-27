package controlapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/apibaseline"
	"github.com/infercrane/infercrane/internal/authz"
	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/optimizer"
	"github.com/infercrane/infercrane/internal/passport"
	"github.com/infercrane/infercrane/internal/qualitycheck"
	"github.com/infercrane/infercrane/internal/qualityevidence"
	"github.com/infercrane/infercrane/internal/store"
)

// fakeMigrationStore holds one endpoint, support-bot, whose binding
// "primary" routes a closed API.
type fakeMigrationStore struct {
	*fakeStore
	ownership    string
	captured     []string
	card         *apibaseline.RateCard
	evidence     []domain.BindingQualityEvidence
	trace        domain.ReplayTrace
	evalSet      string
	stage        store.MigrationStage
	stageErr     error
	stagedWith   []string
	recordedRows []domain.InferenceRecord
}

func (f *fakeMigrationStore) ResolveEndpointForTenant(_ context.Context, tenant, name string) (domain.ResolvedEndpoint, error) {
	if tenant != "tenant-a" || name != "support-bot" {
		return domain.ResolvedEndpoint{}, domain.ErrNotFound
	}
	return domain.ResolvedEndpoint{Endpoint: domain.Endpoint{ID: "endpoint-1", Name: name, LogicalModelID: "model-1", EnvironmentID: "env-1"}, Bindings: []domain.BackendBinding{{ID: "binding-1", Name: "primary", Kind: "external", TargetID: "target-1", OwnershipMode: f.ownership}}}, nil
}

func (f *fakeMigrationStore) ExternalBindingTarget(ctx context.Context, tenant, endpoint, binding string) (domain.ResolvedEndpoint, domain.BackendBinding, domain.Target, error) {
	resolved, err := f.ResolveEndpointForTenant(ctx, tenant, endpoint)
	if err != nil || (binding != "primary" && binding != "binding-1") {
		return resolved, domain.BackendBinding{}, domain.Target{}, domain.ErrNotFound
	}
	return resolved, resolved.Bindings[0], domain.Target{ID: "target-1", UpstreamModel: "gpt-4.1-mini"}, nil
}

func (f *fakeMigrationStore) CaptureEndpointReplayTrace(_ context.Context, tenant, endpoint, binding string, window time.Duration, limit int) (domain.ReplayTrace, error) {
	f.captured = append(f.captured, fmt.Sprintf("%s/%s/%s/%s/%d", tenant, endpoint, binding, window, limit))
	if endpoint != "support-bot" {
		return domain.ReplayTrace{}, domain.ErrNotFound
	}
	return domain.ReplayTrace{ID: "trace-1", EndpointID: "endpoint-1", EndpointName: endpoint, BindingID: "binding-1", ShapeJSON: "[]", SummaryJSON: "{}", ShapeDigest: strings.Repeat("a", 64)}, nil
}

func (f *fakeMigrationStore) EndpointReplayTraces(context.Context, string, string, int) ([]domain.ReplayTrace, error) {
	return []domain.ReplayTrace{f.trace}, nil
}

func (f *fakeMigrationStore) ReplayTraceByDigest(_ context.Context, _, digest string) (domain.ReplayTrace, error) {
	if digest != f.trace.ShapeDigest {
		return domain.ReplayTrace{}, domain.ErrNotFound
	}
	return f.trace, nil
}

func (f *fakeMigrationStore) RecordBindingRateCard(_ context.Context, _, _, _ string, card apibaseline.RateCard) (apibaseline.RateCard, error) {
	if err := card.Validate(time.Now()); err != nil {
		return card, err
	}
	card.ID, card.BindingID = "card-1", "binding-1"
	f.card = &card
	return card, nil
}

func (f *fakeMigrationStore) BindingRateCard(context.Context, string, string) (*apibaseline.RateCard, error) {
	return f.card, nil
}

func (f *fakeMigrationStore) RecordBindingQualityEvidence(_ context.Context, _, _, _ string, row domain.BindingQualityEvidence) (domain.BindingQualityEvidence, bool, error) {
	row.ID = "closed-evidence-1"
	f.evidence = append(f.evidence, row)
	return row, true, nil
}

func (f *fakeMigrationStore) BindingQualityEvidence(context.Context, string, string, int) ([]domain.BindingQualityEvidence, error) {
	return f.evidence, nil
}

func (f *fakeMigrationStore) MigrationVerdictsForCampaign(context.Context, string, string) ([]domain.MigrationQualityVerdict, error) {
	return []domain.MigrationQualityVerdict{{ID: "verdict-1", CandidateRunID: "fast", Verdict: "PASS", PayloadJSON: `{"candidate_score":0.95}`}}, nil
}

func (f *fakeMigrationStore) StageMigration(_ context.Context, tenant, endpoint, step, verdict string, canary int, window time.Duration) (store.MigrationStage, error) {
	f.stagedWith = append(f.stagedWith, fmt.Sprintf("%s/%s/%s/%s/%d/%s", tenant, endpoint, step, verdict, canary, window))
	return f.stage, f.stageErr
}

func (f *fakeMigrationStore) QualityEvalSet(_ context.Context, _, digest string) (domain.QualityEvalSet, error) {
	if f.evalSet == "" {
		return domain.QualityEvalSet{}, domain.ErrNotFound
	}
	return domain.QualityEvalSet{Digest: digest, Body: f.evalSet}, nil
}

func (f *fakeMigrationStore) RecordRequest(_ context.Context, record domain.InferenceRecord) error {
	f.recordedRows = append(f.recordedRows, record)
	return nil
}

func migrationAPI(t *testing.T) (*fakeMigrationStore, http.Handler) {
	t.Helper()
	proofStore := &fakeMigrationStore{fakeStore: &fakeStore{principal: domain.Principal{ID: "operator", TenantID: "tenant-a", Name: "operator", Role: string(authz.Operator), Scopes: authz.DefaultScopeNames(authz.Operator)}}, ownership: "traffic-managed"}
	proofStore.trace = domain.ReplayTrace{ID: "trace-1", EndpointID: "endpoint-1", EndpointName: "support-bot", BindingID: "binding-1", ShapeDigest: strings.Repeat("b", 64), ShapeJSON: `[{"operation":"chat","arrival_ms":1,"duration_ms":10,"input_tokens":5,"output_tokens":3}]`}
	return proofStore, (API{Store: proofStore, Authenticator: proofStore}).Handler()
}

func TestMigrationRoutesCaptureAndPriceTheClosedAPI(t *testing.T) {
	proofStore, handler := migrationAPI(t)
	response := serve(handler, http.MethodPost, "/api/v1/endpoints/support-bot/replays", "token", `{"binding":"primary","window_seconds":7200}`)
	if response.Code != http.StatusCreated || !strings.Contains(response.Body.String(), `"endpoint_name":"support-bot"`) || proofStore.captured[0] != "tenant-a/support-bot/primary/2h0m0s/1000" {
		t.Fatalf("capture status=%d body=%s captured=%v", response.Code, response.Body.String(), proofStore.captured)
	}
	if response := serve(handler, http.MethodGet, "/api/v1/endpoints/support-bot/bindings/primary/rate-card", "token", ""); response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"rate_card":null`) {
		t.Fatalf("missing rate card status=%d body=%s", response.Code, response.Body.String())
	}
	if response := serve(handler, http.MethodPut, "/api/v1/endpoints/support-bot/bindings/primary/rate-card", "token", `{"authority":"guess","currency":"USD","input_microusd_per_million":1,"output_microusd_per_million":1,"valid_until":"2099-01-01T00:00:00Z"}`); response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("rate card without authority status=%d", response.Code)
	}
	valid := time.Now().Add(30 * 24 * time.Hour).UTC().Format(time.RFC3339)
	if response := serve(handler, http.MethodPut, "/api/v1/endpoints/support-bot/bindings/primary/rate-card", "token", `{"authority":"contract","currency":"usd","input_microusd_per_million":320000,"cached_input_microusd_per_million":80000,"output_microusd_per_million":1280000,"valid_until":"`+valid+`"}`); response.Code != http.StatusCreated || proofStore.card == nil || proofStore.card.RecordedBy != "operator" || *proofStore.card.Pricing.CachedInputMicrousdPerMillion != 80000 {
		t.Fatalf("rate card status=%d body=%s card=%+v", response.Code, response.Body.String(), proofStore.card)
	}
	import_ := `{"source":"openai","schema_version":"infercrane.traffic-observations/v1","binding":"primary","observations":[{"source_request_id":"r1","started_at":"` + time.Now().UTC().Format(time.RFC3339) + `","latency_ms":200,"input_tokens":100,"cached_input_tokens":40,"output_tokens":20,"status_code":200,"operation":"chat","streaming":false}]}`
	if response := serve(handler, http.MethodPost, "/api/v1/endpoints/support-bot/traffic-observations", "token", import_); response.Code != http.StatusCreated || len(proofStore.recordedRows) != 1 || proofStore.recordedRows[0].BindingID != "binding-1" || proofStore.recordedRows[0].DeploymentID != "" || *proofStore.recordedRows[0].CachedInputTokens != 40 {
		t.Fatalf("import status=%d body=%s rows=%+v", response.Code, response.Body.String(), proofStore.recordedRows)
	}
	tooMany := strings.Replace(import_, `"cached_input_tokens":40`, `"cached_input_tokens":400`, 1)
	if response := serve(handler, http.MethodPost, "/api/v1/endpoints/support-bot/traffic-observations", "token", tooMany); response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("cached tokens above input accepted: %d", response.Code)
	}
}

func TestMigrationStepNeedsAVerdictAndReportsGuardRefusal(t *testing.T) {
	proofStore, handler := migrationAPI(t)
	if response := serve(handler, http.MethodPost, "/api/v1/endpoints/support-bot/migration", "token", `{"step":"canary"}`); response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("canary without a verdict status=%d", response.Code)
	}
	proofStore.stage = store.MigrationStage{Step: "canary", Activated: true, Plan: domain.ServingPlan{ID: "plan-2", RoutingPolicy: "weighted", MigrationVerdictID: "verdict-1"}, Evaluation: domain.EndpointReleaseGuardEvaluation{Decision: "PASS", ReasonCodesJSON: `["migration_canary_admitted_on_quality_verdict"]`}}
	response := serve(handler, http.MethodPost, "/api/v1/endpoints/support-bot/migration", "token", `{"step":"canary","verdict_id":"verdict-1","canary_basis_points":250}`)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"migration_verdict_id":"verdict-1"`) || proofStore.stagedWith[0] != "tenant-a/support-bot/canary/verdict-1/250/1h0m0s" {
		t.Fatalf("canary status=%d body=%s staged=%v", response.Code, response.Body.String(), proofStore.stagedWith)
	}
	proofStore.stage = store.MigrationStage{Step: "primary", Evaluation: domain.EndpointReleaseGuardEvaluation{Decision: "REJECT", ReasonCodesJSON: `["ttft_regression"]`}}
	if response := serve(handler, http.MethodPost, "/api/v1/endpoints/support-bot/migration", "token", `{"step":"primary"}`); response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), `"activated":false`) || !strings.Contains(response.Body.String(), "ttft_regression") {
		t.Fatalf("refused primary status=%d body=%s", response.Code, response.Body.String())
	}
	proofStore.stageErr = fmt.Errorf("%w: the migration verdict is BLOCK, not PASS", domain.ErrConflict)
	if response := serve(handler, http.MethodPost, "/api/v1/endpoints/support-bot/migration", "token", `{"step":"canary","verdict_id":"blocked"}`); response.Code != http.StatusConflict {
		t.Fatalf("blocked verdict status=%d", response.Code)
	}
}

func TestClosedAPIEvidenceMustNameTheBindingSubject(t *testing.T) {
	proofStore, handler := migrationAPI(t)
	_, key, _ := passport.GenerateKey()
	envelope := func(evaluator, revision string) string {
		payload := qualityevidence.Payload{Schema: qualityevidence.Schema, Deployment: "support-bot", RevisionID: revision, Suite: "support", SuiteVersion: "3", Evaluator: evaluator, EvaluatorVersion: "1", Score: 0.9, Passed: true, SampleCount: 1, ArtifactDigest: "sha256:" + strings.Repeat("c", 64), EvaluatedAt: time.Now().UTC().Add(-time.Minute).Truncate(time.Second), Distribution: &qualityevidence.Distribution{Schema: qualityevidence.DistributionSchema, Kind: "paired_scores", PairingDigest: "sha256:" + strings.Repeat("d", 64), Scores: []float64{0.9}}}
		signed, err := passport.Sign(payload, key)
		if err != nil {
			t.Fatal(err)
		}
		encoded, _ := json.Marshal(signed)
		return string(encoded)
	}
	const path = "/api/v1/endpoints/support-bot/bindings/primary/quality-evidence"
	for _, evaluator := range []string{qualitycheck.Evaluator, qualitycheck.GradedEvaluator} {
		if response := serve(handler, http.MethodPost, path, "token", envelope(evaluator, "binding:binding-1")); response.Code != http.StatusUnprocessableEntity {
			t.Fatalf("reserved evaluator %s status=%d", evaluator, response.Code)
		}
	}
	if response := serve(handler, http.MethodPost, path, "token", envelope("acme-eval", "rev-1")); response.Code != http.StatusUnprocessableEntity || !strings.Contains(response.Body.String(), "binding:binding-1") {
		t.Fatalf("revision subject accepted: %d %s", response.Code, response.Body.String())
	}
	response := serve(handler, http.MethodPost, path, "token", envelope("acme-eval", "binding:binding-1"))
	if response.Code != http.StatusCreated || len(proofStore.evidence) != 1 || proofStore.evidence[0].Source != domain.QualityEvidenceSourceCustomer || proofStore.evidence[0].PairingDigest == "" {
		t.Fatalf("closed API evidence status=%d body=%s rows=%+v", response.Code, response.Body.String(), proofStore.evidence)
	}
}

func TestMigrationCampaignChecksItsBaselineBeforeItIsStored(t *testing.T) {
	graded := func(prompts int) string {
		rows := make([]string, prompts)
		for i := range rows {
			rows[i] = fmt.Sprintf(`{"id":"p%d","prompt":"Say %d","expected":["%d"]}`, i, i, i)
		}
		return `{"schema":"infercrane.dev/quality-eval-set/v1","name":"s","version":"1","grading":"exact_match","prompts":[` + strings.Join(rows, ",") + `]}`
	}
	input := optimizer.Request{WorkloadSource: optimizer.WorkloadSourceCustomerObserved, WorkloadFingerprint: "sha256:" + strings.Repeat("b", 64), QualityEvalSet: "sha256:" + strings.Repeat("e", 64), Migration: &optimizer.MigrationBaseline{Endpoint: "support-bot", Binding: "primary", Dialect: optimizer.MigrationDialectOpenAI, MaxQualityRegressionPercent: 5}}
	for name, test := range map[string]struct {
		edit func(*fakeMigrationStore, *optimizer.Request)
		code string
	}{
		"valid":           {func(*fakeMigrationStore, *optimizer.Request) {}, ""},
		"observe-only":    {func(s *fakeMigrationStore, _ *optimizer.Request) { s.ownership = "observe-only" }, "migration_baseline_observe_only"},
		"unknown binding": {func(_ *fakeMigrationStore, r *optimizer.Request) { r.Migration.Binding = "other" }, "migration_baseline_not_found"},
		"other endpoint":  {func(s *fakeMigrationStore, _ *optimizer.Request) { s.trace.EndpointID = "endpoint-2" }, "migration_workload_not_captured"},
		"deployment trace": {func(s *fakeMigrationStore, _ *optimizer.Request) {
			s.trace.EndpointID, s.trace.DeploymentID = "", "dep-1"
		}, "migration_workload_not_captured"},
		"empty trace": {func(s *fakeMigrationStore, _ *optimizer.Request) { s.trace.ShapeJSON = "[]" }, "migration_workload_empty"},
		"no eval set": {func(s *fakeMigrationStore, _ *optimizer.Request) { s.evalSet = "" }, "quality_eval_set_not_found"},
		"ungraded eval set": {func(s *fakeMigrationStore, _ *optimizer.Request) {
			s.evalSet = `{"schema":"infercrane.dev/quality-eval-set/v1","name":"s","version":"1","prompts":[{"id":"a","prompt":"hi"}]}`
		}, "quality_eval_set_not_graded"},
		"too few prompts": {func(s *fakeMigrationStore, _ *optimizer.Request) { s.evalSet = graded(10) }, "quality_eval_set_too_small"},
		"task set": {func(s *fakeMigrationStore, _ *optimizer.Request) {
			s.evalSet = strings.Replace(graded(30), `"grading":"exact_match"`, `"grading":"answer_line"`, 1)
		}, ""},
		"customer evidence": {func(s *fakeMigrationStore, r *optimizer.Request) {
			s.evalSet, r.QualityCheck, r.QualityEvalSet = "", optimizer.QualityCheckCustomer, ""
		}, ""},
	} {
		proofStore, _ := migrationAPI(t)
		proofStore.evalSet = graded(30)
		request := input
		migration := *input.Migration
		request.Migration = &migration
		test.edit(proofStore, &request)
		response := httptest.NewRecorder()
		ok := (API{Store: proofStore}).validMigrationCampaign(response, httptest.NewRequest(http.MethodPost, "/", nil).WithContext(context.Background()), "tenant-a", request)
		var body struct {
			Error struct{ Code string } `json:"error"`
		}
		_ = json.Unmarshal(response.Body.Bytes(), &body)
		if ok != (test.code == "") || body.Error.Code != test.code {
			t.Fatalf("%s: ok=%v code=%q body=%s", name, ok, body.Error.Code, response.Body.String())
		}
	}
}
