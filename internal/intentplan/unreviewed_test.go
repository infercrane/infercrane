package intentplan

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/infercrane/infercrane/internal/modelcheck"
	"github.com/infercrane/infercrane/internal/support"
)

const phiCommit = "2db69c1c3e91a05d2c64a3185acfbaf36f744e25"

func checkedModel(repository string, parameters int64, config *modelcheck.Config) modelcheck.Model {
	return modelcheck.Model{
		Repository: repository, RequestedRevision: "main", Commit: phiCommit, PipelineTag: "text-generation", License: "mit",
		Architectures: []string{"Phi3ForCausalLM"}, ModelType: "phi3", Files: modelcheck.Files{Listed: true, Config: true, Safetensors: true},
		Weights: &modelcheck.Weights{Parameters: parameters, ByDType: map[string]int64{"BF16": parameters}}, Config: config, ConfigState: modelcheck.ConfigRead,
	}
}

func phiConfig() *modelcheck.Config {
	return &modelcheck.Config{Architectures: []string{"Phi3ForCausalLM"}, ModelType: "phi3", Layers: 40, HiddenSize: 5120, AttentionHeads: 40, KVHeads: 10, ContextLength: 16384, DType: "bfloat16"}
}

func unreviewedPlanner(t *testing.T, model modelcheck.Model, options modelcheck.EvaluateOptions) Planner {
	t.Helper()
	planner := testPlanner(t)
	registries, err := modelcheck.Registries()
	if err != nil {
		t.Fatal(err)
	}
	options.Registries = registries
	if options.GPUs == nil {
		options.GPUs = modelcheck.PricedGPUs(planner.Prices, planner.Now())
	}
	report := modelcheck.Evaluate(model, options)
	planner.Unreviewed = &UnreviewedModel{Repository: model.Repository, Report: &report}
	return planner
}

func TestPlanDeploysAnAutomaticallyCheckedModel(t *testing.T) {
	planner := unreviewedPlanner(t, checkedModel("microsoft/phi-4", 14659507200, phiConfig()), modelcheck.EvaluateOptions{})
	plan, err := planner.Plan(Request{Intent: "chat workload", Model: "microsoft/phi-4", Provider: "runpod"})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status != "ready" || plan.Model == nil || plan.Model.Review != ReviewCheckedAutomatically || plan.Model.Revision != phiCommit || plan.ModelCheck == nil || plan.ModelCheck.Verdict != modelcheck.Supported {
		t.Fatalf("unexpected plan: status=%s model=%+v check=%+v missing=%+v", plan.Status, plan.Model, plan.ModelCheck, plan.Missing)
	}
	if plan.Warnings[0] != checkedWarning || !strings.HasPrefix(plan.Evidence.Configuration, "checked automatically: Hugging Face metadata at 2db69c1c3e91") || plan.Evidence.Performance != "unmeasured" {
		t.Fatalf("unexpected evidence: %+v warnings=%v", plan.Evidence, plan.Warnings)
	}
	draft := plan.DeploymentDraft
	if draft == nil || draft.Name != "phi-4" || draft.Model != "microsoft/phi-4" || draft.ModelRevision != phiCommit || draft.Runtime != "vllm" || draft.RuntimeVersion != support.DefaultRuntimeVersion || draft.GPU != "L40S" || draft.GPUCount != 1 || draft.Cloud != "runpod" {
		t.Fatalf("unexpected draft: %+v", draft)
	}
	if !reflect.DeepEqual(draft.RuntimeArgs, []string{"--max-model-len", "8192"}) || draft.MinReplicas != 1 || draft.MaxReplicas != 1 {
		t.Fatalf("unexpected runtime defaults: %+v", draft)
	}
	if err = draft.Validate(); err != nil {
		t.Fatalf("draft does not validate: %v", err)
	}
	for _, node := range plan.Architecture.Nodes {
		if node.State == "reviewed" {
			t.Fatalf("an automatically checked plan calls %s reviewed", node.ID)
		}
	}
	encoded, err := json.Marshal(NewEnvelope(plan))
	if err != nil || !strings.Contains(string(encoded), `"model_check":{"schema_version":"infercrane.model-check/v1"`) || !strings.Contains(string(encoded), `"review":"checked_automatically"`) {
		t.Fatalf("envelope does not carry the check: %s %v", encoded, err)
	}
}

