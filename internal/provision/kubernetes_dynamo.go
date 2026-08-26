package provision

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/infercrane/infercrane/internal/runtimecontract"
	"github.com/infercrane/infercrane/internal/servingcontract"
)

const (
	dynamoFieldManager = "infercrane-dynamo-v1"
	dynamoAPIVersion   = "nvidia.com/v1beta1"
	dynamoResource     = "dynamographdeployments.nvidia.com"
)

// KubernetesDynamo owns exactly one parent DynamoGraphDeployment for one
// InferCrane serving-plan revision. The Dynamo operator owns every child and
// all routing/scaling inside the graph.
type KubernetesDynamo struct {
	Binary, Context, Namespace, ServiceAccount    string
	VLLMImageDigest, VLLMRuntimeVersion           string
	SGLangImageDigest, SGLangRuntimeVersion       string
	ModelSecretName, GPUResource, GPUProductLabel string
	Runner                                        CommandRunner
}

func (k KubernetesDynamo) Handle(externalKey string) ProviderHandle {
	name := k.resourceName(externalKey)
	return ProviderHandle{ResourceID: "kubernetes:dynamo:" + k.Namespace + "/" + name, ExternalKey: externalKey}
}

func (k KubernetesDynamo) Check(ctx context.Context) error {
	if err := k.validateConfig(); err != nil {
		return err
	}
	if _, err := k.run(ctx, false, "version", "-o", "json"); err != nil {
		return fmt.Errorf("probe Kubernetes API: %w", err)
	}
	output, err := k.run(ctx, false, "api-resources", "--api-group=nvidia.com", "-o", "name")
	if err != nil {
		return fmt.Errorf("discover Dynamo API: %w", err)
	}
	if !strings.Contains(string(output), dynamoResource) {
		return errors.New("DynamoGraphDeployment nvidia.com/v1beta1 API is not installed")
	}
	for _, verb := range []string{"get", "list", "create", "patch", "delete"} {
		allowed, authErr := k.run(ctx, true, "auth", "can-i", verb, dynamoResource)
		if authErr != nil || strings.TrimSpace(string(allowed)) != "yes" {
			return fmt.Errorf("Kubernetes RBAC denies %s %s in namespace %s", verb, dynamoResource, k.Namespace)
		}
	}
	if _, err = k.run(ctx, true, "get", dynamoResource, "--chunk-size=1", "-o", "name"); err != nil {
		return fmt.Errorf("list DynamoGraphDeployments in namespace %s: %w", k.Namespace, err)
	}
	return nil
}

func (k KubernetesDynamo) EnsureReplica(ctx context.Context, spec ReplicaSpec) (ProviderHandle, error) {
	if err := k.validate(spec); err != nil {
		return ProviderHandle{}, fmt.Errorf("%w: %v", ErrInvalidReplicaSpec, err)
	}
	handle := k.Handle(spec.ExternalKey)
	existing, err := k.object(ctx, spec.ExternalKey)
	if err != nil {
		return ProviderHandle{}, err
	}
	intentHash, err := k.intentHash(spec)
	if err != nil {
		return ProviderHandle{}, err
	}
	if existing != nil {
		persisted := existing.Metadata.Annotations["infercrane.dev/intent-hash"]
		if persisted == "" || persisted != intentHash {
			return ProviderHandle{}, errors.New("Dynamo graph exists for the durable key with a different immutable intent")
		}
	}
	manifest, err := k.manifest(spec)
	if err != nil {
		return ProviderHandle{}, err
	}
	path, cleanup, err := writeKubernetesManifest(manifest)
	if err != nil {
		return ProviderHandle{}, err
	}
	defer cleanup()
	apply := []string{"apply", "--server-side", "--field-manager=" + dynamoFieldManager, "--validate=strict", "-f", path}
	if _, err = k.run(ctx, true, append(apply, "--dry-run=server")...); err != nil {
		return ProviderHandle{}, fmt.Errorf("server-side validate Dynamo graph: %w", err)
	}
	if _, err = k.run(ctx, true, apply...); err != nil {
		return ProviderHandle{}, fmt.Errorf("server-side apply Dynamo graph: %w", err)
	}
	return handle, nil
}

