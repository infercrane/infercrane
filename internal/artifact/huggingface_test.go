package artifact

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/infercrane/infercrane/internal/domain"
)

type fakeRunner struct{ output string }

func (f fakeRunner) Run(context.Context, string, ...string) ([]byte, error) {
	if f.output == "" {
		return nil, errors.New("failed")
	}
	return []byte(f.output), nil
}

type credentialRunner struct {
	output      string
	environment []string
	arguments   []string
}

func (r *credentialRunner) Run(context.Context, string, ...string) ([]byte, error) {
	return nil, errors.New("credentialed resolution must use isolated environment")
}

func (r *credentialRunner) RunWithEnvironment(_ context.Context, _ string, environment []string, arguments ...string) ([]byte, error) {
	r.environment, r.arguments = environment, arguments
	return []byte(r.output), nil
}

type fakeTenantSecretStore struct{ reference domain.SecretReference }

func (s fakeTenantSecretStore) SecretReferenceForTenant(context.Context, string, string) (domain.SecretReference, error) {
	return s.reference, nil
}

type fakeTenantSecretResolver struct{ value string }

func (r fakeTenantSecretResolver) Resolve(context.Context, domain.SecretReference) (string, error) {
	return r.value, nil
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

func TestTenantHuggingFaceKeepsCredentialOutOfArgumentsAndArtifact(t *testing.T) {
	sha := "0123456789abcdef0123456789abcdef01234567"
	runner := &credentialRunner{output: `{"repository":"private/coder","requested_revision":"main","immutable_revision":"` + sha + `","runtime_compatibility":{}}`}
	resolver := TenantHuggingFace{
		Public:  HuggingFace{Runner: runner},
		Store:   fakeTenantSecretStore{reference: domain.SecretReference{ID: "secret", Resolver: "env", Reference: "HF_PRIVATE_TOKEN"}},
		Secrets: fakeTenantSecretResolver{value: "hf_private_value"},
	}
	resolved, err := resolver.ResolveAuthenticated(context.Background(), "tenant", "secret", "private/coder", "main")
	if err != nil || resolved.ModelIdentity != "private/coder@"+sha {
		t.Fatalf("artifact=%#v err=%v", resolved, err)
	}
	if strings.Join(runner.environment, " ") != "HF_TOKEN=hf_private_value" || strings.Contains(strings.Join(runner.arguments, " "), "hf_private_value") || strings.Contains(resolved.RuntimeCompatibilityJSON, "hf_private_value") {
		t.Fatalf("credential boundary violated: env=%q args=%q artifact=%#v", runner.environment, runner.arguments, resolved)
	}
}
