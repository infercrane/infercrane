package provision

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"

	"github.com/infercrane/infercrane/internal/runtimecontract"
	"github.com/infercrane/infercrane/internal/servingcontract"
	"github.com/infercrane/infercrane/internal/support"
	"gopkg.in/yaml.v3"
)

var ErrUnavailable = errors.New("SkyPilot unavailable")
var ErrRequestFailed = errors.New("SkyPilot launch request failed")
var ErrInvalidReplicaSpec = errors.New("invalid provider replica specification")
var ErrProviderAuthorization = errors.New("provider authorization denied")
var ErrProviderQuota = errors.New("provider quota exceeded")
var ErrProviderCapacity = errors.New("provider capacity unavailable")

// The default image is pinned by digest so a revision always boots the tested
// runtime bits. Using vLLM's provider-neutral image keeps provisioning portable
// across every SkyPilot cloud and avoids installing the CUDA dependency stack
// on each new GPU replica.
const defaultVLLMImage = "vllm/vllm-openai@sha256:953d3a06d5e64ab582985cd7401289d3abf2a2c14ef2158e9a84313daeec77d7"

// The RunPod variant adds only the SSH bootstrap contract required by
// SkyPilot. Pin it independently so a release never depends on a mutable GHCR
// tag after qualification.
const defaultRunPodVLLMImage = "ghcr.io/infercrane/vllm-runpod@sha256:c9d8303ad7c36e3b25a160c892626be8b0dde8f5954b11095c33d2bca31a9711"

type ReplicaSpec struct {
	ExternalKey                                                             string
	Name, Model, ModelRevision, Cloud, GPU, Region, Runtime, RuntimeVersion string
	RequestID                                                               string
	RuntimeArgs                                                             []string
	Port                                                                    int
	Workload                                                                runtimecontract.Workload
	Serving                                                                 servingcontract.Topology
}
type ProviderHandle struct{ RequestID, ResourceID, ExternalKey string }
type Observation struct {
	Exists   bool
	State    string
	Endpoint string
	Details  string
}
type InventoryFilter struct{ Prefix string }
type Resource struct{ ID, ExternalKey, State, Endpoint string }

type AvailabilityRequest struct {
	Cloud, GPU, Region string
	Count              int
}

type Availability struct {
	State   string `json:"state"`
	Message string `json:"message"`
	Details string `json:"details,omitempty"`
}

type CommandRunner interface {
	Run(context.Context, []string, ...string) ([]byte, error)
}
type execRunner struct{ binary string }

const maxProviderCommandOutput = 8 << 20

var errProviderCommandOutputLimit = errors.New("provider command output exceeded 8 MiB")

type boundedCommandOutput struct {
	mu       sync.Mutex
	data     []byte
	exceeded bool
}

func (w *boundedCommandOutput) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	remaining := maxProviderCommandOutput - len(w.data)
	if remaining > 0 {
		if remaining > len(p) {
			remaining = len(p)
		}
		w.data = append(w.data, p[:remaining]...)
	}
	if remaining < len(p) {
		w.exceeded = true
	}
	return len(p), nil
}

func (r execRunner) Run(ctx context.Context, env []string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, r.binary, args...)
	cmd.Env = append(os.Environ(), env...)
	output := &boundedCommandOutput{data: make([]byte, 0, 64<<10)}
	cmd.Stdout, cmd.Stderr = output, output
	err := cmd.Run()
	if output.exceeded {
		err = errors.Join(err, errProviderCommandOutputLimit)
	}
	return output.data, err
}

type SkyPilot struct {
	Binary, APIKey string
	Runner         CommandRunner
}

// Handle returns the deterministic provider identity for a durable external
// key without contacting SkyPilot. Callers can persist it before mutation.
func (s SkyPilot) Handle(externalKey string) ProviderHandle {
	return ProviderHandle{ResourceID: clusterName(externalKey), ExternalKey: externalKey}
}

