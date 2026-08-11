package controlclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestWaitPreservesDurableOperationOnContextTimeout(t *testing.T) {
	cancelled := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/operations/op-1/cancel" {
			cancelled = true
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "op-1", "kind": "deployment.converge", "status": "waiting", "progress": 55})
	}))
	defer server.Close()
	client, err := New(server.URL, "secret", "test")
	if err != nil {
		t.Fatal(err)
	}
	client.PollInterval = time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Millisecond)
	defer cancel()
	_, err = client.Wait(ctx, "op-1")
	if err == nil || !strings.Contains(err.Error(), "operation op-1") || !strings.Contains(err.Error(), "durable operation continues") {
		t.Fatalf("Wait() error = %v", err)
	}
	if cancelled {
		t.Fatal("local wait cancellation called the operation cancel endpoint")
	}
}

func TestTypedAPIError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(409)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": "conflict", "message": "busy", "retryable": false}})
	}))
	defer server.Close()
	client, _ := New(server.URL, "secret", "test")
	_, _, err := client.Deployment(context.Background(), "qwen")
	typed, ok := err.(*APIError)
	if !ok || typed.Code != "conflict" || typed.Status != 409 {
		t.Fatalf("error = %#v", err)
	}
}