func TestPlanSplitsALargeCheckedModelAcrossGPUs(t *testing.T) {
	config := &modelcheck.Config{Layers: 48, AttentionHeads: 40, KVHeads: 8, HeadDim: 128, ContextLength: 32768}
	planner := unreviewedPlanner(t, checkedModel("acme/big-30b", 30_000_000_000, config), modelcheck.EvaluateOptions{})
	plan, err := planner.Plan(Request{Model: "acme/big-30b", Provider: "runpod"})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Configuration == nil || plan.Configuration.GPUCount != 2 || !slices.Equal(plan.Configuration.RuntimeArgs, []string{"--max-model-len", "8192", "--tensor-parallel-size", "2"}) {
		t.Fatalf("unexpected configuration: %+v missing=%+v", plan.Configuration, plan.Missing)
	}
	// A GPU the model does not fit is refused with the ones it does.
	plan, err = planner.Plan(Request{Model: "acme/big-30b", Provider: "runpod", GPU: "L4"})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status == "ready" || !hasMissing(plan, "gpu") {
		t.Fatalf("an unfit GPU was accepted: %+v", plan.Missing)
	}
}

func TestPlanRefusesUnsupportedModelWithNearestReviewed(t *testing.T) {
	model := checkedModel("RWKV/rwkv-4-169m-pile", 169_000_000, &modelcheck.Config{Architectures: []string{"RwkvForCausalLM"}, Layers: 12, HiddenSize: 768})
	model.Architectures = []string{"RwkvForCausalLM"}
	plan, err := unreviewedPlanner(t, model, modelcheck.EvaluateOptions{}).Plan(Request{Model: "RWKV/rwkv-4-169m-pile"})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status != "unsupported" || plan.DeploymentDraft != nil || plan.ModelCheck == nil || len(plan.Missing) != 1 || plan.Missing[0].Field != "model" {
		t.Fatalf("unexpected plan: %+v", plan)
	}
	if !strings.Contains(plan.Missing[0].Reason, "RwkvForCausalLM is not in the model registry") || len(plan.Missing[0].Options) != 3 || plan.Missing[0].Options[0].State != "reviewed" {
		t.Fatalf("unexpected refusal: %+v", plan.Missing[0])
	}
	for _, option := range plan.Missing[0].Options {
		if option.Value == "bge-m3-embeddings" {
			t.Fatalf("a chat model was offered an embedding model first: %+v", plan.Missing[0].Options)
		}
	}
}

func TestPlanAsksForATokenForGatedModels(t *testing.T) {
	model := checkedModel("meta-llama/Llama-3.2-1B-Instruct", 1_235_814_400, nil)
	model.Architectures, model.Gated, model.ConfigState = []string{"LlamaForCausalLM"}, "manual", modelcheck.ConfigTokenRequired
	plan, err := unreviewedPlanner(t, model, modelcheck.EvaluateOptions{}).Plan(Request{Model: "meta-llama/Llama-3.2-1B-Instruct"})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status != "needs_input" || !hasMissing(plan, "model_secret_reference_id") || !hasMissing(plan, "gpu") || !strings.Contains(missing(plan, "model_secret_reference_id").Remediation, "Settings › Connections") {
		t.Fatalf("unexpected gated plan: %+v", plan.Missing)
	}
	model.Config = &modelcheck.Config{Layers: 16, AttentionHeads: 32, KVHeads: 8, HeadDim: 64, ContextLength: 131072}
	model.ConfigState, model.TokenUsed = modelcheck.ConfigRead, true
	plan, err = unreviewedPlanner(t, model, modelcheck.EvaluateOptions{}).Plan(Request{Model: "meta-llama/Llama-3.2-1B-Instruct", ModelSecretReferenceID: "secret-hf", Provider: "runpod"})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status != "ready" || plan.DeploymentDraft == nil || plan.DeploymentDraft.ModelSecretReferenceID != "secret-hf" || plan.ModelCheck.Access.State != modelcheck.AccessTokenUsed {
		t.Fatalf("a token read did not reach the draft: %+v %+v", plan.DeploymentDraft, plan.Missing)
	}
}

