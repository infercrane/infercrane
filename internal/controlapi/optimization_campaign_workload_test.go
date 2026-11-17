package controlapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/integration"
	"github.com/infercrane/infercrane/internal/optimizer"
	"github.com/infercrane/infercrane/internal/workloadprofile"
)

// The campaign read states the load its immutable proposal bound it to, so a
// client can tell a run on the customer's recorded traffic from one on the
// public prior or a synthetic profile without waiting for a measurement.
func TestCampaignReturnsItsBoundWorkload(t *testing.T) {
	replayDigest := "sha256:" + strings.Repeat("b", 64)
	for _, test := range []struct {
		name, body, key string
		want            campaignWorkloadView
	}{
		{
			name: "customer replay",
			body: `{"model_identity":"qwen3-8b","provider":"aws","region":"eu-central-1","gpu":"L40S","objective":"interactive","workload_source":"customer_observed","workload_fingerprint":"` + replayDigest + `","max_candidates":1}`,
			key:  "campaign-workload-replay",
			want: campaignWorkloadView{Source: optimizer.WorkloadSourceCustomerObserved, Fingerprint: replayDigest, Profile: "interactive"},
		},
		{
			name: "public prior",
			body: `{"model_identity":"qwen3-8b","provider":"aws","region":"eu-central-1","gpu":"L40S","objective":"interactive","workload_profile":"public-interactive","max_candidates":1}`,
			key:  "campaign-workload-prior",
			want: campaignWorkloadView{Source: optimizer.WorkloadSourcePublicPrior, Fingerprint: workloadprofile.PublicPriorDigest, Profile: "public-interactive"},
		},
		{
			name: "reviewed synthetic profile",
			body: `{"model_identity":"qwen3-8b","provider":"aws","region":"eu-central-1","gpu":"L40S","objective":"throughput","workload_profile":"throughput","max_candidates":1}`,
			key:  "campaign-workload-default",
			want: campaignWorkloadView{Source: optimizer.WorkloadSourceReviewedDefault, Fingerprint: "", Profile: "throughput"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			registry, err := integration.V1Catalog()
			if err != nil {
				t.Fatal(err)
			}
			store := &fakeOptimizationCampaignStore{fakeStore: &fakeStore{}}
			handler := (API{Store: store, APIKey: "secret", OptimizationCosts: fakeOptimizationCosts{}, Integrations: registry.Snapshot()}).Handler()
			call := func(method, path, body, key string) *httptest.ResponseRecorder {
				t.Helper()
				request := httptest.NewRequest(method, path, strings.NewReader(body))
				request.Header.Set("Authorization", "Bearer secret")
				if key != "" {
					request.Header.Set("Idempotency-Key", key)
				}
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, request)
				return response
			}

			response := call(http.MethodPost, "/api/v1/optimization/proposals", test.body, "")
			if response.Code != http.StatusOK {
				t.Fatalf("proposal status=%d body=%s", response.Code, response.Body.String())
			}
			var proposed struct {
				Proposal optimizer.Proposal `json:"proposal"`
			}
			if err = json.Unmarshal(response.Body.Bytes(), &proposed); err != nil {
				t.Fatal(err)
			}
			encoded, err := json.Marshal(map[string]any{"proposal": proposed.Proposal})
			if err != nil {
				t.Fatal(err)
			}
			response = call(http.MethodPost, "/api/v1/optimization/campaigns", string(encoded), test.key)
			if response.Code != http.StatusCreated {
				t.Fatalf("create status=%d body=%s", response.Code, response.Body.String())
			}
			assertCampaignWorkload(t, "create", response.Body.Bytes(), "campaign", test.want)

			response = call(http.MethodGet, "/api/v1/optimization/campaigns/"+store.campaign.ID, "", "")
			if response.Code != http.StatusOK {
				t.Fatalf("get status=%d body=%s", response.Code, response.Body.String())
			}
			campaign := assertCampaignWorkload(t, "get", response.Body.Bytes(), "campaign", test.want)
			assertMatchesDocumentedSchema(t, "OptimizationCampaign", campaign)

			response = call(http.MethodGet, "/api/v1/optimization/campaigns", "", "")
			if response.Code != http.StatusOK {
				t.Fatalf("list status=%d body=%s", response.Code, response.Body.String())
			}
			var listed struct {
				Data []json.RawMessage `json:"data"`
			}
			if err = json.Unmarshal(response.Body.Bytes(), &listed); err != nil || len(listed.Data) != 1 {
				t.Fatalf("list body=%s err=%v", response.Body.String(), err)
			}
			assertCampaignWorkload(t, "list", listed.Data[0], "", test.want)
		})
	}
}

// A workload the proposal did not record stays absent: never a default source.
func TestCampaignWorkloadStaysAbsentWhenUnrecorded(t *testing.T) {
	for _, stored := range []string{"", "{", `{"input":"not-an-object"}`, `{"schema_version":"infercrane.optimizer.proposal/v1"}`, `{"input":{"workload_profile":"interactive"}}`} {
		if workload, present := optimizationCampaignResponse(domain.OptimizationCampaign{ProposalJSON: stored}, campaignQualityInputs{})["workload"]; present {
			t.Fatalf("proposal %q recorded no workload source, got %#v", stored, workload)
		}
	}
	// Warnings stay readable when only the workload is missing.
	if warnings, present := optimizationCampaignResponse(domain.OptimizationCampaign{ProposalJSON: `{"input":{"workload_profile":"interactive"}}`}, campaignQualityInputs{})["warnings"]; !present {
		t.Fatalf("a readable proposal without a workload source must still return warnings, got %#v", warnings)
	}
}

func assertCampaignWorkload(t *testing.T, label string, body []byte, field string, want campaignWorkloadView) map[string]any {
	t.Helper()
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatalf("%s: %v", label, err)
	}
	if field != "" {
		nested := raw[field]
		raw = nil
		if err := json.Unmarshal(nested, &raw); err != nil {
			t.Fatalf("%s: campaign envelope body=%s", label, body)
		}
	}
	var got campaignWorkloadView
	if err := json.Unmarshal(raw["workload"], &got); err != nil || got != want {
		t.Fatalf("%s: campaign workload=%s want %+v err=%v", label, raw["workload"], want, err)
	}
	var campaign map[string]any
	encoded, _ := json.Marshal(raw)
	if err := json.Unmarshal(encoded, &campaign); err != nil {
		t.Fatal(err)
	}
	return campaign
}
