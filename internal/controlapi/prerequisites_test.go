package controlapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/infercrane/infercrane/internal/integration"
	"github.com/infercrane/infercrane/internal/optimizer"
)

const (
	dynamoSGLangSettings = "INFERCRANE_DYNAMO_SGLANG_IMAGE_DIGEST and INFERCRANE_DYNAMO_SGLANG_RUNTIME_VERSION"
	dynamoDeployBody     = `{"name":"llama-dynamo","model":"meta-llama/Llama-3.1-8B-Instruct","runtime":"RUNTIME","cloud":"kubernetes","provider_adapter":"kubernetes-dynamo","gpu":"nvidia.com/gpu","min_replicas":1,"max_replicas":1,"serving":{"schema_version":"infercrane.serving/v1","backend":"dynamo","profile":"baseline","mode":"aggregated","routing":"direct","worker":{"replicas":1,"tensor_parallelism":1},"autoscaling":{"owner":"disabled"},"cache":{"backend":"none"}}}`
)

// dynamoControlPlane is the integration snapshot of a control plane that
// runs Dynamo graphs with the given runtimes, none for one without Dynamo.
func dynamoControlPlane(t *testing.T, configured ...string) integration.Snapshot {
	t.Helper()
	registry, err := integration.V1Catalog()
	if err != nil {
		t.Fatal(err)
	}
	settings := map[string][]string{
		"vllm":   {"INFERCRANE_DYNAMO_VLLM_IMAGE_DIGEST", "INFERCRANE_DYNAMO_VLLM_RUNTIME_VERSION"},
		"sglang": {"INFERCRANE_DYNAMO_SGLANG_IMAGE_DIGEST", "INFERCRANE_DYNAMO_SGLANG_RUNTIME_VERSION"},
	}
	if err = registry.ConfigurePrerequisite("kubernetes-dynamo", settings, configured...); err != nil {
		t.Fatal(err)
	}
	return registry.Snapshot()
}

type apiError struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func decodeAPIError(t *testing.T, response *httptest.ResponseRecorder) apiError {
	t.Helper()
	var decoded apiError
	if err := json.Unmarshal(response.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("%v: %s", err, response.Body.String())
	}
	return decoded
}

// A Dynamo deploy for a runtime this control plane has no Dynamo image for
// is refused before anything is submitted, and the error names the settings.
// Before, it was accepted and failed in its deploy operation.
func TestADynamoDeployForARuntimeWithoutItsImageIsRefused(t *testing.T) {
	deploy := func(integrations integration.Snapshot, body string) (*httptest.ResponseRecorder, *fakeStore) {
		t.Helper()
		store := &fakeStore{created: true}
		request := httptest.NewRequest(http.MethodPost, "/api/v1/deployments", strings.NewReader(body))
		request.Header.Set("Authorization", "Bearer secret")
		request.Header.Set("Idempotency-Key", "deploy-dynamo")
		response := httptest.NewRecorder()
		(API{Store: store, APIKey: "secret", Integrations: integrations}).Handler().ServeHTTP(response, request)
		return response, store
	}
	vllmOnly := dynamoControlPlane(t, "vllm")
	response, store := deploy(vllmOnly, strings.Replace(dynamoDeployBody, "RUNTIME", "sglang", 1))
	refused := decodeAPIError(t, response)
	if response.Code != http.StatusUnprocessableEntity || refused.Error.Code != "prerequisite_not_configured" || store.operation.ID != "" || store.operation.RequestJSON != "" {
		t.Fatalf("response=%d %s operation=%#v", response.Code, response.Body.String(), store.operation)
	}
	if want := "provider adapter kubernetes-dynamo cannot deploy sglang: this control plane has no NVIDIA Dynamo image configured for sglang (set " + dynamoSGLangSettings + ")"; refused.Error.Message != want {
		t.Fatalf("message %q, want %q", refused.Error.Message, want)
	}

	for name, test := range map[string]struct {
		integrations integration.Snapshot
		body         string
	}{
		"configured runtime": {vllmOnly, strings.Replace(dynamoDeployBody, "RUNTIME", "vllm", 1)},
		// The catalog alone cannot know what the control plane runs.
		"catalog only": {catalogSnapshot(t), strings.Replace(dynamoDeployBody, "RUNTIME", "sglang", 1)},
		// Plain Kubernetes needs nothing beside its cluster.
		"plain Kubernetes": {dynamoControlPlane(t), `{"name":"qwen","model":"Qwen/Qwen3-8B","runtime":"sglang","cloud":"kubernetes","gpu":"NVIDIA-A10G"}`},
	} {
		if response, store = deploy(test.integrations, test.body); response.Code != http.StatusAccepted || store.operation.RequestJSON == "" {
			t.Fatalf("%s: response=%d %s", name, response.Code, response.Body.String())
		}
	}
}

