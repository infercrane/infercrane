package provision

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/infercrane/infercrane/internal/provideridentity"
)

// The supported RunPod worker-v1-vllm image requires a CUDA 13 capable host.
// Constraining placement prevents RunPod from repeatedly starting the worker on
// hosts whose driver cannot load the image. CUDA's driver compatibility keeps
// this safe for templates built with an older CUDA userspace as well.
const runPodServerlessMinCUDAVersion = "13.0"

// RunPod's maintained vLLM worker is a queue-based worker: its OpenAI route
// is served through the queue at api.runpod.ai/v2/{id}/openai.
const runPodServerlessEndpointType = "QUEUE"

type ServerlessEndpointSpec struct {
	ExternalKey, Model, ModelRevision, TemplateID, GPU, Region string
	WorkersMax                                                 int
}

type ServerlessEndpoint struct {
	ID, Name string
	// TemplateID is the template EnsureEndpoint created the endpoint from, or
	// validated an adopted endpoint's container against. RunPod REST v2 copies
	// a template into the endpoint at create time and does not return it, so
	// ListEndpoints leaves this empty.
	TemplateID string
	// GPUPools and ExcludedGPUTypes are RunPod v2's GPU selection: workers may
	// run on every GPU type in the pools except the excluded ones.
	GPUPools, ExcludedGPUTypes []string
	WorkersMin                 int
	WorkersMax                 int
	// Workers is the active worker count when it was observed, and nil when
	// the call that produced this value did not observe workers.
	Workers *int
}

type ServerlessHealth struct {
	WorkersIdle, WorkersRunning                          int
	JobsCompleted, JobsFailed, JobsInProgress, JobsQueue int
}

type RunPodServerless struct {
	APIKey, BaseURL, InferenceBaseURL, TemplateID string
	Client                                        *http.Client
}

// runPodTemplate is the subset of RunPod REST v2's Template schema that
// InferCrane validates (GET /v2/templates/{id}).
type runPodTemplate struct {
	ID         string            `json:"id"`
	Image      string            `json:"image"`
	Serverless bool              `json:"serverless"`
	Env        map[string]string `json:"env"`
}

// runPodEndpointRecord is the subset of RunPod REST v2's Endpoint schema that
// InferCrane reads (GET /v2/serverless).
type runPodEndpointRecord struct {
	ID    string            `json:"id"`
	Name  string            `json:"name"`
	Type  string            `json:"type"`
	Image string            `json:"image"`
	Env   map[string]string `json:"env"`
	GPU   *struct {
		Pools          []string `json:"pools"`
		ExcludedTypes  []string `json:"excludedTypes"`
		Count          int      `json:"count"`
		MinCudaVersion *string  `json:"minCudaVersion"`
	} `json:"gpu"`
	Workers struct {
		Min int `json:"min"`
		Max int `json:"max"`
	} `json:"workers"`
	DataCenterIDs []string `json:"dataCenterIds"`
}

func (record runPodEndpointRecord) endpoint() ServerlessEndpoint {
	endpoint := ServerlessEndpoint{ID: record.ID, Name: record.Name, WorkersMin: record.Workers.Min, WorkersMax: record.Workers.Max}
	if record.GPU != nil {
		endpoint.GPUPools, endpoint.ExcludedGPUTypes = record.GPU.Pools, record.GPU.ExcludedTypes
	}
	return endpoint
}

// runPodGPUSelection pins one exact GPU type on RunPod v2 Serverless. v2
// selects workers by pool and has no inclusive type allowlist, so the pool is
// narrowed to the requested type by excluding every other type in it.
type runPodGPUSelection struct {
	TypeID, Pool string
	Excluded     []string
}

type runPodServerlessIntent struct {
	name, dataCenterID string
	template           runPodTemplate
	gpu                runPodGPUSelection
	workersMax         int
}

// mismatch reports why an endpoint RunPod returned does not carry this exact
// intent, or "" when it does. Fields RunPod omitted are not guessed: only the
// identity, worker bounds, and GPU selection are required to be present.
func (intent runPodServerlessIntent) mismatch(record runPodEndpointRecord) string {
	switch {
	case record.Name != intent.name:
		return "name"
	case record.Type != "" && record.Type != runPodServerlessEndpointType:
		return "endpoint type"
	case record.Workers.Min != 0 || record.Workers.Max != intent.workersMax:
		return "worker bounds"
	case record.GPU == nil || !equalStrings(record.GPU.Pools, []string{intent.gpu.Pool}):
		return "GPU pool"
	case record.GPU.Count > 1:
		// count is optional in v2 responses (default 1); only a stated
		// count other than one is drift.
		return "GPU count"
	case contains(record.GPU.ExcludedTypes, intent.gpu.TypeID):
		return "GPU type exclusion"
	case record.GPU.MinCudaVersion != nil && *record.GPU.MinCudaVersion != runPodServerlessMinCUDAVersion:
		return "CUDA floor"
	case record.Image != "" && record.Image != intent.template.Image:
		return "image"
	case len(record.Env) > 0 && (record.Env["MODEL_NAME"] != intent.template.Env["MODEL_NAME"] || record.Env["MODEL_REVISION"] != intent.template.Env["MODEL_REVISION"]):
		return "model"
	case intent.dataCenterID != "" && len(record.DataCenterIDs) > 0 && !equalStrings(record.DataCenterIDs, []string{intent.dataCenterID}):
		return "data center"
	}
	// A GPU type RunPod added to the pool after create would widen placement;
	// treat that as drift instead of silently adopting it.
	for _, excluded := range intent.gpu.Excluded {
		if !contains(record.GPU.ExcludedTypes, excluded) {
			return "GPU type exclusion"
		}
	}
	return ""
}

