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
	"strings"

	"github.com/infercrane/infercrane/internal/provideridentity"
)

const defaultRunPodRESTURL = "https://rest.runpod.io/v1"

// The supported RunPod worker-v1-vllm image requires a CUDA 13 capable host.
// Constraining placement prevents RunPod from repeatedly starting the worker on
// hosts whose driver cannot load the image. CUDA's driver compatibility keeps
// this safe for templates built with an older CUDA userspace as well.
const runPodServerlessMinCUDAVersion = "13.0"

type ServerlessEndpointSpec struct {
	ExternalKey, Model, ModelRevision, TemplateID, GPU, Region string
	WorkersMax                                                 int
}

type ServerlessEndpoint struct {
	ID, Name, TemplateID string
	GPUTypeIDs           []string
	WorkersMin           int
	WorkersMax           int
	Workers              int
}

type ServerlessHealth struct {
	WorkersIdle, WorkersRunning                          int
	JobsCompleted, JobsFailed, JobsInProgress, JobsQueue int
}

type RunPodServerless struct {
	APIKey, BaseURL, InferenceBaseURL, TemplateID string
	Client                                        *http.Client
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
	if err := r.validateTemplate(ctx, templateID, spec.Model, spec.ModelRevision); err != nil {
		return ServerlessEndpoint{}, err
	}
	name := serverlessName(spec.ExternalKey)
	endpoints, err := r.ListEndpoints(ctx)
	if err != nil {
		return ServerlessEndpoint{}, err
	}
	var existing []ServerlessEndpoint
	for _, endpoint := range endpoints {
		if endpoint.Name == name {
			existing = append(existing, endpoint)
		}
	}
	if len(existing) > 1 {
		return ServerlessEndpoint{}, fmt.Errorf("multiple RunPod Serverless endpoints match durable key %q", spec.ExternalKey)
	}
	if len(existing) == 1 {
		endpoint := existing[0]
		if endpoint.TemplateID != templateID || endpoint.WorkersMin != 0 || endpoint.WorkersMax != spec.WorkersMax || !contains(endpoint.GPUTypeIDs, RunPodGPUTypeID(spec.GPU)) {
			return ServerlessEndpoint{}, fmt.Errorf("existing RunPod Serverless endpoint %s does not match immutable intent", endpoint.ID)
		}
		return endpoint, nil
	}
	body := map[string]any{
		"name": name, "templateId": templateID, "computeType": "GPU",
		"gpuTypeIds": []string{RunPodGPUTypeID(spec.GPU)}, "gpuCount": 1,
		"minCudaVersion": runPodServerlessMinCUDAVersion,
		"workersMin":     0, "workersMax": spec.WorkersMax,
		"idleTimeout": 5, "scalerType": "QUEUE_DELAY", "scalerValue": 4,
		"flashboot": true,
	}
	if spec.Region != "" {
		body["dataCenterIds"] = []string{spec.Region}
	}
	var endpoint ServerlessEndpoint
	if err = r.do(ctx, http.MethodPost, "/endpoints", body, &endpoint); err != nil {
		return ServerlessEndpoint{}, err
	}
	if endpoint.ID == "" || endpoint.Name != name || endpoint.TemplateID != templateID {
		return ServerlessEndpoint{}, errors.New("RunPod Serverless create returned an invalid endpoint identity")
	}
	if endpoint.WorkersMin != 0 || endpoint.WorkersMax != spec.WorkersMax || !contains(endpoint.GPUTypeIDs, RunPodGPUTypeID(spec.GPU)) {
		return ServerlessEndpoint{}, errors.New("RunPod Serverless create returned endpoint configuration that does not match immutable intent")
	}
	return endpoint, nil
}

func (r RunPodServerless) ListEndpoints(ctx context.Context) ([]ServerlessEndpoint, error) {
	type worker struct {
		DesiredStatus string `json:"desiredStatus"`
	}
	var endpoints []struct {
		ID, Name, TemplateID string
		GPUTypeIDs           []string `json:"gpuTypeIds"`
		WorkersMin           int      `json:"workersMin"`
		WorkersMax           int      `json:"workersMax"`
		Workers              []worker `json:"workers"`
	}
	if err := r.do(ctx, http.MethodGet, "/endpoints?includeWorkers=true", nil, &endpoints); err != nil {
		return nil, err
	}
	out := make([]ServerlessEndpoint, len(endpoints))
	for i, endpoint := range endpoints {
		activeWorkers := 0
		for _, worker := range endpoint.Workers {
			if !strings.EqualFold(worker.DesiredStatus, "EXITED") {
				activeWorkers++
			}
		}
		out[i] = ServerlessEndpoint{ID: endpoint.ID, Name: endpoint.Name, TemplateID: endpoint.TemplateID, GPUTypeIDs: endpoint.GPUTypeIDs, WorkersMin: endpoint.WorkersMin, WorkersMax: endpoint.WorkersMax, Workers: activeWorkers}
	}
	return out, nil
}