func (k KubernetesDynamo) ObserveReplica(ctx context.Context, handle ProviderHandle, port int) (Observation, error) {
	if handle.ExternalKey == "" {
		return Observation{}, errors.New("Dynamo provider handle requires an external key")
	}
	item, err := k.object(ctx, handle.ExternalKey)
	if err != nil || item == nil {
		return Observation{}, err
	}
	return k.observation(handle.ExternalKey, *item, port)
}

func (k KubernetesDynamo) DeleteReplica(ctx context.Context, handle ProviderHandle) error {
	if handle.ExternalKey == "" {
		return errors.New("Dynamo provider handle requires an external key")
	}
	item, err := k.object(ctx, handle.ExternalKey)
	if err != nil || item == nil {
		return err
	}
	if !k.owned(*item, handle.ExternalKey) || item.Metadata.Name != k.resourceName(handle.ExternalKey) || item.Kind != "DynamoGraphDeployment" {
		return errors.New("refusing to delete Dynamo graph without exact InferCrane ownership")
	}
	if _, err = k.run(ctx, true, "delete", "dynamographdeployment.nvidia.com/"+item.Metadata.Name, "--ignore-not-found=true", "--wait=false"); err != nil {
		return fmt.Errorf("delete Dynamo graph: %w", err)
	}
	return nil
}

func (k KubernetesDynamo) Inventory(ctx context.Context, filter InventoryFilter) ([]Resource, error) {
	if err := k.validateConfig(); err != nil {
		return nil, err
	}
	output, err := k.run(ctx, true, "get", dynamoResource, "-l", "app.kubernetes.io/managed-by=infercrane,infercrane.dev/serving-backend=dynamo", "-o", "json")
	if err != nil {
		return nil, fmt.Errorf("inventory Dynamo graphs: %w", err)
	}
	objects, err := decodeKubernetesObjects(output)
	if err != nil {
		return nil, err
	}
	resources := make([]Resource, 0, len(objects))
	for _, item := range objects {
		key := item.Metadata.Annotations["infercrane.dev/external-key"]
		if key == "" || (filter.Prefix != "" && !strings.HasPrefix(key, filter.Prefix)) || !k.owned(item, key) {
			continue
		}
		observation, observeErr := k.observation(key, item, 8000)
		if observeErr != nil {
			return nil, observeErr
		}
		resources = append(resources, Resource{ID: k.Handle(key).ResourceID, ExternalKey: key, State: observation.State, Endpoint: observation.Endpoint})
	}
	sort.Slice(resources, func(i, j int) bool { return resources[i].ExternalKey < resources[j].ExternalKey })
	return resources, nil
}

func (k KubernetesDynamo) object(ctx context.Context, externalKey string) (*kubernetesObject, error) {
	name := k.resourceName(externalKey)
	output, err := k.run(ctx, true, "get", "dynamographdeployment.nvidia.com/"+name, "--ignore-not-found=true", "-o", "json")
	if err != nil {
		return nil, fmt.Errorf("inspect Dynamo graph: %w", err)
	}
	objects, err := decodeKubernetesObjects(output)
	if err != nil {
		return nil, err
	}
	if len(objects) == 0 {
		return nil, nil
	}
	if len(objects) != 1 || objects[0].Kind != "DynamoGraphDeployment" || objects[0].Metadata.Name != name {
		return nil, errors.New("Dynamo inventory returned an unexpected owner set")
	}
	if !k.owned(objects[0], externalKey) {
		return nil, errors.New("Dynamo graph conflicts with durable key ownership")
	}
	return &objects[0], nil
}

