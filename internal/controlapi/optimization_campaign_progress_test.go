package controlapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/optimizer"
)

type progressCampaignStore struct {
	*fakeOptimizationCampaignStore
	spans map[string][]domain.ReplicaSpan
}

func (s *progressCampaignStore) OptimizationReplicaSpans(_ context.Context, tenant, deploymentName, revisionID string, _ time.Time) ([]domain.ReplicaSpan, error) {
	if tenant != "global" {
		return nil, nil
	}
	return s.spans[deploymentName+revisionID], nil
}

// liveRunCampaign is the 2026-11-17 Qwen3-0.6B run on an RTX 4090, read while
// the second setup measured: the control is ranked, so the aggregate state is
// "ranked", and the third setup has not started.
func liveRunCampaign(t *testing.T, now time.Time) domain.OptimizationCampaign {
	t.Helper()
	draft := func(args ...string) optimizer.DeploymentDraft {
		value := optimizer.DeploymentDraft{Name: "qwen-rtx-optimized"}
		value.Model.ID, value.Runtime.Engine, value.Runtime.Args = "Qwen/Qwen3-0.6B", "vllm", args
		value.Scaling.MinReplicas, value.Scaling.MaxReplicas = 1, 1
		return value
	}
	proposed := []optimizer.Candidate{
		{ID: "p-defaults", Features: []optimizer.Feature{{Name: "runtime_args", State: "baseline", Source: "Your current setup (vLLM defaults)"}}, Deployment: draft()},
		{ID: "p-ngram", Features: []optimizer.Feature{{Name: "runtime_args", State: "candidate", Source: "Speculative decoding (n-gram)"}}, Deployment: draft("--speculative-config", `{"method":"ngram","num_speculative_tokens":4}`)},
		{ID: "p-fp8", Features: []optimizer.Feature{{Name: "runtime_args", State: "candidate", Source: "FP8 KV cache, smaller prefill chunks"}}, Deployment: draft("--kv-cache-dtype", "fp8", "--max-num-batched-tokens", "2048")},
	}
	proposal, _ := json.Marshal(optimizer.Proposal{Candidates: proposed, StartingPoint: &optimizer.StartingPoint{CandidateID: "p-defaults"}})
	at := func(minutes float64) string {
		return now.Add(time.Duration(minutes * float64(time.Minute))).Format(time.RFC3339Nano)
	}
	approved, capUSD := now.Add(-80*time.Minute), 3.3
	runs := []domain.OptimizationCandidateRun{
		{ID: "91f3961c0000", ProposalCandidateID: "p-defaults", Rank: 1, State: "ranked", BenchmarkID: "bench-1", DeploymentName: "qwen-rtx-optimized-opt-91f3961c", RevisionID: "rev-control",
			StateEnteredAtJSON: `{"proposed":"` + at(-85) + `","provisioning":"` + at(-80) + `","ready":"` + at(-65) + `","measuring":"` + at(-65) + `","validating":"` + at(-58) + `","ranked":"` + at(-57) + `"}`,
			CostQuoteJSON:      `{"hourly_usd":0.4,"max_replicas":1,"source":"vast-offers","observed_at":"` + at(-80) + `"}`,
			MeasurementProgressJSON: `{"method":"campaign-interleaved-control-v1","started_at":"` + at(-65) + `","updated_at":"` + at(-58) + `","turns_planned":3,"warmups_planned":1,"runs":[` +
				`{"kind":"warmup","arm":"candidate","started_at":"` + at(-65) + `","ended_at":"` + at(-64) + `","duration_seconds":60,"requests":16},` +
				`{"kind":"turn","arm":"candidate","turn":1,"started_at":"` + at(-64) + `","ended_at":"` + at(-62) + `","duration_seconds":120,"requests":200,"output_token_throughput":246},` +
				`{"kind":"turn","arm":"candidate","turn":2,"started_at":"` + at(-62) + `","ended_at":"` + at(-60) + `","duration_seconds":120,"requests":200,"output_token_throughput":247},` +
				`{"kind":"turn","arm":"candidate","turn":3,"started_at":"` + at(-60) + `","ended_at":"` + at(-58) + `","duration_seconds":120,"requests":200,"output_token_throughput":245}]}`},
		{ID: "a59d02bb0000", ProposalCandidateID: "p-ngram", Rank: 2, State: "measuring", DeploymentName: "qwen-rtx-optimized-opt-a59d02bb", RevisionID: "rev-ngram",
			StateEnteredAtJSON: `{"proposed":"` + at(-85) + `","provisioning":"` + at(-57) + `","ready":"` + at(-42) + `","measuring":"` + at(-42) + `"}`,
			CostQuoteJSON:      `{"hourly_usd":0.4,"max_replicas":1,"source":"vast-offers","observed_at":"` + at(-57) + `"}`,
			MeasurementProgressJSON: `{"method":"campaign-interleaved-control-v1","started_at":"` + at(-42) + `","updated_at":"` + at(-1) + `","turns_planned":7,"warmups_planned":2,"runs":[` +
				`{"kind":"warmup","arm":"control","started_at":"` + at(-42) + `","ended_at":"` + at(-41) + `","duration_seconds":60,"requests":16},` +
				`{"kind":"warmup","arm":"candidate","started_at":"` + at(-41) + `","ended_at":"` + at(-40) + `","duration_seconds":60,"requests":16},` +
				`{"kind":"turn","arm":"control","turn":1,"started_at":"` + at(-40) + `","ended_at":"` + at(-38) + `","duration_seconds":120,"requests":200,"output_token_throughput":246},` +
				`{"kind":"turn","arm":"candidate","turn":2,"started_at":"` + at(-1) + `","requests":200}]}`},
		{ID: "26d5f7270000", ProposalCandidateID: "p-fp8", Rank: 3, State: "proposed", StateEnteredAtJSON: `{"proposed":"` + at(-85) + `"}`, MeasurementProgressJSON: `{}`, CostQuoteJSON: `{}`},
	}
	for index := range runs {
		spec, _ := json.Marshal(proposed[index].Deployment)
		runs[index].DeploymentSpecJSON, runs[index].PredictedEvidenceJSON, runs[index].ActualEvidenceJSON = string(spec), `{}`, `{}`
	}
	return domain.OptimizationCampaign{ID: "83ccd598", TenantID: "global", ModelIdentity: "Qwen/Qwen3-0.6B", Objective: "interactive", Intent: "new_endpoint", State: "ranked", ProposalJSON: string(proposal), ApprovedAt: &approved, ApprovedMaxCostUSD: &capUSD, Candidates: runs}
}

