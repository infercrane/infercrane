package provision

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"

	"github.com/infercrane/infercrane/internal/runtimecontract"
)

const kubernetesFieldManager = "infercrane-provider-v1"

// Kubernetes is a narrow, namespaced Provider Contract adapter. Kubernetes
// owns scheduling and child Pods; InferCrane owns only its deterministic
// Deployment/Service set or one standard KServe InferenceService.
type Kubernetes struct {
	Binary, Context, Namespace, WorkloadAPI           string
	ServiceAccount, WorkerSecretName, WorkerSecretKey string
	ImageDigest, GPUResource, GPUProductLabel         string
	Runner                                            CommandRunner
}

type kubernetesObject struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Metadata   struct {
		Name        string            `json:"name"`
		Namespace   string            `json:"namespace"`
		UID         string            `json:"uid"`
		Generation  int64             `json:"generation"`
		Labels      map[string]string `json:"labels"`
		Annotations map[string]string `json:"annotations"`
	} `json:"metadata"`
	Spec struct {
		ClusterIP string `json:"clusterIP"`
	} `json:"spec"`
	Status struct {
		ObservedGeneration int64  `json:"observedGeneration"`
		AvailableReplicas  int    `json:"availableReplicas"`
		ReadyReplicas      int    `json:"readyReplicas"`
		URL                string `json:"url"`
		Address            struct {
			URL string `json:"url"`
		} `json:"address"`
		Conditions []struct {
			Type, Status, Reason, Message string
		} `json:"conditions"`
	} `json:"status"`
}

func (k Kubernetes) Handle(externalKey string) ProviderHandle {
	return ProviderHandle{ResourceID: k.resourceID(k.resourceName(externalKey)), ExternalKey: externalKey}
}

func (k Kubernetes) Check(ctx context.Context) error {
	if err := k.validateConfig(); err != nil {
		return err
	}
	if _, err := k.run(ctx, false, "version", "-o", "json"); err != nil {
		return fmt.Errorf("probe Kubernetes API: %w", err)
	}
	resources := []string{"deployments.apps", "services"}
	if k.WorkloadAPI == "kserve" {
		resources = []string{"inferenceservices.serving.kserve.io"}
		output, err := k.run(ctx, false, "api-resources", "--api-group=serving.kserve.io", "-o", "name")
		if err != nil {
			return fmt.Errorf("discover KServe API: %w", err)
		}
		if !strings.Contains(string(output), "inferenceservices.serving.kserve.io") {
			return errors.New("KServe InferenceService API is not installed")
		}
	}
	for _, verb := range []string{"get", "list", "create", "patch", "delete"} {
		for _, resource := range resources {
			output, err := k.run(ctx, true, "auth", "can-i", verb, resource)
			if err != nil || strings.TrimSpace(string(output)) != "yes" {
				return fmt.Errorf("Kubernetes RBAC denies %s %s in namespace %s", verb, resource, k.Namespace)
			}
		}
	}
	if _, err := k.run(ctx, true, "get", resources[0], "--chunk-size=1", "-o", "name"); err != nil {
		return fmt.Errorf("list Kubernetes workload API in namespace %s: %w", k.Namespace, err)
	}
	return nil
}

