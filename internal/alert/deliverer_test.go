package alert

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
)

type deliveryStore struct {
	delivery domain.AlertDelivery
	attempts int
}

func (s *deliveryStore) SecretReferenceForTenant(context.Context, string, string) (domain.SecretReference, error) {
	return domain.SecretReference{Resolver: "env", Reference: "SIGNING_SECRET"}, nil
}
func (s *deliveryStore) BeginAlertDelivery(_ context.Context, policy domain.AlertPolicy, finding domain.DiagnosticFinding, digest string) (domain.AlertDelivery, bool, error) {
	if s.delivery.ID == "" {
		s.delivery = domain.AlertDelivery{ID: "delivery-1", TenantID: policy.TenantID, PolicyID: policy.ID, FindingID: finding.ID, Status: "pending", BodyDigest: digest}
		return s.delivery, true, nil
	}
	return s.delivery, false, nil
}
func (s *deliveryStore) RecordAlertDeliveryAttempt(_ context.Context, _ string, delivered bool, status int, _ string, _ int) error {
	s.attempts++
	if delivered {
		s.delivery.Status = "delivered"
	}
	s.delivery.ResponseStatus = status
	return nil
}

type staticSecret string

func (s staticSecret) Resolve(context.Context, domain.SecretReference) (string, error) {
	return string(s), nil
}

func TestDelivererSignsCanonicalPayloadAndIsIdempotent(t *testing.T) {
	secret := "test-secret"
	requests := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		timestamp := r.Header.Get("InferCrane-Timestamp")
		mac := hmac.New(sha256.New, []byte(secret))
		_, _ = mac.Write([]byte(timestamp + "."))
		_, _ = mac.Write(body)
		want := "v1=" + hex.EncodeToString(mac.Sum(nil))
		if !hmac.Equal([]byte(r.Header.Get("InferCrane-Signature")), []byte(want)) || r.Header.Get("InferCrane-Delivery") != "delivery-1" {
			t.Fatalf("invalid signature headers: %#v", r.Header)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	store := &deliveryStore{}
	deliverer := Deliverer{Store: store, Secrets: staticSecret(secret), Client: server.Client(), AllowPrivate: true, Now: func() time.Time { return time.Unix(1700000000, 0) }}
	policy := domain.AlertPolicy{ID: "policy", TenantID: "tenant", WebhookURL: server.URL, SecretReferenceID: "secret", MinimumSeverity: "warning", Enabled: true, MaxAttempts: 3}
	finding := domain.DiagnosticFinding{ID: "finding", Severity: "critical", Code: "endpoint_not_serving", EvidenceJSON: `{}`}
	delivery, err := deliverer.Deliver(context.Background(), policy, finding)
	if err != nil || delivery.Status != "delivered" || requests != 1 || store.attempts != 1 {
		t.Fatalf("delivery=%#v requests=%d attempts=%d err=%v", delivery, requests, store.attempts, err)
	}
	store.delivery.Status = "delivered"
	if _, err = deliverer.Deliver(context.Background(), policy, finding); err != nil || requests != 1 {
		t.Fatalf("idempotent delivery requests=%d err=%v", requests, err)
	}
}

func TestDelivererRetriesBoundedlyAndRejectsPrivateDestinations(t *testing.T) {
	requests := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		if requests < 3 {
			http.Error(w, "retry", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	store := &deliveryStore{}
	policy := domain.AlertPolicy{ID: "policy", TenantID: "tenant", WebhookURL: server.URL, SecretReferenceID: "secret", MinimumSeverity: "warning", Enabled: true, MaxAttempts: 3}
	finding := domain.DiagnosticFinding{ID: "finding", Severity: "critical"}
	deliverer := Deliverer{Store: store, Secrets: staticSecret("must-not-leak"), Client: server.Client(), AllowPrivate: true}
	delivery, err := deliverer.Deliver(context.Background(), policy, finding)
	if err != nil || delivery.Status != "delivered" || requests != 3 {
		t.Fatalf("delivery=%#v requests=%d err=%v", delivery, requests, err)
	}

	store = &deliveryStore{}
	deliverer = Deliverer{Store: store, Secrets: staticSecret("must-not-leak"), ResolveIP: func(context.Context, string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("127.0.0.1")}}, nil
	}}
	policy.WebhookURL = "https://internal.example.test/hook"
	if _, err = deliverer.Deliver(context.Background(), policy, finding); err == nil || strings.Contains(err.Error(), "must-not-leak") {
		t.Fatalf("private destination error=%v", err)
	}
}