func TestCampaignReadNamesSetupsAndReportsLiveProgressAndSpend(t *testing.T) {
	now := time.Now().UTC()
	campaign := liveRunCampaign(t, now)
	deleted := now.Add(-50 * time.Minute)
	store := &progressCampaignStore{fakeOptimizationCampaignStore: &fakeOptimizationCampaignStore{fakeStore: &fakeStore{}, campaign: campaign}, spans: map[string][]domain.ReplicaSpan{
		"rev-control": {{ReplicaID: "r1", CreatedAt: now.Add(-80 * time.Minute), DeletedAt: &deleted}},
		"rev-ngram":   {{ReplicaID: "r2", CreatedAt: now.Add(-57 * time.Minute)}},
	}}
	request := httptest.NewRequest(http.MethodGet, "/api/v1/optimization/campaigns/83ccd598", nil)
	request.Header.Set("Authorization", "Bearer secret")
	response := httptest.NewRecorder()
	(API{Store: store, APIKey: "secret"}).Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var body struct {
		Campaign json.RawMessage `json:"campaign"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal(body.Campaign, &document); err != nil {
		t.Fatalf("%v: %s", err, response.Body.String())
	}
	assertMatchesDocumentedSchema(t, "OptimizationCampaign", document)
	var read struct {
		State      string `json:"state"`
		Candidates []struct {
			Label          string               `json:"label"`
			Control        bool                 `json:"control"`
			StateEnteredAt map[string]time.Time `json:"state_entered_at"`
			Measurement    *struct {
				TurnsPlanned int `json:"turns_planned"`
				TurnsDone    int `json:"turns_done"`
				Current      *struct {
					Kind string `json:"kind"`
					Arm  string `json:"arm"`
					Turn int    `json:"turn"`
				} `json:"current"`
			} `json:"measurement"`
		} `json:"candidates"`
		Progress struct {
			Stage          string   `json:"stage"`
			ElapsedSeconds *float64 `json:"elapsed_seconds"`
			TurnsDone      int      `json:"turns_done"`
			TurnsTotal     *int     `json:"turns_total"`
			ETA            struct {
				State            string   `json:"state"`
				Basis            string   `json:"basis"`
				RemainingSeconds *float64 `json:"remaining_seconds"`
			} `json:"eta"`
		} `json:"progress"`
		Spend struct {
			State        string   `json:"state"`
			Label        string   `json:"label"`
			EstimatedUSD *float64 `json:"estimated_usd"`
			CapUSD       *float64 `json:"cap_usd"`
		} `json:"spend"`
	}
	if err := json.Unmarshal(body.Campaign, &read); err != nil {
		t.Fatal(err)
	}
	labels := []string{read.Candidates[0].Label, read.Candidates[1].Label, read.Candidates[2].Label}
	if strings.Join(labels, " | ") != "Your current setup (vLLM defaults) | Speculative decoding (n-gram) | FP8 KV cache, smaller prefill chunks" {
		t.Fatalf("labels = %v", labels)
	}
	if !read.Candidates[0].Control || read.Candidates[1].Control || read.Candidates[2].Control {
		t.Fatal("only the starting point is the control")
	}
	if read.Candidates[1].StateEnteredAt["measuring"].IsZero() || !read.Candidates[2].StateEnteredAt["measuring"].IsZero() {
		t.Fatalf("state entry times = %+v", read.Candidates[1].StateEnteredAt)
	}
	live := read.Candidates[1].Measurement
	if live == nil || live.TurnsPlanned != 7 || live.TurnsDone != 1 || live.Current == nil || live.Current.Turn != 2 || live.Current.Arm != "candidate" || read.Candidates[2].Measurement != nil {
		t.Fatalf("live measurement = %+v", live)
	}
	if read.State != "ranked" || read.Progress.Stage != "running" || read.Progress.TurnsDone != 4 || read.Progress.TurnsTotal == nil || *read.Progress.TurnsTotal != 19 || read.Progress.ElapsedSeconds == nil || *read.Progress.ElapsedSeconds < 4790 {
		t.Fatalf("progress = %+v", read.Progress)
	}
	if read.Progress.ETA.State != "estimated" || read.Progress.ETA.Basis != "observed_durations" || read.Progress.ETA.RemainingSeconds == nil || *read.Progress.ETA.RemainingSeconds <= 0 {
		t.Fatalf("eta = %+v", read.Progress.ETA)
	}
	// $0.40/h for the control's 30 minutes plus the n-gram setup's 57 so far.
	if read.Spend.State != "estimated" || read.Spend.Label != "estimated from quoted rate" || read.Spend.EstimatedUSD == nil || *read.Spend.EstimatedUSD < 0.57 || *read.Spend.EstimatedUSD > 0.59 || read.Spend.CapUSD == nil || *read.Spend.CapUSD != 3.3 {
		t.Fatalf("spend = %+v", read.Spend)
	}

	// Without a replica lookup the spend stays unknown, never zero.
	plain := &fakeOptimizationCampaignStore{fakeStore: &fakeStore{}, campaign: campaign}
	response = httptest.NewRecorder()
	(API{Store: plain, APIKey: "secret"}).Handler().ServeHTTP(response, request)
	if !strings.Contains(response.Body.String(), `"spend":{"state":"unknown","basis":"quoted_rate_x_replica_seconds","label":"estimated from quoted rate","estimated_usd":null,"replica_seconds":null,"cap_usd":3.3,"reason":"a setup's servers cannot be attributed yet"}`) {
		t.Fatalf("spend without replica lookup: %s", response.Body.String())
	}
}

type customerEndpointStore struct {
	*fakeEndpointStore
	customer []domain.Endpoint
}

func (s *customerEndpointStore) CustomerEndpointsForTenant(context.Context, string) ([]domain.Endpoint, error) {
	return s.customer, nil
}

func TestEndpointListShowsOnlyCustomerEndpoints(t *testing.T) {
	store := &customerEndpointStore{fakeEndpointStore: &fakeEndpointStore{fakeStore: &fakeStore{}}, customer: []domain.Endpoint{{ID: "e1", TenantID: "global", Name: "qwen-rtx", DesiredState: "serving"}}}
	request := httptest.NewRequest(http.MethodGet, "/api/v1/endpoints", nil)
	request.Header.Set("Authorization", "Bearer secret")
	response := httptest.NewRecorder()
	(API{Store: store, APIKey: "secret"}).Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"name":"qwen-rtx"`) {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}
