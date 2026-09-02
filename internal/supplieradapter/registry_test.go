package supplieradapter

import "testing"

func TestRegistryRequiresExplicitUniqueAdapterNames(t *testing.T) {
	registry, err := NewRegistry(NewDeepSeekAdapter(nil))
	if err != nil {
		t.Fatal(err)
	}
	adapter, ok := registry.Lookup(DeepSeekAdapterName)
	if !ok || adapter.Name() != DeepSeekAdapterName {
		t.Fatalf("adapter lookup failed: adapter=%v ok=%t", adapter, ok)
	}
	if _, ok := registry.Lookup(DeepSeekSupplier); ok {
		t.Fatal("supplier name implicitly selected executable adapter code")
	}
	if _, err = NewRegistry(NewDeepSeekAdapter(nil), NewDeepSeekAdapter(nil)); err == nil {
		t.Fatal("duplicate adapter registration was accepted")
	}
}

func TestDefaultRegistryContainsQualifiedHostedAdapters(t *testing.T) {
	registry := DefaultRegistry()
	if _, ok := registry.Lookup(DeepSeekAdapterName); !ok {
		t.Fatal("DeepSeek adapter is absent from the default registry")
	}
	if _, ok := registry.Lookup(RunPodVLLMAdapterName); !ok {
		t.Fatal("RunPod vLLM adapter is absent from the default registry")
	}
	if _, ok := registry.Lookup("openai"); ok {
		t.Fatal("unqualified generic adapter appeared in the default registry")
	}
}
