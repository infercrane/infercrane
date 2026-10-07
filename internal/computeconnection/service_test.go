package computeconnection

import (
	"context"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/secretcipher"
)

type memoryStore struct{ item domain.ComputeConnection }

func (m *memoryStore) CreateComputeConnection(_ context.Context, tenant string, item domain.ComputeConnection) (domain.ComputeConnection, error) {
	item.ID, item.TenantID = "connection-1", tenant
	m.item = item
	return item, nil
}
func (m *memoryStore) ComputeConnectionForTenant(_ context.Context, tenant, id string) (domain.ComputeConnection, error) {
	if m.item.TenantID != tenant || m.item.ID != id {
		return domain.ComputeConnection{}, domain.ErrNotFound
	}
	return m.item, nil
}
func (m *memoryStore) ComputeConnectionsForTenant(context.Context, string) ([]domain.ComputeConnection, error) {
	return []domain.ComputeConnection{m.item}, nil
}
func (*memoryStore) DeleteComputeConnectionForTenant(context.Context, string, string) error {
	return nil
}

type verifier struct {
	credential string
	calls      int
}

func (v *verifier) VerifyCredential(_ context.Context, credential string) error {
	v.calls++
	v.credential = credential
	return nil
}

func TestServiceVerifiesEncryptsAndResolvesCredential(t *testing.T) {
	cipher, err := secretcipher.New("01234567890123456789012345678901")
	if err != nil {
		t.Fatal(err)
	}
	store, probe := &memoryStore{}, &verifier{}
	service := Service{Store: store, Cipher: cipher, KeyReference: "key-v1", Verifiers: map[string]CredentialVerifier{"runpod": probe}, Now: func() time.Time { return time.Unix(100, 0) }}
	item, err := service.Create(context.Background(), "tenant-1", CreateRequest{Name: "Primary", Provider: "runpod", Credential: "runpod-secret-token"})
	if err != nil {
		t.Fatal(err)
	}
	if probe.calls != 1 || probe.credential != "runpod-secret-token" || string(store.item.CredentialCiphertext) == probe.credential || item.Adapter != "runpod-pods" || item.Name != "primary" {
		t.Fatalf("item=%#v verified=%q", item, probe.credential)
	}
	_, credential, err := service.Resolve(context.Background(), "tenant-1", item.ID, "runpod")
	if err != nil || credential != "runpod-secret-token" {
		t.Fatalf("credential=%q err=%v", credential, err)
	}
}

func TestServiceRejectsUnqualifiedProviderBeforeCredentialVerification(t *testing.T) {
	cipher, err := secretcipher.New("01234567890123456789012345678901")
	if err != nil {
		t.Fatal(err)
	}
	probe := &verifier{}
	service := Service{
		Store:        &memoryStore{},
		Cipher:       cipher,
		KeyReference: "key-v1",
		Verifiers:    map[string]CredentialVerifier{"unqualified": probe},
	}
	_, err = service.Create(context.Background(), "tenant-1", CreateRequest{Name: "primary", Provider: "unqualified", Credential: "provider-secret-token"})
	if err == nil || probe.calls != 0 {
		t.Fatalf("unqualified provider reached credential verifier: calls=%d err=%v", probe.calls, err)
	}
}
