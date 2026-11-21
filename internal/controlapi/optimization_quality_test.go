package controlapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/optimizationcampaign"
	"github.com/infercrane/infercrane/internal/optimizer"
	"github.com/infercrane/infercrane/internal/passport"
	"github.com/infercrane/infercrane/internal/qualitycheck"
	"github.com/infercrane/infercrane/internal/qualityevidence"
)

func qualityTestCampaign(t *testing.T, input optimizer.Request, candidate domain.OptimizationCandidateRun) domain.OptimizationCampaign {
	t.Helper()
	proposal, err := json.Marshal(optimizer.Proposal{Input: input})
	if err != nil {
		t.Fatal(err)
	}
	return domain.OptimizationCampaign{ID: "campaign-1", Intent: optimizationcampaign.IntentNewEndpoint, ProposalJSON: string(proposal), Candidates: []domain.OptimizationCandidateRun{candidate}}
}

func TestCandidateQualityStatesComeFromPersistedState(t *testing.T) {
	validating := domain.OptimizationCandidateRun{ID: "c1", State: optimizationcampaign.CandidateValidating, DeploymentName: "qwen-opt-1", RevisionID: "rev-1"}
	set := qualitycheck.Default()
	digest := "sha256:" + strings.Repeat("b", 64)
	missing, present := false, true

	checking := candidateQuality(qualityTestCampaign(t, optimizer.Request{}, validating), validating, campaignQualityInputs{})
	if checking.State != qualityStateChecking || checking.Check != "builtin" || checking.PromptCount != len(set.Prompts) || checking.Suite != set.Name || checking.Command != "" {
		t.Fatalf("checking = %+v", checking)
	}

	customer := candidateQuality(qualityTestCampaign(t, optimizer.Request{QualityCheck: optimizer.QualityCheckCustomer}, validating), validating, campaignQualityInputs{})
	if customer.State != qualityStateWaitingEvidence || customer.Command != "infercrane evaluation ingest qwen-opt-1 rev-1 --result ./evaluator-result.json --key ./quality-evidence.key --file ./quality-evidence.json --attach" {
		t.Fatalf("customer evidence wait = %+v", customer)
	}

	waiting := candidateQuality(qualityTestCampaign(t, optimizer.Request{QualityEvalSet: digest}, validating), validating, campaignQualityInputs{evalSetUploaded: &missing})
	if waiting.State != qualityStateWaitingForEvalSet || waiting.EvalSet != digest || !strings.Contains(waiting.Reason, digest) || waiting.Command != "infercrane evaluation set upload ./eval-set.json" {
		t.Fatalf("eval set wait = %+v", waiting)
	}
	uploaded := candidateQuality(qualityTestCampaign(t, optimizer.Request{QualityEvalSet: digest}, validating), validating, campaignQualityInputs{evalSetUploaded: &present})
	if uploaded.State != qualityStateChecking || uploaded.PromptCount != 0 {
		t.Fatalf("uploaded eval set = %+v", uploaded)
	}

	measuring := validating
	measuring.State = optimizationcampaign.CandidateMeasuring
	if view := candidateQuality(qualityTestCampaign(t, optimizer.Request{}, measuring), measuring, campaignQualityInputs{}); view.State != qualityStatePending {
		t.Fatalf("measuring = %+v", view)
	}
	noReference := validating
	noReference.State, noReference.FailureCode = optimizationcampaign.CandidateInconclusive, optimizationcampaign.FailureQualityReferenceUnavailable
	if view := candidateQuality(qualityTestCampaign(t, optimizer.Request{}, noReference), noReference, campaignQualityInputs{}); view.State != qualityStateInconclusive {
		t.Fatalf("no reference = %+v", view)
	}
	unanswered := validating
	unanswered.State, unanswered.FailureCode = optimizationcampaign.CandidateInconclusive, optimizationcampaign.FailureQualityReferenceUnanswered
	if view := candidateQuality(qualityTestCampaign(t, optimizer.Request{}, unanswered), unanswered, campaignQualityInputs{}); view.State != qualityStateInconclusive || !strings.Contains(view.Reason, "no answer text") {
		t.Fatalf("unanswered reference = %+v", view)
	}
	nothingChecked := validating
	nothingChecked.State, nothingChecked.FailureCode = optimizationcampaign.CandidateInconclusive, optimizationcampaign.FailureQualityNothingChecked
	if view := candidateQuality(qualityTestCampaign(t, optimizer.Request{}, nothingChecked), nothingChecked, campaignQualityInputs{}); view.State != qualityStateInconclusive || !strings.Contains(view.Reason, "refused requests with tools") {
		t.Fatalf("nothing checked = %+v", view)
	}
	failed := validating
	failed.State = optimizationcampaign.CandidateFailed
	if view := candidateQuality(qualityTestCampaign(t, optimizer.Request{}, failed), failed, campaignQualityInputs{}); view.State != qualityStateNotRun {
		t.Fatalf("failed before the check = %+v", view)
	}
}

