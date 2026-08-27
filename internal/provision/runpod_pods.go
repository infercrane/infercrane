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
	"strconv"
	"strings"
	"time"
)

// RunPodPods launches immutable custom OCI workloads through RunPod's native
// Pod API. Unlike the SkyPilot adapter, it does not require the workload image
// to contain an SSH daemon or rsync. ExternalKey is compiled into a
// deterministic provider name so a retry can adopt a Pod after a lost create
// response.
type RunPodPods struct {
	APIKey, WorkerAPIKey, BaseURL string
	ContainerDiskGiB              int
	Client                        *http.Client
}

type runPodRecord struct {
	ID               string            `json:"id"`
	Name             string            `json:"name"`
	DesiredStatus    string            `json:"desiredStatus"`
	Image            string            `json:"image"`
	ImageName        string            `json:"imageName"`
	GPUCount         int               `json:"gpuCount"`
	DockerEntrypoint []string          `json:"dockerEntrypoint"`
	DockerStartCmd   []string          `json:"dockerStartCmd"`
	Ports            []string          `json:"ports"`
	MachineID        string            `json:"machineId"`
	LastStartedAt    string            `json:"lastStartedAt"`
	LastStatusChange string            `json:"lastStatusChange"`
	CostPerHour      json.RawMessage   `json:"costPerHr"`
	AdjustedCost     json.RawMessage   `json:"adjustedCostPerHr"`
	Environment      map[string]string `json:"env"`
	PublicIP         string            `json:"publicIp"`
	ContainerDiskGiB int               `json:"containerDiskInGb"`
	VolumeInGiB      int               `json:"volumeInGb"`
	GPUTypeID        string            `json:"gpuTypeId"`
	GPU              struct {
		ID string `json:"id"`
	} `json:"gpu"`
}

func (r RunPodPods) Handle(externalKey string) ProviderHandle {
	return ProviderHandle{ResourceID: clusterName(externalKey), ExternalKey: externalKey}
}

func (r RunPodPods) EnsureReplica(ctx context.Context, spec ReplicaSpec) (ProviderHandle, error) {
	if strings.TrimSpace(r.APIKey) == "" || strings.TrimSpace(r.WorkerAPIKey) == "" {
		return ProviderHandle{}, fmt.Errorf("%w: RunPod and worker API keys are required", ErrProviderAuthorization)
	}
	if spec.ExternalKey == "" || spec.Model == "" || !strings.EqualFold(spec.Cloud, "runpod") || spec.GPU == "" {
		return ProviderHandle{}, fmt.Errorf("%w: external key, model, runpod cloud, and GPU are required", ErrInvalidReplicaSpec)
	}
	if spec.GPUCount == 0 {
		spec.GPUCount = 1
	}
	if spec.GPUCount < 1 || spec.GPUCount > 8 {
		return ProviderHandle{}, fmt.Errorf("%w: RunPod Pod GPU count must be between 1 and 8", ErrInvalidReplicaSpec)
	}
	if strings.TrimSpace(spec.Region) != "" {
		return ProviderHandle{}, fmt.Errorf("%w: native RunPod Pods do not accept a region constraint; leave provider.region empty", ErrInvalidReplicaSpec)
	}
	if spec.Workload.Empty() {
		return ProviderHandle{}, fmt.Errorf("%w: native RunPod Pods require an immutable custom OCI workload", ErrInvalidReplicaSpec)
	}
	if err := spec.Workload.Validate(); err != nil {
		return ProviderHandle{}, fmt.Errorf("%w: %v", ErrInvalidReplicaSpec, err)
	}
	name := r.Handle(spec.ExternalKey).ResourceID
	pods, err := r.list(ctx)
	if err != nil {
		return ProviderHandle{}, err
	}
	matches := matchingRunPodRecords(pods, name)
	if len(matches) > 1 {
		return ProviderHandle{}, fmt.Errorf("%w: multiple RunPod Pods match durable key %q", ErrRequestFailed, spec.ExternalKey)
	}
	command := expandWorkloadCommand(spec.Workload.Command, spec.Model, spec.ModelRevision, spec.Workload.Port, spec.RuntimeArgs)
	if len(matches) == 1 {
		if err = validateRunPodIntent(matches[0], spec, command); err != nil {
			return ProviderHandle{}, err
		}
		return ProviderHandle{ResourceID: matches[0].ID, ExternalKey: spec.ExternalKey}, nil
	}
	disk := r.ContainerDiskGiB
	if disk == 0 {
		disk = 100
	}
	if disk < 50 || disk > 2048 {
		return ProviderHandle{}, fmt.Errorf("%w: RunPod container disk must be between 50 and 2048 GiB", ErrInvalidReplicaSpec)
	}
	body := map[string]any{
		"name": name, "imageName": spec.Workload.Image,
		"gpuTypeIds": []string{runPodGPUType(spec.GPU)}, "gpuTypePriority": "custom", "gpuCount": spec.GPUCount,
		"cloudType": "SECURE", "computeType": "GPU", "interruptible": false,
		"containerDiskInGb": disk,
		"dockerEntrypoint":  []string{command[0]}, "dockerStartCmd": command[1:],
		"ports": []string{strconv.Itoa(spec.Workload.Port) + "/http"},
		"env":   map[string]string{"INFERCRANE_WORKER_API_KEY": r.WorkerAPIKey, "VLLM_API_KEY": r.WorkerAPIKey},
	}
	var created runPodRecord
	if err = r.do(ctx, http.MethodPost, "/pods", body, &created); err != nil {
		// A transport failure may occur after RunPod committed the Pod. Re-list
		// and adopt the deterministic name before surfacing a retryable failure.
		if current, listErr := r.list(ctx); listErr == nil {
			matches = matchingRunPodRecords(current, name)
			if len(matches) == 1 && validateRunPodIntent(matches[0], spec, command) == nil {
				return ProviderHandle{ResourceID: matches[0].ID, ExternalKey: spec.ExternalKey}, nil
			}
		}
		return ProviderHandle{}, err
	}
	if created.ID == "" {
		return ProviderHandle{}, fmt.Errorf("%w: RunPod create returned no Pod ID", ErrRequestFailed)
	}
	return ProviderHandle{ResourceID: created.ID, ExternalKey: spec.ExternalKey}, nil
}