func (r RunPodServerless) EndpointURL(endpointID string) string {
	base := strings.TrimRight(r.InferenceBaseURL, "/")
	if base == "" {
		base = "https://api.runpod.ai/v2"
	}
	return base + "/" + url.PathEscape(endpointID) + "/openai"
}

func (r RunPodServerless) EndpointHealth(ctx context.Context, endpointID string) (ServerlessHealth, error) {
	if endpointID == "" {
		return ServerlessHealth{}, errors.New("RunPod Serverless endpoint ID is required")
	}
	base := strings.TrimRight(r.InferenceBaseURL, "/")
	if base == "" {
		base = "https://api.runpod.ai/v2"
	}
	var response struct {
		Workers struct {
			Idle, Running int
		} `json:"workers"`
		Jobs struct {
			Completed, Failed, InProgress, InQueue int
		} `json:"jobs"`
	}
	if err := r.doAbsolute(ctx, http.MethodGet, base+"/"+url.PathEscape(endpointID)+"/health", nil, &response); err != nil {
		return ServerlessHealth{}, err
	}
	return ServerlessHealth{WorkersIdle: response.Workers.Idle, WorkersRunning: response.Workers.Running, JobsCompleted: response.Jobs.Completed, JobsFailed: response.Jobs.Failed, JobsInProgress: response.Jobs.InProgress, JobsQueue: response.Jobs.InQueue}, nil
}

func (r RunPodServerless) EnsureEndpoint(ctx context.Context, spec ServerlessEndpointSpec) (ServerlessEndpoint, error) {
	if spec.ExternalKey == "" || spec.Model == "" || spec.ModelRevision == "" || spec.GPU == "" || spec.WorkersMax < 1 {
		return ServerlessEndpoint{}, errors.New("external key, model, immutable model revision, GPU, and positive workers max are required")
	}
	templateID := spec.TemplateID
	if templateID == "" {
		templateID = r.TemplateID
	}
	if templateID == "" {
		return ServerlessEndpoint{}, errors.New("RunPod Serverless vLLM template ID is required")
	}
	template, err := r.validateTemplate(ctx, templateID, spec.Model, spec.ModelRevision)
	if err != nil {
		return ServerlessEndpoint{}, err
	}
	gpu, err := r.gpuSelection(ctx, spec.GPU)
	if err != nil {
		return ServerlessEndpoint{}, err
	}
	intent := runPodServerlessIntent{name: serverlessName(spec.ExternalKey), dataCenterID: runPodDataCenterConstraint(spec.Region), template: template, gpu: gpu, workersMax: spec.WorkersMax}
	records, err := r.listEndpoints(ctx)
	if err != nil {
		return ServerlessEndpoint{}, err
	}
	var existing []runPodEndpointRecord
	for _, record := range records {
		if record.Name == intent.name {
			existing = append(existing, record)
		}
	}
	if len(existing) > 1 {
		return ServerlessEndpoint{}, fmt.Errorf("multiple RunPod Serverless endpoints match durable key %q", spec.ExternalKey)
	}
	if len(existing) == 1 {
		if reason := intent.mismatch(existing[0]); reason != "" {
			return ServerlessEndpoint{}, fmt.Errorf("existing RunPod Serverless endpoint %s does not match immutable intent (%s)", existing[0].ID, reason)
		}
		endpoint := existing[0].endpoint()
		endpoint.TemplateID = templateID
		return endpoint, nil
	}
	gpuConfig := map[string]any{"pools": []string{gpu.Pool}, "count": 1, "minCudaVersion": runPodServerlessMinCUDAVersion}
	if len(gpu.Excluded) > 0 {
		gpuConfig["excludedTypes"] = gpu.Excluded
	}
	// RunPod REST v2 CreateEndpointRequest. v2 rejects unknown body fields
	// with 422, so only schema fields are sent.
	body := map[string]any{
		"name": intent.name, "templateId": templateID, "type": runPodServerlessEndpointType,
		"gpu":       gpuConfig,
		"workers":   map[string]any{"min": 0, "max": spec.WorkersMax, "idleTimeout": 5},
		"scaling":   map[string]any{"type": "QUEUE_DELAY", "queueDelay": 4},
		"flashboot": "FLASHBOOT",
	}
	if intent.dataCenterID != "" {
		body["dataCenterIds"] = []string{intent.dataCenterID}
	}
	var created runPodEndpointRecord
	if err = r.do(ctx, http.MethodPost, "/serverless", body, &created); err != nil {
		return ServerlessEndpoint{}, err
	}
	if created.ID == "" || created.Name != intent.name {
		return ServerlessEndpoint{}, errors.New("RunPod Serverless create returned an invalid endpoint identity")
	}
	if reason := intent.mismatch(created); reason != "" {
		return ServerlessEndpoint{}, fmt.Errorf("RunPod Serverless create returned endpoint configuration that does not match immutable intent (%s)", reason)
	}
	endpoint := created.endpoint()
	endpoint.TemplateID = templateID
	return endpoint, nil
}