func (k KubernetesDynamo) observation(externalKey string, item kubernetesObject, port int) (Observation, error) {
	if port == 0 {
		port = 8000
	}
	state, endpoint := "provisioning", ""
	if generationObserved(item) {
		switch strings.ToLower(item.Status.State) {
		case "successful":
			state = "ready"
		case "failed":
			state = "failed"
		}
		if state != "failed" && conditionTrue(item.Status.Conditions, "Ready") {
			state = "ready"
		}
	}
	if state == "ready" {
		endpoint = fmt.Sprintf("http://%s-frontend.%s.svc:%d", k.resourceName(externalKey), k.Namespace, port)
	}
	details, _ := json.Marshal(k.details(item))
	return Observation{Exists: true, State: state, Endpoint: endpoint, Details: string(details)}, nil
}

func (k KubernetesDynamo) manifest(spec ReplicaSpec) ([]byte, error) {
	name, port := k.resourceName(spec.ExternalKey), spec.Port
	if port == 0 {
		port = 8000
	}
	topology := spec.Serving.Normalize()
	runtimeImage, runtimeVersion, err := k.runtime(spec.Runtime)
	if err != nil {
		return nil, err
	}
	intentHash, err := k.intentHash(spec)
	if err != nil {
		return nil, err
	}
	labels := map[string]any{
		"app.kubernetes.io/name":           "infercrane-dynamo",
		"app.kubernetes.io/instance":       name,
		"app.kubernetes.io/managed-by":     "infercrane",
		"infercrane.dev/external-key-hash": k.externalKeyHash(spec.ExternalKey),
		"infercrane.dev/serving-backend":   "dynamo",
	}
	metadata := map[string]any{"name": name, "namespace": k.Namespace, "labels": labels, "annotations": map[string]any{
		"infercrane.dev/external-key": spec.ExternalKey, "infercrane.dev/provider-contract": "infercrane.provider/v1", "infercrane.dev/serving-contract": servingcontract.SchemaV1, "infercrane.dev/intent-hash": intentHash,
	}}
	frontendEnv := []any{map[string]any{"name": "DYN_HTTP_PORT", "value": strconv.Itoa(port)}}
	if topology.Routing == servingcontract.RoutingKVAware {
		frontendEnv = append(frontendEnv, map[string]any{"name": "DYN_ROUTER_MODE", "value": "kv"})
	}
	frontend := map[string]any{
		"name":                   "Frontend",
		"type":                   "frontend",
		"replicas":               1,
		"runtimeVersionOverride": runtimeVersion,
		"podTemplate": map[string]any{
			"spec": map[string]any{
				"serviceAccountName": k.ServiceAccount,
				"containers": []any{
					map[string]any{
						"name":            "main",
						"image":           runtimeImage,
						"imagePullPolicy": "IfNotPresent",
						"command":         []any{"python3", "-m", "dynamo.frontend"},
						"args":            []any{"--http-port", strconv.Itoa(port)},
						"env":             frontendEnv,
						"securityContext": map[string]any{"allowPrivilegeEscalation": false},
					},
				},
			},
		},
	}
	components := []any{frontend}
	if topology.Mode == servingcontract.ModeAggregated {
		worker, workerErr := k.workerComponent(spec, topology, "Worker", "worker", topology.Worker, port)
		if workerErr != nil {
			return nil, workerErr
		}
		components = append(components, worker)
	} else {
		prefill, prefillErr := k.workerComponent(spec, topology, "Prefill", "prefill", topology.Prefill, port)
		if prefillErr != nil {
			return nil, prefillErr
		}
		decode, decodeErr := k.workerComponent(spec, topology, "Decode", "decode", topology.Decode, port)
		if decodeErr != nil {
			return nil, decodeErr
		}
		components = append(components, prefill, decode)
	}
	return json.Marshal(map[string]any{"apiVersion": dynamoAPIVersion, "kind": "DynamoGraphDeployment", "metadata": metadata, "spec": map[string]any{"backendFramework": spec.Runtime, "components": components}})
}

