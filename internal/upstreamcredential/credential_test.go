package upstreamcredential

import (
	"context"
	"errors"
	"testing"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/secretcipher"
)

type memory map[string]domain.AdoptedCredential

func (m memory) AdoptedCredential(_ context.Context, _, target string) (domain.AdoptedCredential, error) {
	if value, ok := m[target]; ok {
		return value, nil
	}
	return domain.AdoptedCredential{}, domain.ErrNotFound
}

type failing struct{}

func (failing) AdoptedCredential(context.Context, string, string) (domain.AdoptedCredential, error) {
	return domain.AdoptedCredential{}, context.DeadlineExceeded
}

func TestSealedCredentialsOpenOnlyForTheirOwnEndpointAndFailClosed(t *testing.T) {
	cipher, err := secretcipher.New("a-test-credential-key-of-at-least-32-bytes")
	if err != nil {
		t.Fatal(err)
	}
	sealer := Sealer{Cipher: &cipher}
	sealed, err := sealer.Seal("tenant-a", "private-qwen", "server-token")
	if err != nil {
		t.Fatal(err)
	}
	store := memory{
		"target-1": {TenantID: "tenant-a", EndpointName: "private-qwen", TargetID: "target-1", Sealed: sealed},
		"moved":    {TenantID: "tenant-b", EndpointName: "private-qwen", TargetID: "moved", Sealed: sealed},
	}
	sealer.Store = store
	if token, err := sealer.Resolve(context.Background(), "tenant-a", "target-1"); err != nil || token != "server-token" {
		t.Fatalf("resolve: %q %v", token, err)
	}
	if _, err := sealer.Resolve(context.Background(), "tenant-b", "moved"); !errors.Is(err, ErrUnreadable) {
		t.Fatalf("a ciphertext moved to another tenant opened: %v", err)
	}
	if _, err := sealer.Resolve(context.Background(), "tenant-a", "none"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("no credential: %v", err)
	}
	withoutKey := Sealer{Store: store}
	if _, err := withoutKey.Resolve(context.Background(), "tenant-a", "target-1"); !errors.Is(err, ErrUnreadable) {
		t.Fatalf("a stored credential without a key must fail closed: %v", err)
	}
	if _, err := withoutKey.Seal("tenant-a", "x", "token"); err == nil {
		t.Fatal("sealed without a key")
	}
	if _, err := (Sealer{Cipher: &cipher, Store: failing{}}).Resolve(context.Background(), "tenant-a", "target-1"); errors.Is(err, ErrUnreadable) || errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("a store error was reported as unreadable or missing: %v", err)
	}
	for _, bad := range []string{"", "two words", "line\nbreak"} {
		if _, err := sealer.Seal("tenant-a", "x", bad); err == nil {
			t.Fatalf("invalid credential %q sealed", bad)
		}
	}
}