func catalogSnapshot(t *testing.T) integration.Snapshot {
	t.Helper()
	registry, err := integration.V1Catalog()
	if err != nil {
		t.Fatal(err)
	}
	return registry.Snapshot()
}

// A proposal that names kubernetes-dynamo on a control plane without a Dynamo
// image for its runtimes says the setups will fail to deploy, and a run with
// them is refused before it is stored. With the images set, both go through.
func TestARunWithDynamoSetupsTheControlPlaneCannotDeployIsRefused(t *testing.T) {
	for name, test := range map[string]struct {
		configured []string
		refused    bool
	}{
		"no Dynamo":     {refused: true},
		"both runtimes": {configured: []string{"vllm", "sglang"}},
	} {
		t.Run(name, func(t *testing.T) {
			store := &fakeOptimizationCampaignStore{fakeStore: &fakeStore{}}
			handler := (API{Store: store, APIKey: "secret", OptimizationCosts: fakeOptimizationCosts{}, Integrations: dynamoControlPlane(t, test.configured...)}).Handler()
			call := func(path, body, key string) *httptest.ResponseRecorder {
				t.Helper()
				request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
				request.Header.Set("Authorization", "Bearer secret")
				if key != "" {
					request.Header.Set("Idempotency-Key", key)
				}
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, request)
				return response
			}
			response := call("/api/v1/optimization/proposals", `{"model_identity":"Qwen/Qwen3-8B","provider":"kubernetes-dynamo","gpu":"NVIDIA-L40S","objective":"interactive","include_simulated":true,"max_candidates":10}`, "")
			if response.Code != http.StatusOK {
				t.Fatalf("proposal status=%d body=%s", response.Code, response.Body.String())
			}
			var proposed struct {
				Proposal optimizer.Proposal `json:"proposal"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &proposed); err != nil {
				t.Fatal(err)
			}
			dynamo := 0
			for _, candidate := range proposed.Proposal.Candidates {
				if candidate.Deployment.Provider.Adapter == "kubernetes-dynamo" {
					dynamo++
				}
			}
			if dynamo == 0 {
				t.Fatalf("the named adapter has no setup: %+v", proposed.Proposal.Candidates)
			}
			warned := strings.Contains(strings.Join(proposed.Proposal.Warnings, " "), "will fail to deploy: this control plane has no NVIDIA Dynamo image configured for sglang or vllm (set INFERCRANE_DYNAMO_SGLANG_IMAGE_DIGEST")
			if warned != test.refused {
				t.Fatalf("deploy warning=%v want %v: %q", warned, test.refused, proposed.Proposal.Warnings)
			}

			encoded, err := json.Marshal(map[string]any{"proposal": proposed.Proposal})
			if err != nil {
				t.Fatal(err)
			}
			response = call("/api/v1/optimization/campaigns", string(encoded), "dynamo-run-"+strings.ReplaceAll(name, " ", "-"))
			if !test.refused {
				if response.Code != http.StatusCreated {
					t.Fatalf("create status=%d body=%s", response.Code, response.Body.String())
				}
				return
			}
			refused := decodeAPIError(t, response)
			if response.Code != http.StatusUnprocessableEntity || refused.Error.Code != "prerequisite_not_configured" || store.campaign.ID != "" {
				t.Fatalf("create status=%d body=%s stored=%q", response.Code, response.Body.String(), store.campaign.ID)
			}
			if !strings.HasPrefix(refused.Error.Message, "setup ") || !strings.Contains(refused.Error.Message, ": provider adapter kubernetes-dynamo cannot deploy ") || !strings.Contains(refused.Error.Message, "(set INFERCRANE_DYNAMO_") {
				t.Fatalf("message %q", refused.Error.Message)
			}
		})
	}
}
