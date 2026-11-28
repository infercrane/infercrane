package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/infercrane/infercrane/internal/config"
)

func TestDeleteSaysWhichUnfinishedDeployItCancelled(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete || r.URL.Path != "/api/v1/deployments/qwen" {
			t.Fatalf("unexpected %s %s", r.Method, r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"operation":{"id":"delete-1","kind":"deployment.delete","status":"pending"},"created":true,"cancelled_operation":{"id":"deploy-1","kind":"deployment.converge","status":"cancelled"}}`))
	}))
	defer server.Close()
	cfg := config.Config{ControlURL: server.URL, APIKey: "secret"}
	output, err := captureStdout(t, func() error {
		return deleteAPICommand(context.Background(), cfg, []string{"qwen", "--yes", "--idempotency-key", "delete-1"})
	})
	if err != nil || !strings.Contains(output, "Cancelled the unfinished deploy operation deploy-1 first") || !strings.Contains(output, "delete-1") {
		t.Fatalf("output=%q err=%v", output, err)
	}
	output, err = captureStdout(t, func() error {
		return deleteAPICommand(context.Background(), cfg, []string{"qwen", "--yes", "--idempotency-key", "delete-1", "--output", "json"})
	})
	var document struct {
		CancelledOperation struct{ ID string } `json:"cancelled_operation"`
	}
	if err != nil || json.Unmarshal([]byte(output), &document) != nil || document.CancelledOperation.ID != "deploy-1" {
		t.Fatalf("output=%q err=%v", output, err)
	}
}
