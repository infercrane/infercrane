package secrets

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/secretcipher"
)

func testSealer(t *testing.T, keyReference string) Sealed {
	t.Helper()
	cipher, err := secretcipher.New(strings.Repeat("k", 32))
	if err != nil {
		t.Fatal(err)
	}
	return Sealed{Cipher: &cipher, KeyReference: keyReference}
}

func TestSealedValueOpensOnlyForItsWorkspaceAndName(t *testing.T) {
	sealer := testSealer(t, "environment:KEY")
	sealed, keyReference, err := sealer.Seal("acme", "hf-token", "hf_abcdefghijklmnop")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(sealed.Ciphertext), "hf_abcdefghijklmnop") {
		t.Fatal("ciphertext contains the plaintext")
	}
	reference := domain.SecretReference{TenantID: "acme", Name: "hf-token", Resolver: ResolverSealed, Sealed: sealed, KeyReference: keyReference}
	value, err := (Resolvers{Sealed: sealer}).Resolve(context.Background(), reference)
	if err != nil || value != "hf_abcdefghijklmnop" {
		t.Fatalf("sealed value did not open: %q %v", value, err)
	}
	moved := reference
	moved.TenantID = "other"
	if _, err = sealer.Resolve(context.Background(), moved); !errors.Is(err, ErrSealedUnreadable) {
		t.Fatalf("a row moved to another workspace opened: %v", err)
	}
	renamed := reference
	renamed.Name = "other-name"
	if _, err = sealer.Resolve(context.Background(), renamed); !errors.Is(err, ErrSealedUnreadable) {
		t.Fatalf("a renamed row opened: %v", err)
	}
	rotated := testSealer(t, "environment:NEW_KEY")
	if _, err = rotated.Resolve(context.Background(), reference); !errors.Is(err, ErrSealedUnreadable) {
		t.Fatalf("a value sealed under another key reference opened: %v", err)
	}
	if _, err = (Sealed{}).Resolve(context.Background(), reference); !errors.Is(err, ErrSealedUnavailable) {
		t.Fatalf("a control plane without a key opened a sealed value: %v", err)
	}
}

func TestSealRefusesUnsafeValues(t *testing.T) {
	sealer := testSealer(t, "ref")
	for _, value := range []string{"", "two words", "line\nbreak", "tab\tvalue", "café", strings.Repeat("a", MaxSealedValueBytes+1)} {
		if _, _, err := sealer.Seal("acme", "name", value); err == nil {
			t.Fatalf("value %q was sealed", value)
		}
	}
	if _, _, err := (Sealed{}).Seal("acme", "name", "token"); !errors.Is(err, ErrSealedUnavailable) {
		t.Fatalf("sealed without a key: %v", err)
	}
}

func TestWorkerHeldReferencesNeverResolveInTheControlPlane(t *testing.T) {
	resolvers := Resolvers{Environment: Environment{Tenants: NewTenantPolicy("*"), Lookup: func(string) (string, bool) { return "leak", true }}, Sealed: testSealer(t, "ref")}
	for _, reference := range []domain.SecretReference{
		{TenantID: "acme", Resolver: ResolverKubernetes, Reference: "hf-token/token"},
		{TenantID: "acme", Resolver: ResolverAWSSecretsManager, Reference: "arn:aws:secretsmanager:eu-central-1:123456789012:secret:hf-token-AbCdEf"},
	} {
		if err := ValidateReference(reference); err != nil {
			t.Fatalf("valid %s reference refused: %v", reference.Resolver, err)
		}
		if value, err := resolvers.Resolve(context.Background(), reference); !errors.Is(err, ErrWorkerHeld) || value != "" {
			t.Fatalf("%s reference resolved in the control plane: %q %v", reference.Resolver, value, err)
		}
	}
	// An env-shaped reference that claims another kind never reaches the environment.
	if value, err := resolvers.Environment.Resolve(context.Background(), domain.SecretReference{TenantID: "acme", Resolver: ResolverKubernetes, Reference: "INFERCRANE_API_KEY"}); err == nil || value != "" {
		t.Fatalf("environment resolver read a non-env reference: %q %v", value, err)
	}
}

func TestValidateReferenceShapes(t *testing.T) {
	invalid := []domain.SecretReference{
		{Resolver: ResolverKubernetes, Reference: "Secret/key"},
		{Resolver: ResolverKubernetes, Reference: "secret"},
		{Resolver: ResolverKubernetes, Reference: "secret/.."},
		{Resolver: ResolverKubernetes, Reference: "secret/a/b"},
		{Resolver: ResolverAWSSecretsManager, Reference: "arn:aws:s3:::bucket"},
		{Resolver: ResolverAWSSecretsManager, Reference: "arn:aws:secretsmanager:eu-central-1:1234:secret:x"},
		{Resolver: ResolverSealed, Reference: "something"},
		{Resolver: "vault", Reference: "x"},
	}
	for _, reference := range invalid {
		if err := ValidateReference(reference); err == nil {
			t.Fatalf("invalid reference accepted: %#v", reference)
		}
	}
	name, key, ok := KubernetesSecretKey("hf-token/HF_TOKEN")
	if !ok || name != "hf-token" || key != "HF_TOKEN" {
		t.Fatalf("kubernetes reference split wrong: %q %q %v", name, key, ok)
	}
}