func (k KubernetesDynamo) workerComponent(spec ReplicaSpec, topology servingcontract.Topology, name, componentType string, pool servingcontract.Pool, _ int) (map[string]any, error) {
	runtimeImage, runtimeVersion, err := k.runtime(spec.Runtime)
	if err != nil {
		return nil, err
	}
	var args []string
	command := []any{"python3", "-m", "dynamo." + spec.Runtime}
	switch spec.Runtime {
	case "vllm":
		args = append(args, "--model", spec.Model, "--served-model-name", spec.Model)
		if spec.ModelRevision != "" {
			args = append(args, "--revision", spec.ModelRevision)
		}
		args = append(args, "--tensor-parallel-size", strconv.Itoa(pool.TensorParallelism))
		if topology.Mode == servingcontract.ModeDisaggregated {
			args = append(args, "--disaggregation-mode", componentType, "--kv-transfer-config", `{"kv_connector":"NixlConnector","kv_role":"kv_both"}`)
		}
		if topology.Routing == servingcontract.RoutingKVAware {
			args = append(args, "--kv-events-config", `{"publisher":"zmq","topic":"kv-events","endpoint":"tcp://*:20080","enable_kv_cache_events":true}`)
		}
	case "sglang":
		args = append(args, "--model-path", spec.Model, "--served-model-name", spec.Model, "--tp", strconv.Itoa(pool.TensorParallelism), "--skip-tokenizer-init")
		if spec.ModelRevision != "" {
			args = append(args, "--revision", spec.ModelRevision)
		}
		if topology.Mode == servingcontract.ModeDisaggregated {
			args = append(args, "--disaggregation-mode", componentType, "--disaggregation-transfer-backend", "nixl", "--disaggregation-bootstrap-port", "12345", "--host", "0.0.0.0")
		}
		if topology.Routing == servingcontract.RoutingKVAware {
			args = append(args, "--kv-events-config", `{"publisher":"zmq","topic":"kv-events","endpoint":"tcp://*:5557"}`)
		}
	default:
		return nil, fmt.Errorf("Dynamo runtime %q is unsupported", spec.Runtime)
	}
	args = append(args, spec.RuntimeArgs...)
	env := []any{}
	container := map[string]any{
		"name": "main", "image": runtimeImage, "imagePullPolicy": "IfNotPresent", "command": command, "args": stringSliceAny(args),
		"resources":       map[string]any{"limits": map[string]any{k.GPUResource: strconv.Itoa(pool.TensorParallelism)}, "requests": map[string]any{k.GPUResource: strconv.Itoa(pool.TensorParallelism)}},
		"securityContext": map[string]any{"allowPrivilegeEscalation": false},
	}
	if k.ModelSecretName != "" {
		container["envFrom"] = []any{map[string]any{"secretRef": map[string]any{"name": k.ModelSecretName}}}
	}
	podSpec := map[string]any{"serviceAccountName": k.ServiceAccount, "nodeSelector": map[string]any{k.GPUProductLabel: spec.GPU}, "containers": []any{container}}
	if topology.Cache.Backend != servingcontract.CacheNone {
		if topology.Cache.Backend != servingcontract.CacheKVBM || spec.Runtime != "vllm" {
			return nil, fmt.Errorf("Dynamo cache backend %q is registered but not executable by the current adapter", topology.Cache.Backend)
		}
		args = append(args, "--kv-transfer-config", `{"kv_connector":"DynamoConnector","kv_connector_module_path":"kvbm.vllm_integration.connector","kv_role":"kv_both"}`)
		container["args"] = stringSliceAny(args)
		env = append(env, map[string]any{"name": "DYN_KVBM_CPU_CACHE_GB", "value": strconv.Itoa(topology.Cache.HostGiB)})
		if topology.Cache.DiskGiB > 0 {
			env = append(env, map[string]any{"name": "DYN_KVBM_DISK_CACHE_GB", "value": strconv.Itoa(topology.Cache.DiskGiB)}, map[string]any{"name": "DYN_KVBM_DISK_CACHE_DIR", "value": "/var/cache/infercrane/kv"})
			container["volumeMounts"] = []any{map[string]any{"name": "kv-cache", "mountPath": "/var/cache/infercrane/kv"}}
			podSpec["volumes"] = []any{map[string]any{"name": "kv-cache", "persistentVolumeClaim": map[string]any{"claimName": topology.Cache.StorageClaim}}}
		}
		if topology.Cache.Metrics {
			env = append(env, map[string]any{"name": "DYN_KVBM_METRICS", "value": "true"})
		}
		container["env"] = env
		resources := container["resources"].(map[string]any)
		resources["requests"].(map[string]any)["memory"] = strconv.Itoa(topology.Cache.MemoryGiB) + "Gi"
		resources["limits"].(map[string]any)["memory"] = strconv.Itoa(topology.Cache.MemoryGiB) + "Gi"
	}
	return map[string]any{"name": name, "type": componentType, "replicas": pool.Replicas, "runtimeVersionOverride": runtimeVersion, "podTemplate": map[string]any{"spec": podSpec}}, nil
}