func (s SkyPilot) EnsureReplica(ctx context.Context, spec ReplicaSpec) (ProviderHandle, error) {
	if spec.ExternalKey == "" || spec.Model == "" || spec.Cloud == "" || spec.GPU == "" {
		return ProviderHandle{}, errors.New("external key, model, cloud, and GPU are required")
	}
	if !spec.Workload.Empty() {
		if err := spec.Workload.Validate(); err != nil {
			return ProviderHandle{}, fmt.Errorf("%w: %v", ErrInvalidReplicaSpec, err)
		}
		spec.Port = spec.Workload.Port
	}
	if spec.Port == 0 {
		spec.Port = 8000
	}
	usesDefaultRuntime := spec.RuntimeVersion == ""
	if usesDefaultRuntime {
		spec.RuntimeVersion = support.DefaultRuntimeVersion
	}
	resourceID := s.Handle(spec.ExternalKey).ResourceID
	requestState, requestFound := "", false
	if spec.RequestID != "" {
		var requestErr error
		requestState, requestFound, requestErr = s.requestState(ctx, spec.RequestID)
		if requestErr != nil {
			return ProviderHandle{}, requestErr
		}
	}
	observation, err := s.observe(ctx, resourceID, spec.Port, false)
	if err != nil {
		return ProviderHandle{}, err
	}
	if observation.Exists {
		if requestFound && (requestState == "failed" || requestState == "cancelled") {
			// SkyPilot can leave a provider record behind when asynchronous setup
			// fails after allocation. Remove that exact deterministic resource;
			// the next durable retry may safely reuse the same replica intent.
			if deleteErr := s.DeleteReplica(ctx, ProviderHandle{ResourceID: resourceID, ExternalKey: spec.ExternalKey}); deleteErr != nil {
				return ProviderHandle{}, fmt.Errorf("%w: %w: async request %s and cleanup failed: %v", ErrUnavailable, ErrRequestFailed, requestState, deleteErr)
			}
			return ProviderHandle{}, fmt.Errorf("%w: %w: async request %s; stale resource cleanup submitted", ErrUnavailable, ErrRequestFailed, requestState)
		}
		return ProviderHandle{RequestID: spec.RequestID, ResourceID: resourceID, ExternalKey: spec.ExternalKey}, nil
	}
	if spec.RequestID != "" {
		if requestFound && requestState != "failed" && requestState != "cancelled" {
			return ProviderHandle{RequestID: spec.RequestID, ResourceID: resourceID, ExternalKey: spec.ExternalKey}, nil
		}
	}
	runtimeImage := "vllm/vllm-openai:v" + spec.RuntimeVersion
	if usesDefaultRuntime {
		runtimeImage = defaultVLLMImage
	}
	if strings.EqualFold(spec.Cloud, "runpod") {
		// RunPod Pods require their container to start the platform SSH service
		// used by SkyPilot. The thin InferCrane image retains that contract while
		// baking vLLM; other clouds remain on the provider-neutral official image.
		runtimeImage = "ghcr.io/infercrane/vllm-runpod:v" + spec.RuntimeVersion
		if usesDefaultRuntime {
			runtimeImage = defaultRunPodVLLMImage
		}
	}
	run := runCommand(spec.Model, spec.ModelRevision, spec.Port, spec.RuntimeArgs)
	if !spec.Workload.Empty() {
		runtimeImage = spec.Workload.Image
		run = workloadCommand(spec.Workload.Command, spec.Model, spec.ModelRevision, spec.Port, spec.RuntimeArgs)
	}
	task := map[string]any{"resources": map[string]any{"infra": infrastructure(spec.Cloud, spec.Region), "accelerators": spec.GPU, "image_id": "docker:" + runtimeImage, "ports": []int{spec.Port}}, "run": run}
	task["secrets"] = map[string]any{"INFERCRANE_WORKER_API_KEY": nil}
	path, cleanup, err := writeTask(task)
	if err != nil {
		return ProviderHandle{}, err
	}
	defer cleanup()
	runner, err := s.runner()
	if err != nil {
		return ProviderHandle{}, err
	}
	output, err := runner.Run(ctx, []string{"INFERCRANE_WORKER_API_KEY=" + s.APIKey}, "launch", "-c", resourceID, "-y", "--async", "--secret", "INFERCRANE_WORKER_API_KEY", path)
	if err != nil {
		return ProviderHandle{}, fmt.Errorf("%w: launch %s: %s", ErrUnavailable, resourceID, s.safeDiagnostic(output))
	}
	return ProviderHandle{RequestID: requestID(string(output)), ResourceID: resourceID, ExternalKey: spec.ExternalKey}, nil
}

