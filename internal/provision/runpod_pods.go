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

	"github.com/infercrane/infercrane/internal/artifactcache"
	"github.com/infercrane/infercrane/internal/secrets"
)

// RunPodPods launches immutable OCI workloads through RunPod's native Pod API.
// Built-in runtimes and custom workloads share the same validated contract.
// Unlike the SkyPilot adapter, it does not require the workload image
// to contain an SSH daemon or rsync. ExternalKey is compiled into a
// deterministic provider name so a retry can adopt a Pod after a lost create
// response.
type RunPodPods struct {
	APIKey, WorkerAPIKey, BaseURL string
	WorkerCredentialVersion       string
	ContainerDiskGiB              int
	ArtifactCachePolicy           string
	HFTokenSecret                 string
	NetworkVolumes                map[string]string
	Client                        *http.Client
}

func (r RunPodPods) VerifyCredential(ctx context.Context, credential string) error {
	r.APIKey = strings.TrimSpace(credential)
	if r.APIKey == "" {
		return ErrProviderAuthorization
	}
	_, err := r.list(ctx)
	return err
}

const runPodPodsUserAgent = "InferCrane-RunPod-Pods/1.0"

// runPodArtifactMountPath is where an identity-bound network volume is mounted
// inside a native Pod.
const runPodArtifactMountPath = "/workspace"

type runPodNetworkVolume struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	DataCenterID string `json:"dataCenter"`
	Size         int    `json:"size"`
}

type runPodGPU struct {
	ID    string `json:"id"`
	Count int    `json:"count"`
}

type runPodNetworkMount struct {
	VolumeID string `json:"volumeId"`
	Path     string `json:"path"`
}

// runPodRecord is the subset of RunPod REST v2's Pod schema that InferCrane
// reads (GET /v2/pods). Status is the observed lifecycle state.
type runPodRecord struct {
	ID           string            `json:"id"`
	Name         string            `json:"name"`
	Status       string            `json:"status"`
	Image        string            `json:"image"`
	Entrypoint   []string          `json:"entrypoint"`
	Cmd          []string          `json:"cmd"`
	Ports        []string          `json:"ports"`
	Environment  map[string]string `json:"env"`
	DiskGiB      int               `json:"disk"`
	DataCenterID string            `json:"dataCenterId"`
	CostPerHour  json.RawMessage   `json:"cost"`
	CreatedAt    string            `json:"createdAt"`
	StartedAt    string            `json:"startedAt"`
	GPU          *runPodGPU        `json:"gpu"`
	Mounts       struct {
		Network []runPodNetworkMount `json:"network"`
	} `json:"mounts"`
}