func (r RunPodServerless) ActiveWorkers(ctx context.Context, endpointID string) (int, error) {
	if endpointID == "" {
		return 0, errors.New("RunPod Serverless endpoint ID is required")
	}
	endpoints, err := r.ListEndpoints(ctx)
	if err != nil {
		return 0, err
	}
	for _, endpoint := range endpoints {
		if endpoint.ID == endpointID {
			return endpoint.Workers, nil
		}
	}
	return 0, fmt.Errorf("RunPod Serverless endpoint %s not found", endpointID)
}

func (r RunPodServerless) DeleteEndpoint(ctx context.Context, endpointID string) error {
	if endpointID == "" {
		return errors.New("RunPod Serverless endpoint ID is required")
	}
	err := r.do(ctx, http.MethodDelete, "/endpoints/"+url.PathEscape(endpointID), nil, nil)
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
	var template struct {
		ID           string            `json:"id"`
		IsServerless bool              `json:"isServerless"`
		Env          map[string]string `json:"env"`
	}
	path := "/templates/" + url.PathEscape(r.TemplateID) + "?includeEndpointBoundTemplates=true&includeRunpodTemplates=true"
	if err := r.do(ctx, http.MethodGet, path, nil, &template); err != nil {
		return err
	}
	if template.ID != r.TemplateID || !template.IsServerless {
		return errors.New("configured RunPod template is not a Serverless template")
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

func (r RunPodServerless) validateTemplate(ctx context.Context, templateID, model, revision string) error {
	var template struct {
		ID           string            `json:"id"`
		IsServerless bool              `json:"isServerless"`
		Env          map[string]string `json:"env"`
	}
	path := "/templates/" + url.PathEscape(templateID) + "?includeEndpointBoundTemplates=true&includeRunpodTemplates=true"
	if err := r.do(ctx, http.MethodGet, path, nil, &template); err != nil {
		return fmt.Errorf("validate RunPod Serverless template: %w", err)
	}
	if template.ID != templateID || !template.IsServerless {
		return errors.New("configured RunPod template is not a Serverless template")
	}
	if template.Env["MODEL_NAME"] != model {
		return fmt.Errorf("RunPod template MODEL_NAME is %q, expected %q", template.Env["MODEL_NAME"], model)
	}
	if template.Env["MODEL_REVISION"] != revision {
		return fmt.Errorf("RunPod template MODEL_REVISION is %q, expected immutable revision %q", template.Env["MODEL_REVISION"], revision)
	}
	if template.Env["RAW_OPENAI_OUTPUT"] != "1" {
		return errors.New("RunPod template must set RAW_OPENAI_OUTPUT=1 for OpenAI-compatible streaming")
	}
	if !strings.EqualFold(template.Env["ENABLE_AUTO_TOOL_CHOICE"], "true") || template.Env["TOOL_CALL_PARSER"] == "" {
		return errors.New("RunPod template must set ENABLE_AUTO_TOOL_CHOICE=true and TOOL_CALL_PARSER for OpenAI-compatible tool calls")
	}
	return nil
}

type providerHTTPError struct {
	Status int
	Body   string
}

func (e *providerHTTPError) Error() string {
	return fmt.Sprintf("RunPod API returned HTTP %d: %s", e.Status, e.Body)
}

func (r RunPodServerless) do(ctx context.Context, method, path string, body, output any) error {
	if r.APIKey == "" {
		return errors.New("RUNPOD_API_KEY is required")
	}
	base := strings.TrimRight(r.BaseURL, "/")
	if base == "" {
		base = defaultRunPodRESTURL
	}
	return r.doAbsolute(ctx, method, base+path, body, output)
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
		message := safeRunPodDiagnostic(string(payload), r.APIKey)
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