func TestCandidateQualityReportsBuiltinAndCustomerVerdicts(t *testing.T) {
	ranked := domain.OptimizationCandidateRun{ID: "c1", State: optimizationcampaign.CandidateRanked, DeploymentName: "qwen-opt-1", RevisionID: "rev-1", QualityEvidenceID: "q1"}
	campaign := qualityTestCampaign(t, optimizer.Request{}, ranked)
	builtin := func(score float64, passed bool, reference string) campaignQualityInputs {
		return campaignQualityInputs{evidence: map[string]domain.QualityEvidence{"q1": {ID: "q1", RevisionID: "rev-1", Source: domain.QualityEvidenceSourceInferCrane, ReferenceRevisionID: reference, Suite: "infercrane-default", SuiteVersion: "1@x", Evaluator: qualitycheck.Evaluator, Score: score, Passed: passed, SampleCount: 24}}}
	}
	for _, tc := range []struct {
		name      string
		inputs    campaignQualityInputs
		state     string
		role      string
		substring string
	}{
		{"pass", builtin(0.98, true, "rev-control"), qualityStatePassed, "candidate", "matched the reference 98% across 24 prompts"},
		{"watch", builtin(0.92, true, "rev-control"), qualityStateWatch, "candidate", "watch band"},
		{"block", builtin(0.80, false, "rev-control"), qualityStateBlocked, "candidate", "More than 10% different"},
		{"reference", builtin(1, true, "rev-1"), qualityStatePassed, "reference", "are the reference"},
	} {
		view := candidateQuality(campaign, ranked, tc.inputs)
		if view.State != tc.state || view.Role != tc.role || view.Source != domain.QualityEvidenceSourceInferCrane || view.Score == nil || view.BlockLimitPercent == nil || *view.BlockLimitPercent != 10 || !strings.Contains(view.Reason, tc.substring) {
			t.Errorf("%s: %+v", tc.name, view)
		}
	}
	withToolCalls := func(score float64, passed bool, broken int) campaignQualityInputs {
		inputs := builtin(score, passed, "rev-control")
		row := inputs.evidence["q1"]
		row.DistributionJSON = fmt.Sprintf(`{"schema":"infercrane.dev/quality-distribution/v1","kind":"paired_scores","pairing_digest":"sha256:%s","scores":[],"tool_calls":{"cases":3,"score":%v,"broken":%d,"reference_invalid":0,"candidate":{"missing_call":%d,"malformed_arguments":0,"wrong_function":0,"schema_invalid":0,"extra_calls":0,"unexpected_call":0},"reference":{"missing_call":0,"malformed_arguments":0,"wrong_function":0,"schema_invalid":0,"extra_calls":0,"unexpected_call":0}}}`, strings.Repeat("a", 64), map[int]float64{0: 1, 1: 0.6667}[broken], broken, broken)
		inputs.evidence["q1"] = row
		return inputs
	}
	// A broken tool call blocks even when the mean is inside the PASS band.
	broken := candidateQuality(campaign, ranked, withToolCalls(0.96, false, 1))
	if broken.State != qualityStateBlocked || broken.ToolCalls == nil || broken.ToolCalls.Broken != 1 || broken.ToolCalls.Candidate.MissingCall != 1 || !strings.Contains(broken.Reason, "broke 1 of 3 tool calls (missing call 1)") || !strings.Contains(broken.Reason, "tool calls matched 67% across 3") {
		t.Fatalf("broken tool call = %+v", broken)
	}
	// Tool cases the reference refused are named as not checked, never as
	// a match, and no tool-call score is claimed when none was scored.
	withUnchecked := func(calls string) campaignQualityInputs {
		inputs := builtin(0.99, true, "rev-control")
		row := inputs.evidence["q1"]
		row.DistributionJSON = `{"schema":"infercrane.dev/quality-distribution/v1","kind":"paired_scores","pairing_digest":"sha256:` + strings.Repeat("a", 64) + `","scores":[],"tool_calls":` + calls + `}`
		inputs.evidence["q1"] = row
		return inputs
	}
	unchecked := candidateQuality(campaign, ranked, withUnchecked(`{"cases":0,"not_checked":5,"score":0,"broken":0,"reference_invalid":0,"candidate":{},"reference":{}}`))
	if unchecked.State != qualityStatePassed || unchecked.ToolCalls == nil || unchecked.ToolCalls.NotChecked != 5 || strings.Contains(unchecked.Reason, "tool calls matched") || !strings.Contains(unchecked.Reason, "5 tool cases not checked because the reference refused requests with tools") {
		t.Fatalf("unchecked tool cases = %+v", unchecked)
	}
	refused := candidateQuality(campaign, ranked, withUnchecked(`{"cases":2,"not_checked":3,"score":0.5,"broken":1,"reference_invalid":0,"candidate":{"tools_refused":1},"reference":{}}`))
	if refused.State != qualityStateBlocked || !strings.Contains(refused.Reason, "broke 1 of 2 tool calls (refused tools the reference accepted 1)") || !strings.Contains(refused.Reason, "3 tool cases not checked") {
		t.Fatalf("candidate that refused tools = %+v", refused)
	}
	kept := candidateQuality(campaign, ranked, withToolCalls(0.99, true, 0))
	if kept.State != qualityStatePassed || kept.ToolCalls == nil || kept.ToolCalls.Cases != 3 || !strings.Contains(kept.Reason, "tool calls matched 100% across 3") {
		t.Fatalf("kept tool calls = %+v", kept)
	}
	customer := candidateQuality(campaign, ranked, campaignQualityInputs{evidence: map[string]domain.QualityEvidence{"q1": {ID: "q1", Source: domain.QualityEvidenceSourceCustomer, Suite: "support", Score: 0.4, Passed: false}}})
	if customer.State != qualityStateBlocked || customer.Source != domain.QualityEvidenceSourceCustomer || customer.BlockLimitPercent != nil || !strings.Contains(customer.Reason, "Your signed evaluation support failed") {
		t.Fatalf("customer = %+v", customer)
	}
	unread := candidateQuality(campaign, ranked, campaignQualityInputs{})
	if unread.State != qualityStatePassed || unread.Score != nil || unread.EvidenceID != "q1" {
		t.Fatalf("unread evidence = %+v", unread)
	}
}