func (s SkyPilot) requestState(ctx context.Context, requestID string) (string, bool, error) {
	runner, err := s.runner()
	if err != nil {
		return "", false, err
	}
	output, runErr := runner.Run(ctx, nil, "api", "status", "--all-status", "--output", "json", requestID)
	if runErr != nil {
		return "", false, fmt.Errorf("observe SkyPilot request %s: %w: %s", requestID, runErr, s.safeDiagnostic(output))
	}
	var rows []map[string]any
	if err = json.Unmarshal(jsonPayload(output), &rows); err != nil {
		return "", false, fmt.Errorf("parse SkyPilot request status: %w", err)
	}
	for _, row := range rows {
		if stringField(row, "request_id") == requestID {
			return strings.ToLower(stringField(row, "status")), true, nil
		}
	}
	return "", false, nil
}

func (s SkyPilot) ObserveReplica(ctx context.Context, handle ProviderHandle, port int) (Observation, error) {
	if handle.ResourceID == "" {
		return Observation{}, errors.New("provider resource ID is required")
	}
	if port == 0 {
		port = 8000
	}
	return s.observe(ctx, handle.ResourceID, port, true)
}

func (s SkyPilot) DeleteReplica(ctx context.Context, handle ProviderHandle) error {
	if handle.ResourceID == "" {
		return errors.New("provider resource ID is required")
	}
	observation, err := s.observe(ctx, handle.ResourceID, 8000, false)
	if err != nil {
		return err
	}
	if !observation.Exists {
		return nil
	}
	runner, runnerErr := s.runner()
	if runnerErr != nil {
		return runnerErr
	}
	output, runErr := runner.Run(ctx, nil, "down", "-y", "--async", handle.ResourceID)
	if runErr != nil {
		// Re-observe before returning an error: a repeated delete is successful once absent.
		if current, observeErr := s.observe(ctx, handle.ResourceID, 8000, false); observeErr == nil && !current.Exists {
			return nil
		}
		return fmt.Errorf("delete %s: %s", handle.ResourceID, s.safeDiagnostic(output))
	}
	return nil
}

func (s SkyPilot) Inventory(ctx context.Context, filter InventoryFilter) ([]Resource, error) {
	runner, err := s.runner()
	if err != nil {
		return nil, err
	}
	output, err := runner.Run(ctx, nil, "status", "-o", "json")
	if err != nil {
		return nil, fmt.Errorf("inventory SkyPilot clusters: %w", err)
	}
	statuses, err := parseStatuses(output)
	if err != nil {
		return nil, err
	}
	resources := make([]Resource, 0, len(statuses))
	for _, status := range statuses {
		if filter.Prefix != "" && !strings.HasPrefix(status.Name, filter.Prefix) {
			continue
		}
		resources = append(resources, Resource{ID: status.Name, ExternalKey: strings.TrimPrefix(status.Name, "infercrane-"), State: normalizeState(status.Status)})
	}
	return resources, nil
}