func (r RunPodPods) ObserveReplica(ctx context.Context, handle ProviderHandle, port int) (Observation, error) {
	if handle.ResourceID == "" {
		return Observation{}, errors.New("provider resource ID is required")
	}
	pods, err := r.list(ctx)
	if err != nil {
		return Observation{}, err
	}
	var pod *runPodRecord
	for index := range pods {
		if pods[index].ID == handle.ResourceID || pods[index].Name == handle.ResourceID {
			pod = &pods[index]
			break
		}
	}
	if pod == nil {
		return Observation{Exists: false, State: "absent"}, nil
	}
	if port == 0 {
		port = 8000
	}
	details, _ := json.Marshal(map[string]any{
		"provider": "runpod-pods", "pod_id": pod.ID, "status": pod.DesiredStatus,
		"machine_id": pod.MachineID, "last_started_at": pod.LastStartedAt,
		"last_status_change": pod.LastStatusChange, "container_disk_gib": pod.ContainerDiskGiB,
		"cost_per_hour": rawNumber(pod.CostPerHour), "adjusted_cost_per_hour": rawNumber(pod.AdjustedCost),
	})
	switch strings.ToUpper(pod.DesiredStatus) {
	case "EXITED", "TERMINATED":
		return Observation{Exists: true, State: "failed", Details: string(details)}, nil
	case "RUNNING":
		return Observation{Exists: true, State: "ready", Endpoint: fmt.Sprintf("https://%s-%d.proxy.runpod.net", pod.ID, port), Details: string(details)}, nil
	default:
		return Observation{Exists: true, State: "starting", Details: string(details)}, nil
	}
}

func (r RunPodPods) DeleteReplica(ctx context.Context, handle ProviderHandle) error {
	if handle.ResourceID == "" {
		return errors.New("provider resource ID is required")
	}
	pods, err := r.list(ctx)
	if err != nil {
		return err
	}
	for _, pod := range pods {
		if pod.ID != handle.ResourceID && pod.Name != handle.ResourceID {
			continue
		}
		if err = r.do(ctx, http.MethodDelete, "/pods/"+url.PathEscape(pod.ID), nil, nil); err != nil {
			return err
		}
	}
	return nil
}

