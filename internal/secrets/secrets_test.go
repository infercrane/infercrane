package secrets

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/infercrane/infercrane/internal/domain"
)

func TestEnvironmentResolvesWithoutLeakingValueInErrors(t *testing.T) {
	const secret = "must-never-appear"
	resolver := Environment{Tenants: NewTenantPolicy("*"), Lookup: func(name string) (string, bool) {
		if name == "OPENROUTER_API_KEY" {
			return secret, true
		}
		return "", false
	}}
	value, err := resolver.Resolve(context.Background(), domain.SecretReference{Resolver: "env", Reference: "OPENROUTER_API_KEY"})
	if err != nil || value != secret {
		t.Fatalf("value=%q err=%v", value, err)
	}
	_, err = resolver.Resolve(context.Background(), domain.SecretReference{Resolver: "env", Reference: "MISSING_SECRET"})
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("unsafe missing-secret error: %v", err)
	}
}

func TestEnvironmentRejectsUnsafeReferenceNames(t *testing.T) {
	resolver := Environment{Tenants: NewTenantPolicy("*")}
	for _, reference := range []string{"lowercase", "A;env", "A"} {
		if _, err := resolver.Resolve(context.Background(), domain.SecretReference{Resolver: "env", Reference: reference}); err == nil {
			t.Fatalf("unsafe reference %q accepted", reference)
		}
	}
}

func TestEnvironmentResolvesOnlyAllowedWorkspaces(t *testing.T) {
	const operatorKey = "operator-key-must-not-leak"
	lookup := func(name string) (string, bool) {
		if name == "INFERCRANE_API_KEY" {
			return operatorKey, true
		}
		return "", false
	}
	resolver := Environment{Lookup: lookup, Tenants: NewTenantPolicy(BootstrapTenant, "operator-workspace")}
	reference := domain.SecretReference{Resolver: "env", Reference: "INFERCRANE_API_KEY"}
	for _, tenant := range []string{BootstrapTenant, "operator-workspace"} {
		reference.TenantID = tenant
		if value, err := resolver.Resolve(context.Background(), reference); err != nil || value != operatorKey {
			t.Fatalf("allowed workspace %q: value=%q err=%v", tenant, value, err)
		}
	}
	for _, tenant := range []string{"signed-up-workspace", ""} {
		reference.TenantID = tenant
		value, err := resolver.Resolve(context.Background(), reference)
		if !errors.Is(err, ErrTenantNotAllowed) || value != "" {
			t.Fatalf("workspace %q read the control plane's environment: value=%q err=%v", tenant, value, err)
		}
		if strings.Contains(err.Error(), operatorKey) {
			t.Fatalf("error leaks the value: %v", err)
		}
	}
	if resolver.AllowsTenant("signed-up-workspace") || !resolver.AllowsTenant("operator-workspace") {
		t.Fatal("AllowsTenant disagrees with Resolve")
	}
}

func TestZeroEnvironmentResolvesNoWorkspace(t *testing.T) {
	resolver := Environment{Lookup: func(string) (string, bool) { return "value", true }}
	if _, err := resolver.Resolve(context.Background(), domain.SecretReference{TenantID: BootstrapTenant, Resolver: "env", Reference: "ANY_NAME"}); !errors.Is(err, ErrTenantNotAllowed) {
		t.Fatalf("zero policy resolved a reference: %v", err)
	}
}

func TestTenantPolicyWildcardAllowsEveryWorkspace(t *testing.T) {
	policy := NewTenantPolicy(" * ")
	for _, tenant := range []string{BootstrapTenant, "any", ""} {
		if !policy.Allows(tenant) {
			t.Fatalf("wildcard refused %q", tenant)
		}
	}
	if NewTenantPolicy("", "  ").Allows("") {
		t.Fatal("blank IDs allowed the empty workspace")
	}
}