func TestPlanExplainsAnUncheckableModel(t *testing.T) {
	planner := testPlanner(t)
	planner.Unreviewed = &UnreviewedModel{Repository: "acme/missing", Code: modelcheck.CodeModelNotFound, Reason: "Hugging Face did not find acme/missing, or it is private."}
	plan, err := planner.Plan(Request{Model: "acme/missing"})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status != "needs_input" || plan.Model != nil || plan.ModelCheck != nil || missing(plan, "model").Reason != planner.Unreviewed.Reason || plan.Warnings[0] != uncheckedWarning {
		t.Fatalf("unexpected plan: %+v", plan)
	}
	// A check for another repository never answers this request.
	plan, err = planner.Plan(Request{Model: "acme/other"})
	if err != nil || missing(plan, "model").Reason == planner.Unreviewed.Reason {
		t.Fatalf("a check for another repository was used: %+v %v", plan.Missing, err)
	}
	// Serverless is not offered for automatically checked models.
	checked := unreviewedPlanner(t, checkedModel("microsoft/phi-4", 14659507200, phiConfig()), modelcheck.EvaluateOptions{})
	plan, err = checked.Plan(Request{Model: "microsoft/phi-4", ComputeMode: "serverless", Provider: "runpod"})
	if err != nil || plan.Status == "ready" || !hasMissing(plan, "compute_mode") {
		t.Fatalf("serverless was accepted: %+v %v", plan.Missing, err)
	}
}

func TestUnreviewedRequestHelpers(t *testing.T) {
	for _, test := range []struct{ model, revision, repository, wantRevision string }{
		{"microsoft/phi-4", "", "microsoft/phi-4", "main"},
		{"https://huggingface.co/microsoft/phi-4", "", "microsoft/phi-4", "main"},
		{"microsoft/phi-4@v1.0", "", "microsoft/phi-4", "v1.0"},
		{"microsoft/phi-4@v1.0", phiCommit, "microsoft/phi-4", phiCommit},
		{"phi-4", "", "", "main"},
		{"deploy microsoft/phi-4 please", "", "", "main"},
	} {
		request := Request{Model: test.model, ModelRevision: test.revision}
		if got := UnreviewedRepository(request); got != test.repository {
			t.Fatalf("UnreviewedRepository(%q) = %q", test.model, got)
		}
		if got := UnreviewedRevision(request); got != test.wantRevision {
			t.Fatalf("UnreviewedRevision(%q, %q) = %q", test.model, test.revision, got)
		}
	}
	if !ResolvesReviewed(nil, Request{Model: "Qwen/Qwen3-8B"}) || ResolvesReviewed(nil, Request{Model: "microsoft/phi-4"}) {
		t.Fatal("ResolvesReviewed disagrees with the reviewed catalog")
	}
	for input, want := range map[string]string{"microsoft/Phi-4": "phi-4", "acme/Model_V2.5": "model_v2.5", "acme/" + strings.Repeat("x", 80): strings.Repeat("x", 64)} {
		if got := deploymentName(input); got != want {
			t.Fatalf("deploymentName(%q) = %q", input, got)
		}
	}
	if _, err := testPlanner(t).Plan(Request{Model: "microsoft/phi-4", ContextTokens: 10}); err == nil {
		t.Fatal("an out-of-range context was accepted")
	}
	if _, err := testPlanner(t).Plan(Request{Model: "microsoft/phi-4", ModelRevision: "../main"}); err == nil {
		t.Fatal("an invalid revision was accepted")
	}
}

func missing(plan Plan, field string) MissingChoice {
	for _, item := range plan.Missing {
		if item.Field == field {
			return item
		}
	}
	return MissingChoice{}
}

// The checked profile bounds the context with each runtime's own flag.
func TestCheckedProfileUsesEachRuntimesContextFlag(t *testing.T) {
	report := modelcheck.Report{Memory: modelcheck.MemoryFit{ContextTokens: 8192, Concurrency: 8}}
	for runtime, want := range map[string][]string{
		"vllm":   {"--max-model-len", "8192", "--tensor-parallel-size", "2"},
		"sglang": {"--context-length", "8192", "--tp-size", "2"},
	} {
		if profile := checkedProfile(report, runtime, "1", "L4", 2, nil); !slices.Equal(profile.RuntimeArgs, want) {
			t.Fatalf("%s: runtime args %v, want %v", runtime, profile.RuntimeArgs, want)
		}
	}
}