func clusterName(name string) string {
	slug := regexp.MustCompile(`[^a-z0-9-]+`).ReplaceAllString(strings.ToLower(name), "-")
	slug = strings.Trim(slug, "-")
	result := "infercrane-" + slug
	if len(result) > 63 {
		digest := sha256.Sum256([]byte(name))
		const suffixLength = 12
		prefixLength := 63 - 1 - suffixLength
		result = strings.TrimRight(result[:prefixLength], "-") + "-" + fmt.Sprintf("%x", digest[:6])
	}
	return strings.TrimRight(result, "-")
}
func runCommand(model, revision string, port int, runtimeArgs []string) string {
	args := make([]string, len(runtimeArgs))
	for i, arg := range runtimeArgs {
		args[i] = shellQuote(arg)
	}
	if revision != "" {
		args = append([]string{"--revision", shellQuote(revision)}, args...)
	}
	// Some RunPod base images still export the retired hf_transfer feature flag
	// without installing that package. Disable it so huggingface_hub uses its
	// supported hf_xet/HTTP transfer path instead of failing before model load.
	return fmt.Sprintf(`unset HF_HUB_ENABLE_HF_TRANSFER; VLLM_API_KEY="$INFERCRANE_WORKER_API_KEY" exec vllm serve %s --host 0.0.0.0 --port %d --served-model-name %s %s`, shellQuote(model), port, shellQuote(model), strings.Join(args, " "))
}
func workloadCommand(command []string, model, revision string, port int, runtimeArgs []string) string {
	values := append(append([]string(nil), command...), runtimeArgs...)
	for i, value := range values {
		if value == "${WORKER_API_KEY}" {
			values[i] = `"$INFERCRANE_WORKER_API_KEY"`
			continue
		}
		value = strings.ReplaceAll(value, "${MODEL}", model)
		value = strings.ReplaceAll(value, "${MODEL_REVISION}", revision)
		value = strings.ReplaceAll(value, "${PORT}", fmt.Sprint(port))
		values[i] = shellQuote(value)
	}
	return "exec " + strings.Join(values, " ")
}
func shellQuote(value string) string { return "'" + strings.ReplaceAll(value, "'", `'"'"'`) + "'" }
func defaultString(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}
func strconvPort(port int) string { return fmt.Sprintf("%d", port) }

type clusterStatus struct{ Name, Status string }

func (s SkyPilot) runner() (CommandRunner, error) {
	if s.Runner != nil {
		return s.Runner, nil
	}
	binary, err := exec.LookPath(defaultString(s.Binary, "sky"))
	if err != nil {
		return nil, fmt.Errorf("%w: install the SkyPilot CLI", ErrUnavailable)
	}
	return execRunner{binary: binary}, nil
}

func (s SkyPilot) observe(ctx context.Context, resourceID string, port int, refresh bool) (Observation, error) {
	runner, err := s.runner()
	if err != nil {
		return Observation{}, err
	}
	args := []string{"status"}
	if refresh {
		args = append(args, "--refresh")
	}
	args = append(args, "-o", "json", resourceID)
	output, err := runner.Run(ctx, nil, args...)
	if clusterMissing(output) {
		return Observation{Exists: false, State: "absent"}, nil
	}
	if err != nil {
		return Observation{}, fmt.Errorf("observe SkyPilot cluster %s: %w: %s", resourceID, err, s.safeDiagnostic(output))
	}
	statuses, err := parseStatuses(output)
	if err != nil {
		return Observation{}, err
	}
	if len(statuses) == 0 {
		return Observation{Exists: false, State: "absent"}, nil
	}
	status := statuses[0]
	observation := Observation{Exists: true, State: normalizeState(status.Status), Details: s.safeDiagnostic(output)}
	if observation.State == "starting" || observation.State == "ready" {
		endpoint, endpointErr := runner.Run(ctx, nil, "status", resourceID, "--endpoint", strconvPort(port))
		if endpointErr == nil && strings.TrimSpace(string(endpoint)) != "" {
			observation.Endpoint = normalizeEndpoint(string(endpoint))
			observation.State = "ready"
			if queue, queueErr := runner.Run(ctx, nil, "queue", resourceID, "-o", "json"); queueErr == nil {
				if failed, reason := failedJob(queue); failed {
					observation.State = "failed"
					observation.Details = s.safeDiagnostic(output) + "\n" + s.safeDiagnostic(queue)
					if reason != "" {
						observation.Details += "\nruntime job failure: " + reason
					}
				}
			}
		}
	}
	return observation, nil
}