func (r RunPodServerless) ListEndpoints(ctx context.Context) ([]ServerlessEndpoint, error) {
	records, err := r.listEndpoints(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]ServerlessEndpoint, len(records))
	for i, record := range records {
		out[i] = record.endpoint()
	}
	return out, nil
}

func (r RunPodServerless) listEndpoints(ctx context.Context) ([]runPodEndpointRecord, error) {
	return runPodListAll[runPodEndpointRecord](ctx, r.do, "/serverless", "endpoints")
}

// ActiveWorkers returns the number of workers RunPod currently allocates to
// the endpoint, in every worker state. Scaled-down workers are not counted.
func (r RunPodServerless) ActiveWorkers(ctx context.Context, endpointID string) (int, error) {
	if endpointID == "" {
		return 0, errors.New("RunPod Serverless endpoint ID is required")
	}
	var response struct {
		Summary *struct {
			Total *int `json:"total"`
		} `json:"summary"`
	}
	err := r.do(ctx, http.MethodGet, "/serverless/"+url.PathEscape(endpointID)+"/workers", nil, &response)
	var responseErr *providerHTTPError
	if errors.As(err, &responseErr) && responseErr.Status == http.StatusNotFound {
		return 0, fmt.Errorf("RunPod Serverless endpoint %s not found", endpointID)
	}
	if err != nil {
		return 0, err
	}
	if response.Summary == nil || response.Summary.Total == nil {
		return 0, fmt.Errorf("RunPod Serverless endpoint %s worker summary is unavailable", endpointID)
	}
	return *response.Summary.Total, nil
}

func (r RunPodServerless) DeleteEndpoint(ctx context.Context, endpointID string) error {
	if endpointID == "" {
		return errors.New("RunPod Serverless endpoint ID is required")
	}
	err := r.do(ctx, http.MethodDelete, "/serverless/"+url.PathEscape(endpointID), nil, nil)
	var responseErr *providerHTTPError
	if errors.As(err, &responseErr) && responseErr.Status == http.StatusNotFound {
		return nil
	}
	return err
}

func (r RunPodServerless) Check(ctx context.Context) error {
	if r.TemplateID == "" {
		return errors.New("INFERCRANE_RUNPOD_SERVERLESS_TEMPLATE_ID is required")
	}
	template, err := r.template(ctx, r.TemplateID)
	if err != nil {
		return err
	}
	if template.Env["MODEL_NAME"] == "" {
		return errors.New("RunPod template must set MODEL_NAME")
	}
	revision := template.Env["MODEL_REVISION"]
	if revision == "" || revision == "main" || revision == "master" {
		return errors.New("RunPod template must pin MODEL_REVISION to an immutable revision")
	}
	if template.Env["RAW_OPENAI_OUTPUT"] != "1" {
		return errors.New("RunPod template must set RAW_OPENAI_OUTPUT=1")
	}
	if !strings.EqualFold(template.Env["ENABLE_AUTO_TOOL_CHOICE"], "true") || template.Env["TOOL_CALL_PARSER"] == "" {
		return errors.New("RunPod template must enable automatic tool choice and set TOOL_CALL_PARSER")
	}
	return nil
}

func (r RunPodServerless) template(ctx context.Context, templateID string) (runPodTemplate, error) {
	var template runPodTemplate
	if err := r.do(ctx, http.MethodGet, "/templates/"+url.PathEscape(templateID), nil, &template); err != nil {
		return runPodTemplate{}, err
	}
	if template.ID != templateID || !template.Serverless {
		return runPodTemplate{}, errors.New("configured RunPod template is not a Serverless template")
	}
	return template, nil
}