func (r RunPodPods) list(ctx context.Context) ([]runPodRecord, error) {
	var pods []runPodRecord
	if err := r.do(ctx, http.MethodGet, "/pods", nil, &pods); err != nil {
		return nil, err
	}
	return pods, nil
}

func (r RunPodPods) do(ctx context.Context, method, path string, body, output any) error {
	base := strings.TrimRight(r.BaseURL, "/")
	if base == "" {
		base = defaultRunPodRESTURL
	}
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode RunPod Pod request: %w", err)
		}
		reader = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, base+path, reader)
	if err != nil {
		return fmt.Errorf("create RunPod Pod request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+r.APIKey)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	client := r.Client
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("%w: RunPod Pod API request failed", ErrRequestFailed)
	}
	defer response.Body.Close()
	payload, readErr := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if readErr != nil {
		return fmt.Errorf("read RunPod Pod response: %w", readErr)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		message := safeRunPodDiagnostic(string(payload), r.APIKey)
		switch response.StatusCode {
		case http.StatusUnauthorized, http.StatusForbidden:
			return fmt.Errorf("%w: RunPod Pod API returned HTTP %d: %s", ErrProviderAuthorization, response.StatusCode, message)
		case http.StatusConflict, http.StatusTooManyRequests:
			return fmt.Errorf("%w: RunPod Pod API returned HTTP %d: %s", ErrProviderCapacity, response.StatusCode, message)
		default:
			if strings.Contains(strings.ToLower(message), "capacity") || strings.Contains(strings.ToLower(message), "stock") {
				return fmt.Errorf("%w: RunPod Pod API returned HTTP %d: %s", ErrProviderCapacity, response.StatusCode, message)
			}
			return fmt.Errorf("%w: RunPod Pod API returned HTTP %d: %s", ErrRequestFailed, response.StatusCode, message)
		}
	}
	if output != nil && len(bytes.TrimSpace(payload)) > 0 {
		if err = json.Unmarshal(payload, output); err != nil {
			return fmt.Errorf("decode RunPod Pod response: %w", err)
		}
	}
	return nil
}

func matchingRunPodRecords(pods []runPodRecord, name string) []runPodRecord {
	matches := make([]runPodRecord, 0, 1)
	for _, pod := range pods {
		if pod.Name == name && strings.ToUpper(pod.DesiredStatus) != "TERMINATED" {
			matches = append(matches, pod)
		}
	}
	return matches
}

func validateRunPodIntent(pod runPodRecord, spec ReplicaSpec, command []string) error {
	image := pod.ImageName
	if image == "" {
		image = pod.Image
	}
	actualGPU := pod.GPUTypeID
	if actualGPU == "" {
		actualGPU = pod.GPU.ID
	}
	credentialMismatch := len(pod.Environment) > 0 && (pod.Environment["INFERCRANE_WORKER_API_KEY"] == "" || pod.Environment["VLLM_API_KEY"] == "")
	if image != spec.Workload.Image || pod.GPUCount != spec.GPUCount || (actualGPU != "" && actualGPU != runPodGPUType(spec.GPU)) || credentialMismatch || !equalStrings(pod.DockerEntrypoint, command[:1]) || !equalStrings(pod.DockerStartCmd, command[1:]) {
		return fmt.Errorf("%w: existing RunPod Pod does not match immutable workload intent", ErrInvalidReplicaSpec)
	}
	return nil
}

func expandWorkloadCommand(command []string, model, revision string, port int, runtimeArgs []string) []string {
	values := append(append([]string(nil), command...), runtimeArgs...)
	for index, value := range values {
		value = strings.ReplaceAll(value, "${MODEL}", model)
		value = strings.ReplaceAll(value, "${MODEL_REVISION}", revision)
		value = strings.ReplaceAll(value, "${PORT}", strconv.Itoa(port))
		values[index] = value
	}
	return values
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func rawNumber(value json.RawMessage) any {
	if len(value) == 0 {
		return nil
	}
	var decoded any
	if json.Unmarshal(value, &decoded) != nil {
		return nil
	}
	return decoded
}
