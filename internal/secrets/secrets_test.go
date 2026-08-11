package secrets

import (
	"context"
	"strings"
	"testing"

	"github.com/infercrane/infercrane/internal/domain"
)

func TestEnvironmentResolvesWithoutLeakingValueInErrors(t *testing.T) {
	const secret = "must-never-appear"
	resolver := Environment{Lookup: func(name string) (string, bool) {
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
	resolver := Environment{}
	for _, reference := range []string{"lowercase", "A;env", "A"} {
		if _, err := resolver.Resolve(context.Background(), domain.SecretReference{Resolver: "env", Reference: reference}); err == nil {
			t.Fatalf("unsafe reference %q accepted", reference)
		}
	}
}