type fakeEvalSetStore struct {
	*fakeQualityEvidenceStore
	sets  map[string]domain.QualityEvalSet
	audit []string
}

func (f *fakeEvalSetStore) RecordQualityEvalSet(_ context.Context, tenant, actor string, body []byte) (domain.QualityEvalSet, bool, error) {
	set, err := qualitycheck.DecodeEvalSet(body)
	if err != nil {
		return domain.QualityEvalSet{}, false, err
	}
	if existing, ok := f.sets[set.Digest]; ok {
		return existing, false, nil
	}
	row := domain.QualityEvalSet{ID: "set-1", TenantID: tenant, Digest: set.Digest, Name: set.Name, Version: set.Version, PromptCount: len(set.Prompts), Body: string(body), CreatedBy: actor, CreatedAt: time.Now().UTC()}
	f.sets[set.Digest] = row
	return row, true, nil
}

func (f *fakeEvalSetStore) QualityEvalSet(_ context.Context, _, digest string) (domain.QualityEvalSet, error) {
	row, ok := f.sets[digest]
	if !ok {
		return domain.QualityEvalSet{}, domain.ErrNotFound
	}
	return row, nil
}

func (f *fakeEvalSetStore) DeleteQualityEvalSet(_ context.Context, _, digest string) error {
	if _, ok := f.sets[digest]; !ok {
		return domain.ErrNotFound
	}
	delete(f.sets, digest)
	return nil
}