func (s SkyPilot) safeDiagnostic(output []byte) string {
	const limit = 4096
	if len(output) > limit {
		output = append(append([]byte(nil), output[:limit]...), []byte("…")...)
	}
	message := strings.TrimSpace(string(output))
	if s.APIKey != "" {
		message = strings.ReplaceAll(message, s.APIKey, "[REDACTED]")
	}
	return message
}

func failedJob(data []byte) (bool, string) {
	data = jsonPayload(data)
	var value any
	if json.Unmarshal(data, &value) != nil {
		return false, ""
	}
	var inspect func(any) (bool, string)
	inspect = func(current any) (bool, string) {
		switch typed := current.(type) {
		case map[string]any:
			status := strings.ToUpper(stringField(typed, "status", "state"))
			if status == "FAILED" || status == "CANCELLED" {
				return true, stringField(typed, "failure_reason", "error", "message")
			}
			for _, nested := range typed {
				if failed, reason := inspect(nested); failed {
					return true, reason
				}
			}
		case []any:
			for _, nested := range typed {
				if failed, reason := inspect(nested); failed {
					return true, reason
				}
			}
		}
		return false, ""
	}
	return inspect(value)
}

func normalizeEndpoint(value string) string {
	value = strings.TrimRight(strings.TrimSpace(value), "/")
	if value != "" && !strings.Contains(value, "://") {
		value = "http://" + value
	}
	return value
}

func parseStatuses(data []byte) ([]clusterStatus, error) {
	data = jsonPayload(data)
	var list []map[string]any
	if err := json.Unmarshal(data, &list); err != nil {
		var keyed map[string]map[string]any
		if keyedErr := json.Unmarshal(data, &keyed); keyedErr != nil {
			return nil, fmt.Errorf("parse SkyPilot status JSON: %w", err)
		}
		for name, values := range keyed {
			values["name"] = name
			list = append(list, values)
		}
	}
	statuses := make([]clusterStatus, 0, len(list))
	for _, item := range list {
		name := stringField(item, "name", "cluster_name")
		if name == "" {
			continue
		}
		statuses = append(statuses, clusterStatus{Name: name, Status: stringField(item, "status", "state")})
	}
	return statuses, nil
}

func clusterMissing(output []byte) bool {
	message := strings.ToLower(string(output))
	return strings.Contains(message, "cluster") && strings.Contains(message, "not found")
}

// SkyPilot may prefix machine-readable output with API-server notices. Find the
// first JSON value that decodes instead of requiring stdout to contain JSON only.
func jsonPayload(data []byte) []byte {
	for index, value := range data {
		if value != '[' && value != '{' {
			continue
		}
		var raw json.RawMessage
		if json.NewDecoder(strings.NewReader(string(data[index:]))).Decode(&raw) == nil {
			return raw
		}
	}
	return data
}
func stringField(values map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := values[key].(string); ok {
			return value
		}
	}
	return ""
}
func normalizeState(status string) string {
	switch strings.ToUpper(status) {
	case "UP":
		return "starting"
	case "INIT":
		return "provisioning"
	case "STOPPED", "DOWN":
		return "failed"
	default:
		return strings.ToLower(status)
	}
}
func infrastructure(cloud, region string) string {
	if region == "" {
		return cloud
	}
	return cloud + "/" + region
}
func writeTask(task map[string]any) (string, func(), error) {
	data, err := yaml.Marshal(task)
	if err != nil {
		return "", func() {}, err
	}
	file, err := os.CreateTemp("", "infercrane-skypilot-*.yaml")
	if err != nil {
		return "", func() {}, err
	}
	cleanup := func() { _ = os.Remove(file.Name()) }
	if _, err = file.Write(data); err != nil {
		_ = file.Close()
		cleanup()
		return "", func() {}, err
	}
	if err = file.Close(); err != nil {
		cleanup()
		return "", func() {}, err
	}
	return file.Name(), cleanup, nil
}
func requestID(output string) string {
	match := regexp.MustCompile(`(?i)\b[0-9a-f]{8}-[0-9a-f-]{27,36}\b`).FindString(output)
	return match
}
