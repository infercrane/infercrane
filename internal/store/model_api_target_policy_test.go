package store

import (
	"testing"

	"github.com/infercrane/infercrane/internal/supplieradapter"
)

func TestEveryStrictMVPTargetRequiresImmutableBinding(t *testing.T) {
	for _, adapter := range []string{
		supplieradapter.RunPodVLLMAdapterName,
		supplieradapter.RunPodSGLangLBAdapterName,
		supplieradapter.ZAIAdapterName,
	} {
		if !requiresModelAPITargetBinding(adapter) {
			t.Fatalf("adapter %q escaped the durable target-binding policy", adapter)
		}
	}
	if requiresModelAPITargetBinding(supplieradapter.DeepSeekAdapterName) {
		t.Fatal("legacy DeepSeek route unexpectedly changed its publication contract")
	}
}
