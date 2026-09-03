package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/infercrane/infercrane/internal/config"
)

func TestModelAPIPublishSendsExactManifestToOperatorRoute(t *testing.T) {
	directory := t.TempDir()
	manifestPath := filepath.Join(directory, "offer.json")
	manifest := []byte(`{"id":"offer-deepseek-v4","supplier":"deepseek"}`)
	if err := os.WriteFile(manifestPath, manifest, 0o600); err != nil {
		t.Fatal(err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/admin/model-api/offers" {
			t.Fatalf("request=%s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer operator-token" {
			t.Fatalf("authorization=%q", r.Header.Get("Authorization"))
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		var got, want map[string]any
		if err = json.Unmarshal(body, &got); err != nil {
			t.Fatalf("request body=%q err=%v", body, err)
		}
		if err = json.Unmarshal(manifest, &want); err != nil {
			t.Fatal(err)
		}
		if got["id"] != want["id"] || got["supplier"] != want["supplier"] {
			t.Fatalf("request=%v want=%v", got, want)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"offer":{"id":"offer-deepseek-v4"}}`)
	}))
	defer server.Close()

	output, err := captureStdout(t, func() error {
		return modelAPICommand(context.Background(), config.Config{ControlURL: server.URL, APIKey: "operator-token"}, []string{"publish", "offer", "--file", manifestPath, "--output", "json"})
	})
	if err != nil {
		t.Fatal(err)
	}
	var response map[string]any
	if err = json.Unmarshal([]byte(output), &response); err != nil || response["offer"] == nil {
		t.Fatalf("output=%q err=%v", output, err)
	}
}

func TestModelAPIPublishRejectsInvalidManifestBeforeRequest(t *testing.T) {
	manifestPath := filepath.Join(t.TempDir(), "invalid.json")
	if err := os.WriteFile(manifestPath, []byte(`{"id":`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := modelAPICommand(context.Background(), config.Config{}, []string{"publish", "offer", "--file", manifestPath}); err == nil {
		t.Fatal("expected invalid JSON to be rejected")
	}
}

func TestModelAPIPublishSupportsImmutableTargetBinding(t *testing.T) {
	manifestPath := filepath.Join(t.TempDir(), "binding.json")
	if err := os.WriteFile(manifestPath, []byte(`{"id":"binding-qwen38-runpod-r1"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/admin/model-api/target-bindings" {
			t.Fatalf("request=%s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"target_binding":{"id":"binding-qwen38-runpod-r1"}}`)
	}))
	defer server.Close()
	if err := modelAPICommand(context.Background(), config.Config{ControlURL: server.URL, APIKey: "operator-token"}, []string{"publish", "target-binding", "--file", manifestPath}); err != nil {
		t.Fatal(err)
	}
}