func TestQualityEvalSetUploadInspectAndDelete(t *testing.T) {
	store := &fakeEvalSetStore{fakeQualityEvidenceStore: &fakeQualityEvidenceStore{fakeStore: &fakeStore{}}, sets: map[string]domain.QualityEvalSet{}}
	handler := (API{Store: store, APIKey: "secret"}).Handler()
	send := func(method, path, body string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(method, path, strings.NewReader(body))
		request.Header.Set("Authorization", "Bearer secret")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}
	body := `{"schema":"infercrane.dev/quality-eval-set/v1","name":"support","version":"2","prompts":[{"id":"refund","prompt":"How do I get a refund?"}]}`
	digest := qualitycheck.Digest([]byte(body))
	response := send(http.MethodPost, "/api/v1/quality/eval-sets", body)
	if response.Code != http.StatusCreated || !strings.Contains(response.Body.String(), `"digest":"`+digest+`"`) || !strings.Contains(response.Body.String(), `"prompt_count":1`) || strings.Contains(response.Body.String(), "refund") {
		t.Fatalf("upload status=%d body=%s", response.Code, response.Body.String())
	}
	if response = send(http.MethodPost, "/api/v1/quality/eval-sets", body); response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"created":false`) {
		t.Fatalf("repeat upload status=%d body=%s", response.Code, response.Body.String())
	}
	if response = send(http.MethodPost, "/api/v1/quality/eval-sets", `{"schema":"infercrane.dev/quality-eval-set/v1","name":"s","version":"1","prompts":[{"id":"a","prompt":"x","expected":"y"}]}`); response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("eval set with expected answers status=%d body=%s", response.Code, response.Body.String())
	}
	if response = send(http.MethodPost, "/api/v1/quality/eval-sets", strings.Repeat(" ", qualitycheck.MaxEvalSetBytes+1)); response.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized eval set status=%d", response.Code)
	}
	if response = send(http.MethodGet, "/api/v1/quality/eval-sets/"+digest, ""); response.Code != http.StatusOK || strings.Contains(response.Body.String(), "refund") {
		t.Fatalf("inspect status=%d body=%s", response.Code, response.Body.String())
	}
	tools, err := os.ReadFile("../../examples/evaluation/eval-set-tools.json")
	if err != nil {
		t.Fatal(err)
	}
	if response = send(http.MethodPost, "/api/v1/quality/eval-sets", string(tools)); response.Code != http.StatusCreated || !strings.Contains(response.Body.String(), `"prompt_count":4`) || strings.Contains(response.Body.String(), "get_order") {
		t.Fatalf("v2 upload status=%d body=%s", response.Code, response.Body.String())
	}
	if response = send(http.MethodGet, "/api/v1/quality/eval-sets/not-a-digest", ""); response.Code != http.StatusBadRequest {
		t.Fatalf("bad digest status=%d", response.Code)
	}
	if response = send(http.MethodDelete, "/api/v1/quality/eval-sets/"+digest, ""); response.Code != http.StatusNoContent {
		t.Fatalf("delete status=%d body=%s", response.Code, response.Body.String())
	}
	if response = send(http.MethodGet, "/api/v1/quality/eval-sets/"+digest, ""); response.Code != http.StatusNotFound {
		t.Fatalf("deleted eval set status=%d", response.Code)
	}
}

func TestQualityEvidenceAPIRefusesTheBuiltinEvaluatorName(t *testing.T) {
	base := &fakeStore{resolved: domain.ResolvedDeployment{Deployment: domain.Deployment{ID: "deployment-1", Name: "prod", ActiveRevisionID: "rev-1"}}}
	store := &fakeQualityEvidenceStore{fakeStore: base}
	handler := (API{Store: store, APIKey: "secret"}).Handler()
	_, key, err := passport.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	payload := qualityevidence.Payload{Schema: qualityevidence.Schema, Deployment: "prod", RevisionID: "rev-2", Suite: "infercrane-default", SuiteVersion: "1", Evaluator: "InferCrane-Builtin", EvaluatorVersion: "1", Score: 1, Passed: true, SampleCount: 24, ArtifactDigest: "sha256:" + strings.Repeat("a", 64), EvaluatedAt: time.Now().UTC()}
	envelope, err := passport.Sign(payload, key)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(envelope)
	request := httptest.NewRequest(http.MethodPost, "/api/v1/deployments/prod/quality-evidence", strings.NewReader(string(body)))
	request.Header.Set("Authorization", "Bearer secret")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusUnprocessableEntity || len(store.evidence) != 0 || !strings.Contains(response.Body.String(), "reserved_quality_evaluator") {
		t.Fatalf("status=%d evidence=%+v body=%s", response.Code, store.evidence, response.Body.String())
	}
	payload.Evaluator = "customer-eval"
	envelope, _ = passport.Sign(payload, key)
	body, _ = json.Marshal(envelope)
	request = httptest.NewRequest(http.MethodPost, "/api/v1/deployments/prod/quality-evidence", strings.NewReader(string(body)))
	request.Header.Set("Authorization", "Bearer secret")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusCreated || len(store.evidence) != 1 || store.evidence[0].Source != domain.QualityEvidenceSourceCustomer || !strings.Contains(response.Body.String(), `"source":"customer"`) {
		t.Fatalf("customer attach status=%d evidence=%+v body=%s", response.Code, store.evidence, response.Body.String())
	}
}
