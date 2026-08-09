package provision

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"gopkg.in/yaml.v3"
	"os"
	"os/exec"
	"regexp"
	"strings"
)

var ErrUnavailable = errors.New("SkyPilot unavailable")

type ReplicaSpec struct {
	ExternalKey                                     string
	Name, Model, Cloud, GPU, Region, RuntimeVersion string
	RequestID                                       string
	RuntimeArgs                                     []string
	Port                                            int
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

type CommandRunner interface {
	Run(context.Context, []string, ...string) ([]byte, error)
}
type execRunner struct{ binary string }

func (r execRunner) Run(ctx context.Context, env []string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, r.binary, args...)
	cmd.Env = append(os.Environ(), env...)
	return cmd.CombinedOutput()
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
	if spec.Port == 0 {
		spec.Port = 8000
	}
	if spec.RuntimeVersion == "" {
		spec.RuntimeVersion = "0.23.0"
	}
	resourceID := s.Handle(spec.ExternalKey).ResourceID
	observation, err := s.observe(ctx, resourceID, spec.Port, false)
	if err != nil {
		return ProviderHandle{}, err
	}
	if observation.Exists {
		return ProviderHandle{RequestID: spec.RequestID, ResourceID: resourceID, ExternalKey: spec.ExternalKey}, nil
	}
	if spec.RequestID != "" {
		state, found, requestErr := s.requestState(ctx, spec.RequestID)
		if requestErr != nil {
			return ProviderHandle{}, requestErr
		}
		if found && state != "failed" && state != "cancelled" {
			return ProviderHandle{RequestID: spec.RequestID, ResourceID: resourceID, ExternalKey: spec.ExternalKey}, nil
		}
	}
	task := map[string]any{"resources": map[string]any{"infra": infrastructure(spec.Cloud, spec.Region), "accelerators": spec.GPU, "ports": []int{spec.Port}}, "setup": "python -m pip install 'vllm==" + spec.RuntimeVersion + "'", "run": runCommand(spec.Model, spec.Port, spec.RuntimeArgs)}
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
		return ProviderHandle{}, fmt.Errorf("%w: launch %s: %s", ErrUnavailable, resourceID, strings.TrimSpace(string(output)))
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
		return "", false, fmt.Errorf("observe SkyPilot request %s: %w: %s", requestID, runErr, strings.TrimSpace(string(output)))
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
		return fmt.Errorf("delete %s: %s", handle.ResourceID, strings.TrimSpace(string(output)))
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
		result = result[:63]
	}
	return strings.TrimRight(result, "-")
}
func runCommand(model string, port int, runtimeArgs []string) string {
	args := make([]string, len(runtimeArgs))
	for i, arg := range runtimeArgs {
		args[i] = shellQuote(arg)
	}
	return fmt.Sprintf(`vllm serve %s --host 0.0.0.0 --port %d --served-model-name %s --api-key "$INFERCRANE_WORKER_API_KEY" %s`, shellQuote(model), port, shellQuote(model), strings.Join(args, " "))
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
		return Observation{}, fmt.Errorf("observe SkyPilot cluster %s: %w: %s", resourceID, err, strings.TrimSpace(string(output)))
	}
	statuses, err := parseStatuses(output)
	if err != nil {
		return Observation{}, err
	}
	if len(statuses) == 0 {
		return Observation{Exists: false, State: "absent"}, nil
	}
	status := statuses[0]
	observation := Observation{Exists: true, State: normalizeState(status.Status), Details: string(output)}
	if observation.State == "starting" || observation.State == "ready" {
		endpoint, endpointErr := runner.Run(ctx, nil, "status", resourceID, "--endpoint", strconvPort(port))
		if endpointErr == nil && strings.TrimSpace(string(endpoint)) != "" {
			observation.Endpoint = strings.TrimRight(strings.TrimSpace(string(endpoint)), "/")
			observation.State = "ready"
		}
	}
	return observation, nil
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
