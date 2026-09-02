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
	registry, err := NewRegistry(NewDeepSeekAdapter(nil))
	if err != nil {
		panic(err)
	}
	return registry
}
