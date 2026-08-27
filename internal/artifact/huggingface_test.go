package artifact

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type fakeRunner struct{ output string }

func (f fakeRunner) Run(context.Context, string, ...string) ([]byte, error) {
	if f.output == "" {
		return nil, errors.New("failed")
	}
	return []byte(f.output), nil
}

func TestHuggingFaceResolveReturnsImmutableIdentity(t *testing.T) {
	sha := "0123456789abcdef0123456789abcdef01234567"
	resolved, err := (HuggingFace{Runner: fakeRunner{output: `{"repository":"Qwen/Qwen3-8B","requested_revision":"main","immutable_revision":"` + sha + `","approximate_size_bytes":100,"runtime_compatibility":{"library_name":"transformers"}}`}}).Resolve(context.Background(), "Qwen/Qwen3-8B", "main")
	if err != nil || resolved.ImmutableRevision != sha || resolved.ModelIdentity != "Qwen/Qwen3-8B@"+sha || resolved.CacheState != "unknown" {
		t.Fatalf("artifact=%#v err=%v", resolved, err)
	}
}

func TestHuggingFaceResolveRejectsMutableResponse(t *testing.T) {
	_, err := (HuggingFace{Runner: fakeRunner{output: `{"repository":"Qwen/Qwen3-8B","requested_revision":"main","immutable_revision":"main"}`}}).Resolve(context.Background(), "Qwen/Qwen3-8B", "main")
	if err == nil {
		t.Fatal("mutable identity was accepted")
	}
}

func TestHuggingFaceResolverPrefersExactRevisionFileSizes(t *testing.T) {
	exact := strings.Index(resolverScript, `size = sum(sizes)`)
	fallback := strings.Index(resolverScript, `size = getattr(info, "used_storage", None)`)
	if exact < 0 || fallback < 0 || exact >= fallback {
		t.Fatal("resolver must prefer the exact revision file sum over repository-wide historical storage")
	}
	if !strings.Contains(resolverScript, `"artifact_size_source": size_source`) {
		t.Fatal("resolver must preserve the size evidence source")
	}
}
