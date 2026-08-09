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
)

const defaultRunPodRESTURL = "https://rest.runpod.io/v1"

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
		if endpoint.TemplateID != templateID || endpoint.WorkersMin != 0 || endpoint.WorkersMax != spec.WorkersMax || !contains(endpoint.GPUTypeIDs, runPodGPUType(spec.GPU)) {
			return ServerlessEndpoint{}, fmt.Errorf("existing RunPod Serverless endpoint %s does not match immutable intent", endpoint.ID)
		}
		return endpoint, nil
	}
	body := map[string]any{
		"name": name, "templateId": templateID, "computeType": "GPU",
		"gpuTypeIds": []string{runPodGPUType(spec.GPU)}, "gpuCount": 1,
		"workersMin": 0, "workersMax": spec.WorkersMax,
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
	return endpoint, nil
}

func (r RunPodServerless) ListEndpoints(ctx context.Context) ([]ServerlessEndpoint, error) {
	var endpoints []struct {
		ID, Name, TemplateID string
		GPUTypeIDs           []string `json:"gpuTypeIds"`
		WorkersMin           int      `json:"workersMin"`
		WorkersMax           int      `json:"workersMax"`
		Workers              []any    `json:"workers"`
	}
	if err := r.do(ctx, http.MethodGet, "/endpoints?includeWorkers=true", nil, &endpoints); err != nil {
		return nil, err
	}
	out := make([]ServerlessEndpoint, len(endpoints))
	for i, endpoint := range endpoints {
		out[i] = ServerlessEndpoint{ID: endpoint.ID, Name: endpoint.Name, TemplateID: endpoint.TemplateID, GPUTypeIDs: endpoint.GPUTypeIDs, WorkersMin: endpoint.WorkersMin, WorkersMax: endpoint.WorkersMax, Workers: len(endpoint.Workers)}
	}
	return out, nil
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
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, base+path, reader)
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
		return &providerHTTPError{Status: response.StatusCode, Body: strings.TrimSpace(string(payload))}
	}
	if output != nil && len(payload) > 0 {
		if err = json.Unmarshal(payload, output); err != nil {
			return fmt.Errorf("decode RunPod API response: %w", err)
		}
	}
	return nil
}

func serverlessName(externalKey string) string { return clusterName(externalKey) + "-serverless" }

func runPodGPUType(gpu string) string {
	switch strings.ToUpper(strings.TrimSpace(gpu)) {
	case "L40S", "NVIDIA L40S":
		return "NVIDIA L40S"
	case "H100", "NVIDIA H100 80GB HBM3":
		return "NVIDIA H100 80GB HBM3"
	case "A100-80GB", "A100 80GB", "NVIDIA A100-SXM4-80GB":
		return "NVIDIA A100-SXM4-80GB"
	default:
		return gpu
	}
}

func contains(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}
