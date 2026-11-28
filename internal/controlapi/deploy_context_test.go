package controlapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/infercrane/infercrane/internal/modelcheck"
	"github.com/infercrane/infercrane/internal/workflows"
)

type deployContextResponse struct {
	ContextLength *contextLength `json:"context_length"`
}

func submitDeploy(t *testing.T, resolver *modelcheck.Resolver, body string) (*httptest.ResponseRecorder, workflows.CloudRequest, *contextLength) {
	t.Helper()
	store := &fakeStore{created: true}
	api := API{Store: store, APIKey: "secret"}
	if resolver != nil {
		api.ModelCheck = resolver
	}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/deployments", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer secret")
	request.Header.Set("Idempotency-Key", "deploy-context")
	response := httptest.NewRecorder()
	api.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusAccepted {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var submitted workflows.CloudRequest
	if err := json.Unmarshal([]byte(store.operation.RequestJSON), &submitted); err != nil {
		t.Fatal(err)
	}
	var decoded deployContextResponse
	if err := json.Unmarshal(response.Body.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	return response, submitted, decoded.ContextLength
}

// An 8B model's own 40960-token context needs more KV cache than a 24 GB GPU
// holds beside BF16 weights, so vLLM refuses to start. The plain deploy gets
// the check's 8192-token length instead, and the response says why.
func TestPlainDeployBoundsContextTheGPUCannotHold(t *testing.T) {
	resolver, _ := recordedHub(t)
	for _, test := range []struct{ body, flag string }{
		{`{"name":"qwen","model":"Qwen/Qwen3-8B","cloud":"gcp","provider_adapter":"gcp-compute","region":"europe-west4","gpu":"nvidia-l4"}`, "--max-model-len"},
		{`{"name":"qwen","model":"Qwen/Qwen3-8B","cloud":"kubernetes","gpu":"NVIDIA-A10G"}`, "--max-model-len"},
		{`{"name":"qwen","model":"Qwen/Qwen3-8B","runtime":"sglang","cloud":"kubernetes","gpu":"NVIDIA-A10G"}`, "--context-length"},
	} {
		_, submitted, fit := submitDeploy(t, resolver, test.body)
		if !slices.Equal(submitted.RuntimeArgs, []string{test.flag, "8192"}) {
			t.Fatalf("%s: recorded runtime args %v", test.body, submitted.RuntimeArgs)
		}
		if fit == nil || fit.SetBy != contextSetByInferCrane || fit.Tokens == nil || *fit.Tokens != 8192 || fit.ModelTokens == nil || *fit.ModelTokens != 40960 || !strings.Contains(fit.Reason, "40960") || !strings.Contains(fit.Reason, test.flag+" 8192") {
			t.Fatalf("%s: context %+v", test.body, fit)
		}
	}
}

func TestPlainDeployKeepsAContextTheGPUHolds(t *testing.T) {
	resolver, _ := recordedHub(t)
	_, submitted, fit := submitDeploy(t, resolver, `{"name":"qwen","model":"Qwen/Qwen3-8B","cloud":"runpod","gpu":"L40S"}`)
	if len(submitted.RuntimeArgs) != 0 {
		t.Fatalf("a context that fits was bounded: %v", submitted.RuntimeArgs)
	}
	if fit == nil || fit.SetBy != contextSetByRuntime || fit.Tokens != nil || fit.ModelTokens == nil || *fit.ModelTokens != 40960 || !strings.Contains(fit.Reason, "fits one L40S") {
		t.Fatalf("context %+v", fit)
	}
}

// Phi-4's BF16 weights alone exceed a 24 GB GPU. InferCrane says so and
// does not pretend a context length would help.
func TestPlainDeploySaysWhenEvenTheBoundedContextDoesNotFit(t *testing.T) {
	resolver, _ := recordedHub(t)
	_, submitted, fit := submitDeploy(t, resolver, `{"name":"phi","model":"microsoft/phi-4","cloud":"kubernetes","gpu":"NVIDIA-A10G"}`)
	if len(submitted.RuntimeArgs) != 0 {
		t.Fatalf("runtime args %v", submitted.RuntimeArgs)
	}
	if fit == nil || fit.SetBy != contextSetByRuntime || fit.Tokens != nil || !strings.Contains(fit.Reason, "even one request of 8192 tokens does not fit") {
		t.Fatalf("context %+v", fit)
	}
}

// The deployment's own engine arguments stand: InferCrane neither reads the
// model nor adds a context length to them.
func TestDeployWithEngineArgumentsKeepsThem(t *testing.T) {
	resolver, calls := recordedHub(t)
	_, submitted, fit := submitDeploy(t, resolver, `{"name":"qwen","model":"Qwen/Qwen3-8B","cloud":"kubernetes","gpu":"NVIDIA-A10G","runtime_args":["--max-model-len","4096"]}`)
	if !slices.Equal(submitted.RuntimeArgs, []string{"--max-model-len", "4096"}) || fit == nil || fit.SetBy != contextSetByRequest || fit.Tokens == nil || *fit.Tokens != 4096 {
		t.Fatalf("args=%v context=%+v", submitted.RuntimeArgs, fit)
	}
	_, submitted, fit = submitDeploy(t, resolver, `{"name":"qwen","model":"Qwen/Qwen3-8B","cloud":"kubernetes","gpu":"NVIDIA-A10G","runtime_args":["--enable-prefix-caching"]}`)
	if !slices.Equal(submitted.RuntimeArgs, []string{"--enable-prefix-caching"}) || fit == nil || fit.SetBy != contextSetByRuntime || fit.Tokens != nil || !strings.Contains(fit.Reason, "own engine arguments") || !strings.Contains(fit.Reason, "--max-model-len") {
		t.Fatalf("args=%v context=%+v", submitted.RuntimeArgs, fit)
	}
	if calls.Load() != 0 {
		t.Fatalf("Hugging Face was read %d times for a deploy with its own arguments", calls.Load())
	}
}

// Without metadata access a reviewed model is sized from the sizes the
// catalog records for its pinned commit, so a default install bounds it too.
func TestDeployWithoutMetadataAccessSizesAReviewedModel(t *testing.T) {
	for _, body := range []string{
		`{"name":"qwen","model":"Qwen/Qwen3-8B","cloud":"kubernetes","gpu":"NVIDIA-A10G"}`,
		`{"name":"qwen","model":"Qwen/Qwen3-8B","model_revision":"b968826d9c46dd6066d109eabc6255188de91218","cloud":"kubernetes","gpu":"NVIDIA-A10G"}`,
	} {
		_, submitted, fit := submitDeploy(t, nil, body)
		if !slices.Equal(submitted.RuntimeArgs, []string{"--max-model-len", "8192"}) || fit == nil || fit.SetBy != contextSetByInferCrane || fit.Tokens == nil || *fit.Tokens != 8192 || fit.ModelTokens == nil || *fit.ModelTokens != 40960 || !strings.Contains(fit.Reason, "the reviewed catalog records") {
			t.Fatalf("%s: args=%v context=%+v", body, submitted.RuntimeArgs, fit)
		}
	}
	_, submitted, fit := submitDeploy(t, nil, `{"name":"bge","model":"BAAI/bge-m3","cloud":"kubernetes","gpu":"NVIDIA-A10G"}`)
	if len(submitted.RuntimeArgs) != 0 || fit == nil || fit.SetBy != contextSetByRuntime || !strings.Contains(fit.Reason, "Embedding models") {
		t.Fatalf("embedding model: args=%v context=%+v", submitted.RuntimeArgs, fit)
	}
}

// Without metadata access a model outside the reviewed catalog, a gated
// reviewed model with no recorded sizes, or a reviewed model at another
// commit goes ahead as before, and the response says the context could not
// be sized and how to bound it.
func TestDeployWithoutMetadataAccessLeavesAnUnreviewedContextAndSaysWhy(t *testing.T) {
	for _, body := range []string{
		`{"name":"phi","model":"microsoft/phi-4","cloud":"kubernetes","gpu":"NVIDIA-A10G"}`,
		`{"name":"llama","model":"meta-llama/Llama-3.1-8B-Instruct","cloud":"kubernetes","gpu":"NVIDIA-A10G"}`,
		`{"name":"qwen","model":"Qwen/Qwen3-8B","model_revision":"` + strings.Repeat("a", 40) + `","cloud":"kubernetes","gpu":"NVIDIA-A10G"}`,
		`{"name":"qwen","model":"Qwen/Qwen3-8B","runtime":"sglang","model_revision":"` + strings.Repeat("a", 40) + `","cloud":"kubernetes","gpu":"NVIDIA-A10G"}`,
	} {
		_, submitted, fit := submitDeploy(t, nil, body)
		flag := "--max-model-len"
		if strings.Contains(body, "sglang") {
			flag = "--context-length"
		}
		if len(submitted.RuntimeArgs) != 0 || fit == nil || fit.SetBy != contextSetByRuntime || fit.Tokens != nil || !strings.Contains(fit.Reason, "metadata access is off") || !strings.Contains(fit.Reason, "set "+flag+" to bound it") {
			t.Fatalf("%s: args=%v context=%+v", body, submitted.RuntimeArgs, fit)
		}
	}
}

// A custom image chooses its own command, so InferCrane says nothing about
// its context.
func TestCustomImageDeployGetsNoContextLength(t *testing.T) {
	resolver, calls := recordedHub(t)
	body := `{"name":"custom","model":"Qwen/Qwen3-8B","runtime":"custom-oci","cloud":"kubernetes","gpu":"NVIDIA-A10G","workload":{"image":"registry.example/serve@sha256:` + strings.Repeat("a", 64) + `","command":["serve"],"protocol":"openai","port":8000,"readiness_path":"/health","models_path":"/v1/models","metrics_path":"/metrics","cancellation":"http-disconnect","drain":"connection","shutdown_grace_seconds":30}}`
	response, submitted, fit := submitDeploy(t, resolver, body)
	if fit != nil || len(submitted.RuntimeArgs) != 0 || strings.Contains(response.Body.String(), "context_length") || calls.Load() != 0 {
		t.Fatalf("args=%v context=%+v calls=%d", submitted.RuntimeArgs, fit, calls.Load())
	}
}
