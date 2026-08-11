package providerfixture

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
)

// KubernetesCLI is a deterministic kubectl transport fixture. It models API
// persistence and lost responses without pretending to be a Kubernetes API
// server; Kind supplies the separate schema/admission integration gate.
type KubernetesCLI struct {
	mu                  sync.Mutex
	Objects             map[string]map[string]any
	ApplyCalls, DryRuns int
	DeleteCalls         int
	FailAfterApplyOnce  bool
	ApplyFailure        bool
	KServeInstalled     bool
}

func NewKubernetesCLI() *KubernetesCLI {
	return &KubernetesCLI{Objects: make(map[string]map[string]any), KServeInstalled: true}
}

func (k *KubernetesCLI) Run(_ context.Context, _ []string, args ...string) ([]byte, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	command, index := kubectlCommand(args)
	if index < 0 {
		return nil, errors.New("invalid kubectl invocation")
	}
	switch command {
	case "version":
		return []byte(`{"serverVersion":{"gitVersion":"v1.36.1"}}`), nil
	case "api-resources":
		if !k.KServeInstalled {
			return nil, errors.New("the server doesn't have a resource type")
		}
		return []byte("inferenceservices.serving.kserve.io\n"), nil
	case "auth":
		return []byte("yes\n"), nil
	case "apply":
		return k.apply(args[index+1:])
	case "get":
		return k.get(args[index+1:])
	case "delete":
		return k.delete(args[index+1:])
	default:
		return nil, errors.New("unsupported kubectl command")
	}
}

func (k *KubernetesCLI) apply(args []string) ([]byte, error) {
	path := flagValue(args, "-f")
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var root map[string]any
	if json.Unmarshal(body, &root) != nil {
		return nil, errors.New("invalid manifest")
	}
	if hasArg(args, "--dry-run=server") {
		k.DryRuns++
		return []byte("validated\n"), nil
	}
	if k.ApplyFailure {
		return nil, errors.New("field conflict")
	}
	items := []any{root}
	if root["kind"] == "List" {
		items, _ = root["items"].([]any)
	}
	for _, raw := range items {
		item, _ := raw.(map[string]any)
		metadata, _ := item["metadata"].(map[string]any)
		name, _ := metadata["name"].(string)
		kind, _ := item["kind"].(string)
		metadata["uid"] = "uid-" + strings.ToLower(kind) + "-" + name
		metadata["generation"] = float64(1)
		switch kind {
		case "Deployment":
			item["status"] = map[string]any{"availableReplicas": 1, "readyReplicas": 1, "conditions": []any{map[string]any{"type": "Available", "status": "True", "reason": "MinimumReplicasAvailable"}}}
		case "InferenceService":
			item["status"] = map[string]any{"url": "http://" + name + ".example.test", "conditions": []any{map[string]any{"type": "Ready", "status": "True", "reason": "Ready"}}}
		}
		k.Objects[strings.ToLower(kind)+"/"+name] = item
	}
	k.ApplyCalls++
	if k.FailAfterApplyOnce {
		k.FailAfterApplyOnce = false
		return nil, errors.New("response lost")
	}
	return []byte("applied\n"), nil
}

func (k *KubernetesCLI) get(args []string) ([]byte, error) {
	var selected []map[string]any
	if len(args) > 0 && (strings.Contains(args[0], ",") || !strings.Contains(args[0], "/")) {
		for _, item := range k.Objects {
			selected = append(selected, item)
		}
	} else {
		for _, arg := range args {
			if strings.HasPrefix(arg, "-") {
				break
			}
			parts := strings.SplitN(arg, "/", 2)
			if len(parts) != 2 {
				continue
			}
			kind := normalizeKubernetesKind(parts[0])
			if item := k.Objects[kind+"/"+parts[1]]; item != nil {
				selected = append(selected, item)
			}
		}
	}
	items := make([]any, len(selected))
	for i := range selected {
		items[i] = selected[i]
	}
	return json.Marshal(map[string]any{"apiVersion": "v1", "kind": "List", "items": items})
}

func (k *KubernetesCLI) delete(args []string) ([]byte, error) {
	for _, arg := range args {
		if strings.HasPrefix(arg, "-") {
			continue
		}
		parts := strings.SplitN(arg, "/", 2)
		if len(parts) != 2 {
			continue
		}
		delete(k.Objects, normalizeKubernetesKind(parts[0])+"/"+parts[1])
	}
	k.DeleteCalls++
	return []byte("deleted\n"), nil
}

func kubectlCommand(args []string) (string, int) {
	for i, arg := range args {
		switch arg {
		case "version", "api-resources", "auth", "apply", "get", "delete":
			return arg, i
		}
	}
	return "", -1
}

func flagValue(args []string, name string) string {
	for i := range args {
		if args[i] == name && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func hasArg(args []string, value string) bool {
	for _, arg := range args {
		if arg == value {
			return true
		}
	}
	return false
}

func normalizeKubernetesKind(value string) string {
	value = strings.ToLower(value)
	switch {
	case strings.HasPrefix(value, "deployment"):
		return "deployment"
	case strings.HasPrefix(value, "service"):
		return "service"
	case strings.HasPrefix(value, "inferenceservice"):
		return "inferenceservice"
	default:
		return value
	}
}