func (k KubernetesDynamo) validate(spec ReplicaSpec) error {
	if err := k.validateConfig(); err != nil {
		return err
	}
	if _, _, err := k.runtime(spec.Runtime); err != nil {
		return err
	}
	if spec.ExternalKey == "" || len(spec.ExternalKey) > 253 || spec.Model == "" || spec.Cloud != "kubernetes" || spec.GPU == "" {
		return errors.New("Dynamo graph requires bounded external key, model, cloud kubernetes, and GPU")
	}
	if !spec.Workload.Empty() {
		return errors.New("Dynamo graph uses its qualified runtime image and does not accept a portable workload override")
	}
	if err := spec.Serving.Validate(spec.Runtime, spec.Cloud, "kubernetes-dynamo", 1, 1); err != nil {
		return err
	}
	if spec.Serving.Autoscaling.Owner == servingcontract.AutoscalingDynamoPlanner {
		return errors.New("Dynamo Planner intent is registered but not executable until DGDSA/Planner ownership is qualified")
	}
	return nil
}

func (k KubernetesDynamo) validateConfig() error {
	if k.Context == "" || k.Namespace == "" || k.ServiceAccount == "" || k.GPUResource == "" || k.GPUProductLabel == "" {
		return errors.New("Dynamo Kubernetes context, namespace, service account, GPU resource, and GPU product label are required")
	}
	configured := 0
	for _, runtime := range []string{"vllm", "sglang"} {
		image, version := k.runtimeConfig(runtime)
		if image == "" && version == "" {
			continue
		}
		configured++
		if _, _, err := k.runtime(runtime); err != nil {
			return err
		}
	}
	if configured == 0 {
		return errors.New("Dynamo requires at least one complete vLLM or SGLang image digest and runtime version pair")
	}
	for _, value := range []string{k.Namespace, k.ServiceAccount} {
		if !validKubernetesDNSLabel(value) {
			return fmt.Errorf("invalid Kubernetes identifier %q", value)
		}
	}
	if k.ModelSecretName != "" && !validKubernetesDNSLabel(k.ModelSecretName) {
		return fmt.Errorf("invalid Kubernetes Secret name %q", k.ModelSecretName)
	}
	return nil
}

