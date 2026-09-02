package supplieradapter

import (
	"errors"
	"fmt"
	"strings"
)

// Registry is an immutable adapter lookup. Runtime code selects an adapter by
// the operator-published adapter name; supplier names and endpoints never
// implicitly select executable code.
type Registry struct {
	adapters map[string]Adapter
}

func NewRegistry(adapters ...Adapter) (*Registry, error) {
	registry := &Registry{adapters: make(map[string]Adapter, len(adapters))}
	for _, adapter := range adapters {
		if adapter == nil {
			return nil, errors.New("supplier adapter is required")
		}
		name := strings.TrimSpace(adapter.Name())
		if name == "" {
			return nil, errors.New("supplier adapter name is required")
		}
		if _, exists := registry.adapters[name]; exists {
			return nil, fmt.Errorf("supplier adapter %q is duplicated", name)
		}
		registry.adapters[name] = adapter
	}
	return registry, nil
}

func (r *Registry) Lookup(name string) (Adapter, bool) {
	if r == nil {
		return nil, false
	}
	adapter, ok := r.adapters[strings.TrimSpace(name)]
	return adapter, ok
}

func DefaultRegistry() *Registry {
	registry, err := NewRegistry(NewDeepSeekAdapter(nil), NewHuggingFaceRouterAdapter(nil), NewRunPodVLLMAdapter(nil))
	if err != nil {
		panic(err)
	}
	return registry
}

// RequiresImmutableTargetBinding identifies adapters whose execution identity
// includes operator-selected infrastructure or a routed provider. Their exact
// endpoint and model/provider tuple must be committed before publication.
func RequiresImmutableTargetBinding(adapter string) bool {
	switch strings.TrimSpace(adapter) {
	case HuggingFaceRouterAdapterName, RunPodVLLMAdapterName:
		return true
	default:
		return false
	}
}

// RequiresBillingPrincipal identifies adapters whose supplier-side payer must
// be pinned separately from the credential reference. This prevents a secret
// rotation from silently moving spend to another account.
func RequiresBillingPrincipal(adapter string) bool {
	return strings.TrimSpace(adapter) == HuggingFaceRouterAdapterName
}
