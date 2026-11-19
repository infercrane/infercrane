package gateway

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/routes"
)

type capturingRecorder struct {
	mu      sync.Mutex
	records []domain.InferenceRecord
}

func (c *capturingRecorder) RecordRequest(_ context.Context, record domain.InferenceRecord) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.records = append(c.records, record)
	return nil
}

func TestCustomerCloudEndpointIsRefusedNotForwardedAnywhere(t *testing.T) {
	forwarded := false
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { forwarded = true }))
	defer upstream.Close()
	directory := routes.New()
	// Even a stale route under the alias must not carry its prompts.
	directory.Put(routes.Snapshot{TenantID: "team", DeploymentID: "d", Alias: "support-bot", UpstreamModel: "m", RouterURL: upstream.URL})
	directory.SetResidentForTenant("team", "support-bot", true)
	principal := domain.Principal{ID: "app", TenantID: "team", Role: "viewer"}
	handler := (&Gateway{Routes: directory, Authenticator: fakeAuthenticator{principal: principal}}).Handler()
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"support-bot","messages":[{"role":"user","content":"secret"}]}`))
	request.Header.Set("Authorization", "Bearer key")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden || !strings.Contains(response.Body.String(), "residency_error") || forwarded {
		t.Fatalf("response=%d %s forwarded=%v", response.Code, response.Body.String(), forwarded)
	}
	// Another tenant's endpoint of the same name is unaffected.
	if directory.ResidentForTenant("other", "support-bot") {
		t.Fatal("residency leaked across tenants")
	}
	directory.SetResidentForTenant("team", "support-bot", false)
	if aliases := directory.ResidentAliasesForTenant("team"); len(aliases) != 0 {
		t.Fatalf("aliases=%v", aliases)
	}
}

func TestHashKeyKeysSessionAndPromptHashes(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"m","choices":[],"usage":{"prompt_tokens":3,"completion_tokens":1}}`))
	}))
	defer upstream.Close()
	directory := routes.New()
	directory.Put(routes.Snapshot{TenantID: "team", DeploymentID: "d", Alias: "coder", UpstreamModel: "m", RouterURL: upstream.URL})
	recorder := &capturingRecorder{}
	key := []byte("a key that never leaves the agent's machine")
	handler := (&Gateway{Routes: directory, APIKey: "bootstrap-secret", HashKey: key, Recorder: recorder, Authenticator: fakeAuthenticator{principal: domain.Principal{ID: "app", TenantID: "team", Role: "viewer"}}}).Handler()
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"coder","messages":[{"role":"user","content":"hi"}]}`))
	request.Header.Set("Authorization", "Bearer key")
	request.Header.Set("X-InferCrane-Session-ID", "session-1")
	handler.ServeHTTP(httptest.NewRecorder(), request)
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte("session-1"))
	if len(recorder.records) != 1 || recorder.records[0].SessionIDHash != hex.EncodeToString(mac.Sum(nil)) {
		t.Fatalf("records=%+v", recorder.records)
	}
}
