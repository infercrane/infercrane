package authn

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/infercrane/infercrane/internal/domain"
)

type fakeSource struct{ records []domain.CredentialRecord }

func (f fakeSource) ActiveCredentials(context.Context) ([]domain.CredentialRecord, error) {
	return f.records, nil
}
func TestCacheAuthenticatesWithoutSourceOnRequestPath(t *testing.T) {
	sum := sha256.Sum256([]byte("secret"))
	cache := &Cache{Source: fakeSource{records: []domain.CredentialRecord{{Hash: hex.EncodeToString(sum[:]), Principal: domain.Principal{ID: "p", TenantID: "t"}}}}}
	if err := cache.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	cache.Source = nil
	principal, err := cache.AuthenticatePrincipal(context.Background(), "secret")
	if err != nil || principal.ID != "p" {
		t.Fatalf("principal=%#v err=%v", principal, err)
	}
	if _, err := cache.AuthenticatePrincipal(context.Background(), "wrong"); err == nil {
		t.Fatal("wrong credential authenticated")
	}
}