func (pod runPodRecord) networkVolumeID() string {
	if len(pod.Mounts.Network) == 0 {
		return ""
	}
	return pod.Mounts.Network[0].VolumeID
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
	if spec.Workload.Empty() {
		return ProviderHandle{}, fmt.Errorf("%w: native RunPod Pods require an immutable OCI workload", ErrInvalidReplicaSpec)
	}
	if err := spec.Workload.Validate(); err != nil {
		return ProviderHandle{}, fmt.Errorf("%w: %v", ErrInvalidReplicaSpec, err)
	}
	engineVariables, err := engineEnvironment(spec)
	if err != nil {
		return ProviderHandle{}, err
	}
	// A Pod has no cloud identity to read an object store. A model credential
	// becomes a RunPod secret in the account that runs the Pod; Hugging Face
	// adapters are fetched in the Pod at their pinned commit.
	if err = refuseModelInputs("runpod-pods", spec, ModelInputs{Credential: true, Adapters: true}, secrets.ResolverSealed, secrets.ResolverEnvironment); err != nil {
		return ProviderHandle{}, err
	}
	hfTokenSecret := r.HFTokenSecret
	if spec.ModelCredential != nil {
		hfTokenSecret = runPodModelSecretName(spec.TenantID, spec.ModelCredential.ReferenceID)
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
	command := expandWorkloadCommand(servedCommand(spec.Workload.Command, spec), spec.Model, spec.ModelRevision, spec.Workload.Port, spec.RuntimeArgs)
	if hubFetchNeeded(spec) {
		command = wrapWithFetch(command, fetchScript(spec, false, true))
	}
	volumeID, err := r.configuredNetworkVolume(spec)
	if err != nil {
		return ProviderHandle{}, err
	}
	if len(matches) == 1 {
		if err = validateRunPodIntent(matches[0], spec, command, volumeID, hfTokenSecret); err != nil {
			return ProviderHandle{}, err
		}
		return ProviderHandle{ResourceID: name, ExternalKey: spec.ExternalKey}, nil
	}
	if spec.ModelCredential != nil {
		if err = r.ensureModelSecret(ctx, hfTokenSecret, spec.ModelCredential); err != nil {
			return ProviderHandle{}, err
		}
	}
	var volume runPodNetworkVolume
	if volumeID != "" {
		volume, err = r.verifyNetworkVolume(ctx, modelIdentity(spec), volumeID, spec.Region)
		if err != nil {
			return ProviderHandle{}, err
		}
	}
	disk := r.ContainerDiskGiB
	if disk == 0 {
		disk = 100
	}
	if disk < 50 || disk > 2048 {
		return ProviderHandle{}, fmt.Errorf("%w: RunPod container disk must be between 50 and 2048 GiB", ErrInvalidReplicaSpec)
	}
	environment := map[string]string{"INFERCRANE_WORKER_API_KEY": r.WorkerAPIKey, "VLLM_API_KEY": r.WorkerAPIKey}
	for _, variable := range engineVariables {
		environment[variable] = spec.RuntimeEnv[variable]
	}
	if hfTokenSecret != "" {
		if !validRunPodIdentifier(hfTokenSecret) {
			return ProviderHandle{}, fmt.Errorf("%w: RunPod Hugging Face secret name is invalid", ErrInvalidReplicaSpec)
		}
		environment["HF_TOKEN"] = "{{ RUNPOD_SECRET_" + hfTokenSecret + " }}"
	}
	// RunPod REST v2 CreatePodRequest. v2 rejects unknown body fields with 422,
	// so only schema fields are sent. One exact GPU type is placed per create;
	// v2 never substitutes another type. Spot (interruptible) Pods were retired.
	body := map[string]any{
		"name": name, "image": spec.Workload.Image,
		"gpu":        map[string]any{"id": RunPodGPUTypeID(spec.GPU), "count": spec.GPUCount},
		"cloud":      "SECURE",
		"disk":       disk,
		"entrypoint": []string{command[0]}, "cmd": command[1:],
		"ports": []string{strconv.Itoa(spec.Workload.Port) + "/http"},
		"env":   environment,
	}
	dataCenterID := runPodDataCenterConstraint(spec.Region)
	if volume.ID != "" {
		body["mounts"] = map[string]any{"network": []runPodNetworkMount{{VolumeID: volume.ID, Path: runPodArtifactMountPath}}}
		// The attached network volume is already an exact placement constraint:
		// v2 requires every network volume to be in the Pod's data center.
		dataCenterID = ""
		environment["HF_HOME"] = "/workspace/huggingface"
		environment["HUGGINGFACE_HUB_CACHE"] = "/workspace/huggingface/hub"
		environment["INFERCRANE_MODEL_DIR"] = "/workspace/infercrane/model"
		environment["HF_XET_HIGH_PERFORMANCE"] = "1"
	}
	if dataCenterID != "" {
		body["dataCenterIds"] = []string{dataCenterID}
	}
	var created runPodRecord
	if err = r.do(ctx, http.MethodPost, "/pods", body, &created); err != nil {
		// A transport failure may occur after RunPod committed the Pod. Re-list
		// and adopt the deterministic name before surfacing a retryable failure.
		if current, listErr := r.list(ctx); listErr == nil {
			matches = matchingRunPodRecords(current, name)
			if len(matches) == 1 && validateRunPodIntent(matches[0], spec, command, volumeID, hfTokenSecret) == nil {
				return ProviderHandle{ResourceID: name, ExternalKey: spec.ExternalKey}, nil
			}
		}
		return ProviderHandle{}, err
	}
	if created.ID == "" {
		return ProviderHandle{}, fmt.Errorf("%w: RunPod create returned no Pod ID", ErrRequestFailed)
	}
	// Keep the deterministic provider name as InferCrane's durable identity.
	// RunPod allocates the opaque Pod ID only after POST /v2/pods; replacing the
	// pre-persisted name with that ID would violate the control plane's
	// write-once provider identity contract. Observe and delete resolve this
	// stable name to the current opaque Pod ID on every call.
	return ProviderHandle{ResourceID: name, ExternalKey: spec.ExternalKey}, nil
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
		"provider": "runpod-pods", "pod_id": pod.ID, "status": pod.Status,
		"data_center_id": nullableString(pod.DataCenterID), "created_at": nullableString(pod.CreatedAt),
		"started_at": nullableString(pod.StartedAt), "container_disk_gib": pod.DiskGiB,
		"cost_per_hour":             rawNumber(pod.CostPerHour),
		"network_volume":            runPodVolumeEvidence(pod.Mounts.Network),
		"worker_credential_version": r.WorkerCredentialVersion,
	})
	switch strings.ToUpper(pod.Status) {
	case "EXITED", "ERROR", "TERMINATED":
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
		// A Pod removed between the inventory read and this call is already
		// in the requested state.
		if err = r.do(ctx, http.MethodDelete, "/pods/"+url.PathEscape(pod.ID), nil, nil); err != nil && !errors.Is(err, errRunPodNotFound) {
			return err
		}
	}
	return nil
}