func (k KubernetesDynamo) intentHash(spec ReplicaSpec) (string, error) {
	spec.RequestID = ""
	runtimeImage, runtimeVersion, err := k.runtime(spec.Runtime)
	if err != nil {
		return "", err
	}
	body, err := json.Marshal(struct {
		Spec                                                                                                         ReplicaSpec
		Namespace, ServiceAccount, RuntimeImageDigest, RuntimeVersion, ModelSecretName, GPUResource, GPUProductLabel string
	}{spec, k.Namespace, k.ServiceAccount, runtimeImage, runtimeVersion, k.ModelSecretName, k.GPUResource, k.GPUProductLabel})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(body)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func (k KubernetesDynamo) runtime(runtime string) (string, string, error) {
	image, version := k.runtimeConfig(runtime)
	switch runtime {
	case "vllm":
	case "sglang":
	default:
		return "", "", fmt.Errorf("Dynamo runtime %q is unsupported", runtime)
	}
	if image == "" || version == "" {
		return "", "", fmt.Errorf("Dynamo %s runtime image and version are not configured", runtime)
	}
	if runtimecontract.ValidateImage(image) != nil {
		return "", "", fmt.Errorf("Dynamo %s runtime image must be pinned by sha256 digest", runtime)
	}
	if !validDynamoRuntimeVersion(version) {
		return "", "", fmt.Errorf("Dynamo %s runtime version must be numeric MAJOR.MINOR.PATCH", runtime)
	}
	return image, version, nil
}

func (k KubernetesDynamo) runtimeConfig(runtime string) (string, string) {
	switch runtime {
	case "vllm":
		return k.VLLMImageDigest, k.VLLMRuntimeVersion
	case "sglang":
		return k.SGLangImageDigest, k.SGLangRuntimeVersion
	default:
		return "", ""
	}
}

func validDynamoRuntimeVersion(value string) bool {
	parts := strings.Split(value, ".")
	if len(parts) != 3 {
		return false
	}
	for _, part := range parts {
		if part == "" || (len(part) > 1 && part[0] == '0') {
			return false
		}
		if _, err := strconv.ParseUint(part, 10, 16); err != nil {
			return false
		}
	}
	return true
}

func (k KubernetesDynamo) details(item kubernetesObject) map[string]any {
	conditions := make([]map[string]string, 0, len(item.Status.Conditions))
	for _, condition := range item.Status.Conditions {
		conditions = append(conditions, map[string]string{"type": condition.Type, "status": condition.Status, "reason": condition.Reason, "message": boundedKubernetesDetail(condition.Message)})
	}
	components := make(map[string]any, len(item.Status.Components))
	for name, component := range item.Status.Components {
		components[name] = map[string]int{"replicas": component.Replicas, "ready_replicas": component.ReadyReplicas, "available_replicas": component.AvailableReplicas, "updated_replicas": component.UpdatedReplicas, "scheduled_replicas": component.ScheduledReplicas}
	}
	return map[string]any{"context": k.Context, "namespace": k.Namespace, "workload_api": "dynamo", "api_version": item.APIVersion, "name": item.Metadata.Name, "uid": item.Metadata.UID, "generation": item.Metadata.Generation, "observed_generation": item.Status.ObservedGeneration, "state": item.Status.State, "conditions": conditions, "components": components}
}

func (k KubernetesDynamo) owned(item kubernetesObject, externalKey string) bool {
	return item.Metadata.Namespace == k.Namespace && item.Metadata.Labels["app.kubernetes.io/managed-by"] == "infercrane" && item.Metadata.Labels["infercrane.dev/serving-backend"] == "dynamo" && item.Metadata.Labels["infercrane.dev/external-key-hash"] == k.externalKeyHash(externalKey) && item.Metadata.Annotations["infercrane.dev/external-key"] == externalKey
}

func (k KubernetesDynamo) resourceName(externalKey string) string {
	return "infercrane-" + k.externalKeyHash(externalKey)
}
func (k KubernetesDynamo) externalKeyHash(externalKey string) string {
	sum := sha256.Sum256([]byte(externalKey))
	return hex.EncodeToString(sum[:12])
}

func (k KubernetesDynamo) run(ctx context.Context, namespaced bool, args ...string) ([]byte, error) {
	base := Kubernetes{Binary: k.Binary, Context: k.Context, Namespace: k.Namespace, Runner: k.Runner}
	return base.run(ctx, namespaced, args...)
}

func stringSliceAny(values []string) []any {
	out := make([]any, len(values))
	for i := range values {
		out[i] = values[i]
	}
	return out
}

func boundedKubernetesDetail(value string) string {
	value = strings.TrimSpace(value)
	if len(value) > 1024 {
		return value[:1024]
	}
	return value
}
