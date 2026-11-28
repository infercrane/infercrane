package controlapi

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/infercrane/infercrane/internal/modelcheck"
	"github.com/infercrane/infercrane/internal/optimizationcapability"
	"github.com/infercrane/infercrane/internal/provideridentity"
	"github.com/infercrane/infercrane/internal/workflows"
)

// Who set the context length a new deployment's runtime serves.
const (
	contextSetByRequest    = "request"
	contextSetByInferCrane = "infercrane"
	contextSetByRuntime    = "runtime"
)

// contextLength says which context length a new deployment's runtime serves,
// and why. Tokens is the bound in its engine arguments, or nil when they set
// none and the runtime takes the model's own maximum. ModelTokens is that
// maximum, when the model's config.json was read.
type contextLength struct {
	Tokens      *int   `json:"tokens"`
	ModelTokens *int   `json:"model_tokens"`
	SetBy       string `json:"set_by"`
	Reason      string `json:"reason"`
}

// fitContextLength bounds the context of a plain deploy of a chat model when
// one request at the model's own maximum does not fit the chosen GPU next to
// the weights, which is when vLLM and SGLang refuse to start. It reuses the
// automatic model check's memory estimate and its default request length. A
// deploy that sets its own engine arguments, a custom image, or a serving
// graph keeps what it says. It never fails the deploy: when the model cannot
// be read, the context is left to the runtime and the reason says why.
func (a API) fitContextLength(ctx context.Context, tenant string, request *workflows.CloudRequest, builtInImage bool) *contextLength {
	if value, set := optimizationcapability.ContextLength(request.RuntimeArgs); set {
		result := &contextLength{SetBy: contextSetByRequest, Reason: "The deployment's engine arguments set the context length."}
		if tokens, err := strconv.Atoi(value); err == nil {
			result.Tokens = &tokens
		}
		return result
	}
	flag := optimizationcapability.ContextLengthFlag(request.Runtime)
	if flag == "" || !builtInImage || !request.Serving.Empty() {
		return nil
	}
	unbounded := func(modelTokens *int, reason string) *contextLength {
		return &contextLength{ModelTokens: modelTokens, SetBy: contextSetByRuntime, Reason: reason + " The runtime takes the model's own maximum context; set " + flag + " to bound it."}
	}
	switch {
	case len(request.RuntimeArgs) > 0:
		return unbounded(nil, "The deployment sets its own engine arguments, so InferCrane adds no context length to them.")
	case !modelcheck.ValidRepository(request.Model):
		return unbounded(nil, "InferCrane sizes the context from Hugging Face metadata, and this model is not a Hugging Face repository.")
	}
	memory, known := provideridentity.AcceleratorMemoryGiB(request.GPU)
	if !known {
		return unbounded(nil, "InferCrane does not know how much memory "+request.GPU+" has, so it cannot size the context.")
	}
	model, registries, failure := a.readModel(ctx, modelCheckRequest{Tenant: tenant, Repository: request.Model, Revision: request.ModelRevision, ModelSecretReferenceID: request.ModelSecretReferenceID})
	if failure != nil {
		if failure.Code == codeMetadataAccessDisabled {
			return unbounded(nil, "Hugging Face metadata access is off on this control plane, so InferCrane cannot read the model's size.")
		}
		return unbounded(nil, "InferCrane could not read the model to size the context: "+failure.Message)
	}
	// One request on one GPU: the plain deploy sets no tensor parallelism,
	// and the runtimes refuse to start when a single request at the context
	// length cannot fit the KV cache left beside the weights.
	now := time.Now().UTC()
	gpus := []modelcheck.GPU{{Name: request.GPU, MemoryGiB: memory}}
	fit := func(contextTokens int) modelcheck.Report {
		return modelcheck.Evaluate(model, modelcheck.EvaluateOptions{ContextTokens: contextTokens, Concurrency: 1, GPUs: gpus, Registries: registries, Now: now})
	}
	fits := func(report modelcheck.Report) bool {
		return report.Memory.State == modelcheck.Fits && report.Memory.GPUCount != nil && *report.Memory.GPUCount == 1
	}
	full := fit(modelcheck.MaxContextTokens)
	switch {
	case full.Task == modelcheck.TaskEmbeddings:
		return unbounded(nil, "Embedding models keep no KV cache between requests.")
	case full.Task != modelcheck.TaskChat:
		return unbounded(nil, "InferCrane could not tell whether this model generates text, so it cannot size the context.")
	case full.Memory.State == modelcheck.Unknown:
		return unbounded(nil, "InferCrane could not size the context: "+full.Memory.Reason)
	case full.Config.ContextLength == nil:
		return unbounded(nil, "The model's config.json declares no context length.")
	}
	modelTokens := *full.Config.ContextLength
	if fits(full) {
		return &contextLength{ModelTokens: &modelTokens, SetBy: contextSetByRuntime, Reason: fmt.Sprintf("By InferCrane's estimate, one request at the model's own maximum of %d tokens fits one %s next to the weights, so the runtime keeps it.", modelTokens, request.GPU)}
	}
	bounded := fit(0)
	if !fits(bounded) {
		return &contextLength{ModelTokens: &modelTokens, SetBy: contextSetByRuntime, Reason: fmt.Sprintf("By InferCrane's estimate, even one request of %d tokens does not fit one %s next to the weights, so the runtime may not start. Choose a GPU with more memory.", bounded.Memory.ContextTokens, request.GPU)}
	}
	tokens := bounded.Memory.ContextTokens
	request.RuntimeArgs = append(request.RuntimeArgs, flag, strconv.Itoa(tokens))
	return &contextLength{Tokens: &tokens, ModelTokens: &modelTokens, SetBy: contextSetByInferCrane, Reason: fmt.Sprintf("By InferCrane's estimate, one request at the model's own maximum of %d tokens does not fit one %s next to the weights, and the runtime would not start. InferCrane set %s %d, the request length its model check sizes GPUs for. Set your own context length to change it.", modelTokens, request.GPU, flag, tokens)}
}