// Prefetch adopts an operator-prepared exact-model network volume. Population
// is intentionally an explicit, durable workflow outside replica creation;
// this adapter verifies identity and locality without silently creating spend.
func (r RunPodPods) Prefetch(ctx context.Context, request artifactcache.Request) (artifactcache.Operation, error) {
	if err := request.Validate(); err != nil {
		return artifactcache.Operation{}, artifactcache.Definitive("invalid_prefetch_request", err)
	}
	if strings.ToLower(strings.TrimSpace(r.ArtifactCachePolicy)) == "disabled" {
		return artifactcache.Operation{}, artifactcache.Definitive("artifact_cache_disabled", errors.New("RunPod artifact cache adapter is disabled"))
	}
	if request.Provider != "runpod" {
		return artifactcache.Operation{}, artifactcache.Definitive("invalid_provider_boundary", errors.New("RunPod artifact prefetch provider must be runpod"))
	}
	volumeID, err := runPodVolumeLocation(request.Location)
	if err != nil {
		return artifactcache.Operation{}, artifactcache.Definitive("invalid_cache_location", err)
	}
	if configured := r.NetworkVolumes[request.ModelIdentity]; configured == "" || configured != volumeID {
		return artifactcache.Operation{}, artifactcache.Definitive("artifact_cache_not_configured", errors.New("RunPod network volume is not configured for this immutable model identity"))
	}
	if _, err = r.verifyNetworkVolume(ctx, request.ModelIdentity, volumeID, request.Region); err != nil {
		return artifactcache.Operation{}, artifactcache.Definitive("artifact_volume_verification_failed", err)
	}
	return artifactcache.Operation{ProviderOperationID: volumeID, Status: "succeeded"}, nil
}

func (r RunPodPods) Observe(ctx context.Context, request artifactcache.Request) (artifactcache.Observation, error) {
	if err := request.Validate(); err != nil {
		return artifactcache.Observation{}, err
	}
	volumeID, err := runPodVolumeLocation(request.Location)
	if err != nil || request.Provider != "runpod" || r.NetworkVolumes[request.ModelIdentity] != volumeID {
		return artifactcache.Observation{}, errors.New("RunPod artifact cache request does not match the configured boundary")
	}
	volume, err := r.verifyNetworkVolume(ctx, request.ModelIdentity, volumeID, request.Region)
	if err != nil {
		return artifactcache.Observation{}, err
	}
	observed := time.Now().UTC()
	evidence, _ := json.Marshal(map[string]any{"volume_id": volume.ID, "data_center_id": volume.DataCenterID, "size_gib": volume.Size, "model_identity_digest": modelIdentityDigest(request.ModelIdentity), "mount": "/workspace", "population_state": "operator_attested"})
	return artifactcache.Observation{State: "present", Source: "runpod-network-volume", EvidenceJSON: string(evidence), ObservedAt: observed, ExpiresAt: observed.Add(5 * time.Minute)}, nil
}