func (r RunPodServerless) validateTemplate(ctx context.Context, templateID, model, revision string) (runPodTemplate, error) {
	template, err := r.template(ctx, templateID)
	if err != nil {
		return runPodTemplate{}, fmt.Errorf("validate RunPod Serverless template: %w", err)
	}
	if template.Env["MODEL_NAME"] != model {
		return runPodTemplate{}, fmt.Errorf("RunPod template MODEL_NAME is %q, expected %q", template.Env["MODEL_NAME"], model)
	}
	if template.Env["MODEL_REVISION"] != revision {
		return runPodTemplate{}, fmt.Errorf("RunPod template MODEL_REVISION is %q, expected immutable revision %q", template.Env["MODEL_REVISION"], revision)
	}
	if template.Env["RAW_OPENAI_OUTPUT"] != "1" {
		return runPodTemplate{}, errors.New("RunPod template must set RAW_OPENAI_OUTPUT=1 for OpenAI-compatible streaming")
	}
	if !strings.EqualFold(template.Env["ENABLE_AUTO_TOOL_CHOICE"], "true") || template.Env["TOOL_CALL_PARSER"] == "" {
		return runPodTemplate{}, errors.New("RunPod template must set ENABLE_AUTO_TOOL_CHOICE=true and TOOL_CALL_PARSER for OpenAI-compatible tool calls")
	}
	return template, nil
}

// gpuSelection resolves the requested GPU type to its RunPod Serverless pool
// from the v2 GPU catalog and excludes every other type in that pool.
func (r RunPodServerless) gpuSelection(ctx context.Context, gpu string) (runPodGPUSelection, error) {
	typeID := RunPodGPUTypeID(gpu)
	var catalog struct {
		GPUs []struct {
			ID   string  `json:"id"`
			Pool *string `json:"pool"`
		} `json:"gpus"`
	}
	if err := r.do(ctx, http.MethodGet, "/catalog/gpus", nil, &catalog); err != nil {
		return runPodGPUSelection{}, fmt.Errorf("read RunPod GPU catalog: %w", err)
	}
	selection := runPodGPUSelection{TypeID: typeID}
	for _, entry := range catalog.GPUs {
		if entry.ID == typeID && entry.Pool != nil {
			selection.Pool = strings.TrimSpace(*entry.Pool)
		}
	}
	if selection.Pool == "" {
		return runPodGPUSelection{}, fmt.Errorf("RunPod GPU type %q is not offered in a Serverless pool", typeID)
	}
	for _, entry := range catalog.GPUs {
		if entry.ID != typeID && entry.Pool != nil && strings.TrimSpace(*entry.Pool) == selection.Pool {
			selection.Excluded = append(selection.Excluded, entry.ID)
		}
	}
	sort.Strings(selection.Excluded)
	return selection, nil
}

type providerHTTPError struct {
	Status int
	Body   string
}

func (e *providerHTTPError) Error() string {
	return fmt.Sprintf("RunPod API request failed (HTTP %d)", e.Status)
}

func (r RunPodServerless) do(ctx context.Context, method, path string, body, output any) error {
	if r.APIKey == "" {
		return errors.New("RUNPOD_API_KEY is required")
	}
	return r.doAbsolute(ctx, method, runPodBaseURL(r.BaseURL)+path, body, output)
}

func (r RunPodServerless) doAbsolute(ctx context.Context, method, endpoint string, body, output any) error {
	if r.APIKey == "" {
		return errors.New("RUNPOD_API_KEY is required")
	}
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+r.APIKey)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	client := r.Client
	if client == nil {
		client = http.DefaultClient
	}
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("RunPod API request: %w", err)
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(response.Body, 2<<20))
	if err != nil {
		return fmt.Errorf("read RunPod API response: %w", err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		message := safeRunPodDiagnostic(runPodProblemText(payload), r.APIKey)
		return &providerHTTPError{Status: response.StatusCode, Body: message}
	}
	if output != nil && len(payload) > 0 {
		if err = json.Unmarshal(payload, output); err != nil {
			return fmt.Errorf("decode RunPod API response: %w", err)
		}
	}
	return nil
}

// ServerlessEndpointName derives the provider-visible name from a persisted
// replica external key. Keeping this deterministic lets cleanup recover an
// endpoint even when RunPod accepted create but its response was lost.
func ServerlessEndpointName(externalKey string) string {
	return clusterName(externalKey) + "-serverless"
}

func serverlessName(externalKey string) string { return ServerlessEndpointName(externalKey) }

// RunPodGPUTypeID converts InferCrane's small, explicit aliases to RunPod's
// provider resource IDs. Unknown values remain unchanged instead of being
// guessed, so catalog and launch matching stay exact.
func RunPodGPUTypeID(gpu string) string {
	return provideridentity.GPUTypeID("runpod", gpu)
}

func contains(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}