func (k Kubernetes) EnsureReplica(ctx context.Context, spec ReplicaSpec) (ProviderHandle, error) {
	if err := k.validate(spec); err != nil {
		return ProviderHandle{}, fmt.Errorf("%w: %v", ErrInvalidReplicaSpec, err)
	}
	handle := k.Handle(spec.ExternalKey)
	if _, err := k.objects(ctx, spec.ExternalKey); err != nil {
		return ProviderHandle{}, err
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
	apply := []string{"apply", "--server-side", "--field-manager=" + kubernetesFieldManager, "--validate=strict", "-f", path}
	if _, err = k.run(ctx, true, append(apply, "--dry-run=server")...); err != nil {
		return ProviderHandle{}, fmt.Errorf("server-side validate Kubernetes workload: %w", err)
	}
	if _, err = k.run(ctx, true, apply...); err != nil {
		return ProviderHandle{}, fmt.Errorf("server-side apply Kubernetes workload: %w", err)
	}
	return handle, nil
}

func (k Kubernetes) ObserveReplica(ctx context.Context, handle ProviderHandle, port int) (Observation, error) {
	if handle.ExternalKey == "" {
		return Observation{}, errors.New("Kubernetes provider handle requires an external key")
	}
	objects, err := k.objects(ctx, handle.ExternalKey)
	if err != nil || len(objects) == 0 {
		return Observation{}, err
	}
	if port == 0 {
		port = 8000
	}
	state, endpoint := "provisioning", ""
	if k.WorkloadAPI == "kserve" {
		item := objects[0]
		if generationObserved(item) && conditionTrue(item.Status.Conditions, "Ready") {
			state = "ready"
			endpoint = item.Status.Address.URL
			if endpoint == "" {
				endpoint = item.Status.URL
			}
		}
	} else if k.ownerSetComplete(objects) {
		for _, item := range objects {
			if item.Kind == "Deployment" && generationObserved(item) && item.Status.AvailableReplicas > 0 {
				state = "ready"
				endpoint = fmt.Sprintf("http://%s.%s.svc:%d", k.resourceName(handle.ExternalKey), k.Namespace, port)
			}
		}
	}
	details, _ := json.Marshal(k.nativeDetails(objects))
	return Observation{Exists: true, State: state, Endpoint: endpoint, Details: string(details)}, nil
}

func (k Kubernetes) DeleteReplica(ctx context.Context, handle ProviderHandle) error {
	if handle.ExternalKey == "" {
		return errors.New("Kubernetes provider handle requires an external key")
	}
	objects, err := k.objects(ctx, handle.ExternalKey)
	if err != nil || len(objects) == 0 {
		return err
	}
	name := k.resourceName(handle.ExternalKey)
	resources := make([]string, 0, len(objects))
	for _, item := range objects {
		if !k.owned(item, handle.ExternalKey) || item.Metadata.Name != name {
			return errors.New("refusing to delete Kubernetes resource without exact InferCrane ownership")
		}
		switch item.Kind {
		case "Deployment":
			resources = append(resources, "deployment/"+name)
		case "Service":
			resources = append(resources, "service/"+name)
		case "InferenceService":
			resources = append(resources, "inferenceservice.serving.kserve.io/"+name)
		default:
			return fmt.Errorf("refusing to delete unexpected Kubernetes kind %q", item.Kind)
		}
	}
	sort.Strings(resources)
	args := append([]string{"delete"}, resources...)
	args = append(args, "--ignore-not-found=true", "--wait=false")
	if _, err = k.run(ctx, true, args...); err != nil {
		return fmt.Errorf("delete Kubernetes workload: %w", err)
	}
	return nil
}

func (k Kubernetes) Inventory(ctx context.Context, filter InventoryFilter) ([]Resource, error) {
	if err := k.validateConfig(); err != nil {
		return nil, err
	}
	selector := "app.kubernetes.io/managed-by=infercrane"
	var args []string
	if k.WorkloadAPI == "kserve" {
		args = []string{"get", "inferenceservices.serving.kserve.io", "-l", selector, "-o", "json"}
	} else {
		args = []string{"get", "deployments.apps,services", "-l", selector, "-o", "json"}
	}
	output, err := k.run(ctx, true, args...)
	if err != nil {
		return nil, fmt.Errorf("inventory Kubernetes workloads: %w", err)
	}
	objects, err := decodeKubernetesObjects(output)
	if err != nil {
		return nil, err
	}
	grouped := make(map[string][]kubernetesObject)
	for _, item := range objects {
		key := item.Metadata.Annotations["infercrane.dev/external-key"]
		if key == "" || (filter.Prefix != "" && !strings.HasPrefix(key, filter.Prefix)) || !k.owned(item, key) {
			continue
		}
		grouped[key] = append(grouped[key], item)
	}
	keys := make([]string, 0, len(grouped))
	for key := range grouped {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	resources := make([]Resource, 0, len(keys))
	for _, key := range keys {
		items := grouped[key]
		observation, observeErr := k.observationFromObjects(key, items, 8000)
		if observeErr != nil {
			return nil, observeErr
		}
		resources = append(resources, Resource{ID: k.resourceID(k.resourceName(key)), ExternalKey: key, State: observation.State, Endpoint: observation.Endpoint})
	}
	return resources, nil
}

func (k Kubernetes) observationFromObjects(externalKey string, objects []kubernetesObject, port int) (Observation, error) {
	state, endpoint := "degraded", ""
	if k.WorkloadAPI == "kserve" && len(objects) == 1 {
		state = "provisioning"
		if generationObserved(objects[0]) && conditionTrue(objects[0].Status.Conditions, "Ready") {
			state, endpoint = "ready", objects[0].Status.Address.URL
			if endpoint == "" {
				endpoint = objects[0].Status.URL
			}
		}
	} else if k.WorkloadAPI == "deployment" && k.ownerSetComplete(objects) {
		state = "provisioning"
		for _, item := range objects {
			if item.Kind == "Deployment" && generationObserved(item) && item.Status.AvailableReplicas > 0 {
				state = "ready"
				endpoint = fmt.Sprintf("http://%s.%s.svc:%d", k.resourceName(externalKey), k.Namespace, port)
			}
		}
	}
	details, _ := json.Marshal(k.nativeDetails(objects))
	return Observation{Exists: len(objects) > 0, State: state, Endpoint: endpoint, Details: string(details)}, nil
}

func (k Kubernetes) objects(ctx context.Context, externalKey string) ([]kubernetesObject, error) {
	name := k.resourceName(externalKey)
	var args []string
	if k.WorkloadAPI == "kserve" {
		args = []string{"get", "inferenceservice.serving.kserve.io/" + name, "--ignore-not-found=true", "-o", "json"}
	} else {
		args = []string{"get", "deployment.apps/" + name, "service/" + name, "--ignore-not-found=true", "-o", "json"}
	}
	output, err := k.run(ctx, true, args...)
	if err != nil {
		return nil, fmt.Errorf("inspect Kubernetes workload: %w", err)
	}
	objects, err := decodeKubernetesObjects(output)
	if err != nil {
		return nil, err
	}
	for _, item := range objects {
		if !k.owned(item, externalKey) {
			return nil, fmt.Errorf("Kubernetes resource %s/%s conflicts with durable key ownership", item.Kind, item.Metadata.Name)
		}
	}
	return objects, nil
}

func (k Kubernetes) manifest(spec ReplicaSpec) ([]byte, error) {
	name, port := k.resourceName(spec.ExternalKey), spec.Port
	if port == 0 {
		port = 8000
	}
	image, command, readinessPath, shutdown := k.ImageDigest, []string{"serve", "--model", spec.Model, "--host", "0.0.0.0", "--port", fmt.Sprint(port), "--api-key", "$(INFERCRANE_WORKER_API_KEY)"}, "/health", 30
	if spec.ModelRevision != "" {
		command = append(command, "--revision", spec.ModelRevision)
	}
	command = append(command, spec.RuntimeArgs...)
	if !spec.Workload.Empty() {
		image, command, readinessPath, shutdown = spec.Workload.Image, expandWorkloadArgs(spec.Workload, spec), spec.Workload.ReadinessPath, spec.Workload.ShutdownGraceSeconds
	}
	labels := map[string]any{"app.kubernetes.io/name": "infercrane-runtime", "app.kubernetes.io/instance": name, "app.kubernetes.io/managed-by": "infercrane", "infercrane.dev/external-key-hash": k.externalKeyHash(spec.ExternalKey)}
	intentHash, err := k.intentHash(spec)
	if err != nil {
		return nil, err
	}
	metadata := map[string]any{"name": name, "namespace": k.Namespace, "labels": labels, "annotations": map[string]any{"infercrane.dev/external-key": spec.ExternalKey, "infercrane.dev/provider-contract": "infercrane.provider/v1", "infercrane.dev/intent-hash": intentHash}}
	container := map[string]any{"name": "runtime", "image": image, "imagePullPolicy": "IfNotPresent", "args": command, "ports": []any{map[string]any{"name": "http", "containerPort": port}}, "env": []any{map[string]any{"name": "INFERCRANE_WORKER_API_KEY", "valueFrom": map[string]any{"secretKeyRef": map[string]any{"name": k.WorkerSecretName, "key": k.WorkerSecretKey}}}}, "readinessProbe": map[string]any{"httpGet": map[string]any{"path": readinessPath, "port": "http"}, "periodSeconds": 5, "timeoutSeconds": 2, "failureThreshold": 12}, "resources": map[string]any{"limits": map[string]any{k.GPUResource: "1"}, "requests": map[string]any{k.GPUResource: "1"}}, "securityContext": map[string]any{"allowPrivilegeEscalation": false}}
	podSpec := map[string]any{"serviceAccountName": k.ServiceAccount, "terminationGracePeriodSeconds": shutdown, "containers": []any{container}, "nodeSelector": map[string]any{k.GPUProductLabel: spec.GPU}}
	if k.WorkloadAPI == "kserve" {
		metadata["annotations"].(map[string]any)["serving.kserve.io/deploymentMode"] = "Standard"
		predictor := map[string]any{"minReplicas": 1, "maxReplicas": 1, "serviceAccountName": k.ServiceAccount, "nodeSelector": map[string]any{k.GPUProductLabel: spec.GPU}, "containers": []any{container}}
		return json.Marshal(map[string]any{"apiVersion": "serving.kserve.io/v1beta1", "kind": "InferenceService", "metadata": metadata, "spec": map[string]any{"predictor": predictor}})
	}
	deployment := map[string]any{"apiVersion": "apps/v1", "kind": "Deployment", "metadata": metadata, "spec": map[string]any{"replicas": 1, "strategy": map[string]any{"type": "Recreate"}, "progressDeadlineSeconds": 1800, "selector": map[string]any{"matchLabels": map[string]any{"app.kubernetes.io/instance": name}}, "template": map[string]any{"metadata": map[string]any{"labels": labels}, "spec": podSpec}}}
	service := map[string]any{"apiVersion": "v1", "kind": "Service", "metadata": metadata, "spec": map[string]any{"selector": map[string]any{"app.kubernetes.io/instance": name}, "ports": []any{map[string]any{"name": "http", "port": port, "targetPort": "http"}}}}
	return json.Marshal(map[string]any{"apiVersion": "v1", "kind": "List", "items": []any{deployment, service}})
}

func expandWorkloadArgs(workload runtimecontract.Workload, spec ReplicaSpec) []string {
	args := append(append([]string(nil), workload.Command...), spec.RuntimeArgs...)
	for i := range args {
		args[i] = strings.ReplaceAll(args[i], "${MODEL}", spec.Model)
		args[i] = strings.ReplaceAll(args[i], "${MODEL_REVISION}", spec.ModelRevision)
		args[i] = strings.ReplaceAll(args[i], "${PORT}", fmt.Sprint(workload.Port))
		args[i] = strings.ReplaceAll(args[i], "${WORKER_API_KEY}", "$(INFERCRANE_WORKER_API_KEY)")
	}
	return args
}

func (k Kubernetes) validate(spec ReplicaSpec) error {
	if err := k.validateConfig(); err != nil {
		return err
	}
	if spec.ExternalKey == "" || len(spec.ExternalKey) > 253 || spec.Model == "" || spec.Cloud != "kubernetes" || spec.GPU == "" {
		return errors.New("Kubernetes replica requires bounded external key, model, cloud kubernetes, and GPU")
	}
	if !spec.Workload.Empty() {
		if err := spec.Workload.Validate(); err != nil {
			return fmt.Errorf("portable workload: %w", err)
		}
	} else if runtimecontract.ValidateImage(k.ImageDigest) != nil {
		return errors.New("Kubernetes workload image must be pinned by sha256 digest")
	}
	return nil
}

func (k Kubernetes) validateConfig() error {
	if k.Namespace == "" || k.Context == "" || k.ServiceAccount == "" || k.WorkerSecretName == "" || k.WorkerSecretKey == "" || k.GPUResource == "" || k.GPUProductLabel == "" {
		return errors.New("Kubernetes context, namespace, service account, worker Secret name/key, GPU resource, and GPU product label are required")
	}
	if k.WorkloadAPI != "deployment" && k.WorkloadAPI != "kserve" {
		return errors.New("Kubernetes workload API must be deployment or kserve")
	}
	for _, value := range []string{k.Namespace, k.ServiceAccount, k.WorkerSecretName} {
		if !validKubernetesDNSLabel(value) {
			return fmt.Errorf("invalid Kubernetes identifier %q", value)
		}
	}
	if !validKubernetesSecretKey(k.WorkerSecretKey) {
		return fmt.Errorf("invalid Kubernetes Secret key %q", k.WorkerSecretKey)
	}
	return nil
}

func (k Kubernetes) ownerSetComplete(objects []kubernetesObject) bool {
	if k.WorkloadAPI == "kserve" {
		return len(objects) == 1 && objects[0].Kind == "InferenceService"
	}
	deployment, service := 0, 0
	for _, item := range objects {
		switch item.Kind {
		case "Deployment":
			deployment++
		case "Service":
			service++
		}
	}
	return deployment == 1 && service == 1 && len(objects) == 2
}

func (k Kubernetes) owned(item kubernetesObject, externalKey string) bool {
	return item.Metadata.Namespace == k.Namespace && item.Metadata.Labels["app.kubernetes.io/managed-by"] == "infercrane" && item.Metadata.Labels["infercrane.dev/external-key-hash"] == k.externalKeyHash(externalKey) && item.Metadata.Annotations["infercrane.dev/external-key"] == externalKey
}

func (k Kubernetes) resourceName(externalKey string) string {
	return "infercrane-" + k.externalKeyHash(externalKey)
}
func (k Kubernetes) externalKeyHash(externalKey string) string {
	sum := sha256.Sum256([]byte(externalKey))
	return hex.EncodeToString(sum[:12])
}
func (k Kubernetes) resourceID(name string) string {
	return "kubernetes:" + k.WorkloadAPI + ":" + k.Namespace + "/" + name
}

func (k Kubernetes) intentHash(spec ReplicaSpec) (string, error) {
	spec.RequestID = ""
	value := struct {
		Spec           ReplicaSpec
		Namespace      string
		WorkloadAPI    string
		ServiceAccount string
		WorkerSecret   string
		WorkerKey      string
		ImageDigest    string
		GPUResource    string
		GPULabel       string
	}{spec, k.Namespace, k.WorkloadAPI, k.ServiceAccount, k.WorkerSecretName, k.WorkerSecretKey, k.ImageDigest, k.GPUResource, k.GPUProductLabel}
	body, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(body)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func (k Kubernetes) nativeDetails(objects []kubernetesObject) map[string]any {
	items := make([]map[string]any, 0, len(objects))
	for _, item := range objects {
		conditions := make([]map[string]string, 0, len(item.Status.Conditions))
		for _, condition := range item.Status.Conditions {
			conditions = append(conditions, map[string]string{"type": condition.Type, "status": condition.Status, "reason": condition.Reason})
		}
		items = append(items, map[string]any{"api_version": item.APIVersion, "kind": item.Kind, "name": item.Metadata.Name, "namespace": item.Metadata.Namespace, "uid": item.Metadata.UID, "generation": item.Metadata.Generation, "observed_generation": item.Status.ObservedGeneration, "available_replicas": item.Status.AvailableReplicas, "ready_replicas": item.Status.ReadyReplicas, "conditions": conditions})
	}
	return map[string]any{"context": k.Context, "namespace": k.Namespace, "workload_api": k.WorkloadAPI, "resources": items}
}

func generationObserved(item kubernetesObject) bool {
	return item.Metadata.Generation > 0 && item.Status.ObservedGeneration == item.Metadata.Generation
}

func (k Kubernetes) run(ctx context.Context, namespaced bool, args ...string) ([]byte, error) {
	runner := k.Runner
	if runner == nil {
		binary := k.Binary
		if binary == "" {
			binary = "kubectl"
		}
		if _, err := exec.LookPath(binary); err != nil {
			return nil, errors.New("kubectl is required for the Kubernetes provider")
		}
		runner = execRunner{binary: binary}
	}
	global := []string{"--context", k.Context}
	if namespaced {
		global = append(global, "--namespace", k.Namespace)
	}
	output, err := runner.Run(ctx, nil, append(global, args...)...)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		// kubectl output may contain admission details but can also include
		// credential material from plugins. Preserve classification, not output.
		return nil, errors.New("kubectl request failed")
	}
	return output, nil
}

func decodeKubernetesObjects(output []byte) ([]kubernetesObject, error) {
	if len(strings.TrimSpace(string(output))) == 0 {
		return []kubernetesObject{}, nil
	}
	var envelope struct {
		Kind  string             `json:"kind"`
		Items []kubernetesObject `json:"items"`
	}
	if err := json.Unmarshal(output, &envelope); err != nil {
		return nil, errors.New("kubectl returned invalid JSON")
	}
	if envelope.Kind == "List" {
		return envelope.Items, nil
	}
	var item kubernetesObject
	if err := json.Unmarshal(output, &item); err != nil || item.Kind == "" {
		return nil, errors.New("kubectl returned an invalid Kubernetes object")
	}
	return []kubernetesObject{item}, nil
}

func conditionTrue(conditions []struct{ Type, Status, Reason, Message string }, name string) bool {
	for _, condition := range conditions {
		if condition.Type == name && condition.Status == "True" {
			return true
		}
	}
	return false
}

func writeKubernetesManifest(body []byte) (string, func(), error) {
	file, err := os.CreateTemp("", "infercrane-kubernetes-*.json")
	if err != nil {
		return "", func() {}, err
	}
	path := file.Name()
	cleanup := func() { _ = os.Remove(path) }
	if err = file.Chmod(0o600); err == nil {
		_, err = file.Write(body)
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		cleanup()
		return "", func() {}, err
	}
	return path, cleanup, nil
}

func validKubernetesDNSLabel(value string) bool {
	if value == "" || len(value) > 63 || value[0] < 'a' || value[0] > 'z' {
		return false
	}
	for i, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || (r == '-' && i > 0 && i < len(value)-1) {
			continue
		}
		return false
	}
	return true
}

func validKubernetesSecretKey(value string) bool {
	if value == "" || len(value) > 253 || value[0] == '.' || value[0] == '-' || value[0] == '_' {
		return false
	}
	for i, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || ((r == '.' || r == '-' || r == '_') && i < len(value)-1) {
			continue
		}
		return false
	}
	return true
}