func runPodVolumeLocation(location string) (string, error) {
	const prefix = "runpod-volume://"
	if !strings.HasPrefix(location, prefix) {
		return "", errors.New("RunPod artifact cache location must use runpod-volume://ID")
	}
	volumeID := strings.TrimPrefix(location, prefix)
	if !validRunPodIdentifier(volumeID) {
		return "", errors.New("RunPod artifact cache location contains an invalid volume ID")
	}
	return volumeID, nil
}

func (r RunPodPods) list(ctx context.Context) ([]runPodRecord, error) {
	return runPodListAll[runPodRecord](ctx, r.do, "/pods", "pods")
}

// errRunPodNotFound marks an HTTP 404 from the Pod API. Callers that treat an
// absent resource as success test for it explicitly.
var errRunPodNotFound = errors.New("RunPod resource not found")

func (r RunPodPods) do(ctx context.Context, method, path string, body, output any) error {
	providerAPIKey := strings.TrimSpace(r.APIKey)
	base := runPodBaseURL(r.BaseURL)
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
	request.Header.Set("Authorization", "Bearer "+providerAPIKey)
	// RunPod's edge may reject generic Go HTTP clients even when the exact
	// bearer credential is valid. Identify this integration explicitly so the
	// control-plane lifecycle and an operator's direct request take the same
	// authenticated path through their edge.
	request.Header.Set("User-Agent", runPodPodsUserAgent)
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
		message := strings.ToLower(safeRunPodDiagnostic(runPodProblemText(payload), providerAPIKey))
		// Classification follows RunPod REST v2's documented create semantics
		// (https://docs.runpod.io/api-reference-v2/pods/create-a-pod).
		switch response.StatusCode {
		case http.StatusUnauthorized, http.StatusForbidden:
			return fmt.Errorf("%w: RunPod rejected the provider credential (HTTP %d)", ErrProviderAuthorization, response.StatusCode)
		case http.StatusPaymentRequired:
			return fmt.Errorf("%w: RunPod reported insufficient account balance (HTTP 402)", ErrProviderQuota)
		case http.StatusNotFound:
			return fmt.Errorf("%w: RunPod Pod API returned HTTP 404", errRunPodNotFound)
		case http.StatusUnprocessableEntity:
			// v2 returns 422 when the body does not match the published
			// contract. Retrying the same durable intent cannot heal it and
			// would keep a customer operation alive for its whole retry budget.
			return fmt.Errorf("%w: RunPod rejected the Pod specification (HTTP 422)", ErrInvalidReplicaSpec)
		case http.StatusBadRequest:
			// v2 returns 400 both for a broken cross-field rule and for a GPU
			// and data center combination that could not be placed. The create
			// body is fixed by contract tests, so a 400 here is placement.
			return fmt.Errorf("%w: RunPod could not place the requested Pod (HTTP 400)", ErrProviderCapacity)
		case http.StatusConflict, http.StatusTooManyRequests:
			return fmt.Errorf("%w: RunPod could not allocate the requested Pod (HTTP %d)", ErrProviderCapacity, response.StatusCode)
		default:
			if strings.Contains(message, "capacity") || strings.Contains(message, "stock") {
				return fmt.Errorf("%w: RunPod could not allocate the requested Pod (HTTP %d)", ErrProviderCapacity, response.StatusCode)
			}
			return fmt.Errorf("%w: RunPod Pod API request failed (HTTP %d)", ErrRequestFailed, response.StatusCode)
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
		if pod.Name == name && strings.ToUpper(pod.Status) != "TERMINATED" {
			matches = append(matches, pod)
		}
	}
	return matches
}

func validateRunPodIntent(pod runPodRecord, spec ReplicaSpec, command []string, volumeID, hfTokenSecret string) error {
	gpuMismatch := pod.GPU == nil || pod.GPU.Count != spec.GPUCount || (pod.GPU.ID != "" && pod.GPU.ID != RunPodGPUTypeID(spec.GPU))
	credentialMismatch := len(pod.Environment) > 0 && (pod.Environment["INFERCRANE_WORKER_API_KEY"] == "" || pod.Environment["VLLM_API_KEY"] == "")
	secretMismatch := hfTokenSecret != "" && len(pod.Environment) > 0 && pod.Environment["HF_TOKEN"] != "{{ RUNPOD_SECRET_"+hfTokenSecret+" }}"
	engineMismatch := len(pod.Environment) > 0 && !engineEnvironmentMatches(pod.Environment, spec.RuntimeEnv)
	requestedDataCenterID := runPodDataCenterConstraint(spec.Region)
	regionMismatch := requestedDataCenterID != "" && pod.DataCenterID != "" && pod.DataCenterID != requestedDataCenterID
	if pod.Image != spec.Workload.Image || gpuMismatch || credentialMismatch || secretMismatch || engineMismatch || pod.networkVolumeID() != volumeID || regionMismatch || !equalStrings(pod.Entrypoint, command[:1]) || !equalStrings(pod.Cmd, command[1:]) {
		return fmt.Errorf("%w: existing RunPod Pod does not match immutable workload intent", ErrInvalidReplicaSpec)
	}
	return nil
}

func validRunPodIdentifier(value string) bool {
	if len(value) < 1 || len(value) > 128 {
		return false
	}
	for _, char := range value {
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9') || char == '-' || char == '_' {
			continue
		}
		return false
	}
	return true
}

func (r RunPodPods) configuredNetworkVolume(spec ReplicaSpec) (string, error) {
	policy := strings.ToLower(strings.TrimSpace(r.ArtifactCachePolicy))
	if policy == "" {
		policy = "prefer"
	}
	if policy != "disabled" && policy != "prefer" && policy != "required" {
		return "", fmt.Errorf("%w: RunPod artifact cache policy must be disabled, prefer, or required", ErrInvalidReplicaSpec)
	}
	if policy == "disabled" {
		return "", nil
	}
	identity := modelIdentity(spec)
	volumeID := strings.TrimSpace(r.NetworkVolumes[identity])
	if volumeID == "" && policy == "required" {
		return "", fmt.Errorf("%w: RunPod artifact cache requires a network volume for immutable model %q", ErrInvalidReplicaSpec, identity)
	}
	return volumeID, nil
}

func (r RunPodPods) verifyNetworkVolume(ctx context.Context, identity, volumeID, region string) (runPodNetworkVolume, error) {
	volumes, err := runPodListAll[runPodNetworkVolume](ctx, r.do, "/network-volumes", "networkVolumes")
	if err != nil {
		return runPodNetworkVolume{}, fmt.Errorf("verify RunPod network volume: %w", err)
	}
	expectedName := runPodArtifactVolumeName(identity)
	for _, volume := range volumes {
		if volume.ID != volumeID {
			continue
		}
		if volume.Name != expectedName || volume.Size < 1 || volume.DataCenterID == "" {
			return runPodNetworkVolume{}, fmt.Errorf("%w: RunPod network volume %s must be named %q, have positive size, and declare a data center", ErrInvalidReplicaSpec, volumeID, expectedName)
		}
		requestedDataCenterID := runPodDataCenterConstraint(region)
		if requestedDataCenterID != "" && requestedDataCenterID != volume.DataCenterID {
			return runPodNetworkVolume{}, fmt.Errorf("%w: RunPod network volume %s is in %s, not requested region %s", ErrInvalidReplicaSpec, volumeID, volume.DataCenterID, requestedDataCenterID)
		}
		return volume, nil
	}
	return runPodNetworkVolume{}, fmt.Errorf("%w: configured RunPod network volume %s was not found", ErrInvalidReplicaSpec, volumeID)
}

func runPodArtifactVolumeName(identity string) string {
	hexDigest := strings.TrimPrefix(modelIdentityDigest(identity), "sha256:")
	return "infercrane-artifact-" + hexDigest[:20]
}

// runPodVolumeEvidence reports the attached network volume as the Pod lists
// it. RunPod v2 Pod reads carry only the volume ID and mount path; name, size,
// and data center were verified before create and are not repeated here.
func runPodVolumeEvidence(mounts []runPodNetworkMount) any {
	if len(mounts) == 0 {
		return nil
	}
	return map[string]any{"id": mounts[0].VolumeID, "mount": mounts[0].Path, "state": "attached"}
}

func nullableString(value string) any {
	if value == "" {
		return nil
	}
	return value
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
