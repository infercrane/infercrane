package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/infercrane/infercrane/internal/config"
)

func TestDeployCLIOnlySubmitsControlPlaneRequest(t *testing.T) {
	var path, key string
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, key = r.URL.Path, r.Header.Get("Idempotency-Key")
		if r.Header.Get("Authorization") != "Bearer secret" {
			t.Fatal("missing API authentication")
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"operation":{"id":"op-1","status":"pending"}}`))
	}))
	defer server.Close()

	err := deployAPICommand(context.Background(), config.Config{ControlURL: server.URL, APIKey: "secret"}, "deploy", []string{"Qwen/Qwen3-8B", "--cloud", "runpod", "--gpu", "L40S", "--min", "1", "--max", "4", "--idempotency-key", "release-1"})
	if err != nil || path != "/api/v1/deployments" || key != "release-1" || body["cloud"] != "runpod" || body["max_replicas"] != float64(4) {
		t.Fatalf("path=%s key=%s body=%#v err=%v", path, key, body, err)
	}
}

func TestPrimaryDeployPathDefaultsToRunPodL40S(t *testing.T) {
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte(`{"operation":{"id":"op-1","status":"pending"}}`))
	}))
	defer server.Close()

	err := deployAPICommand(context.Background(), config.Config{ControlURL: server.URL, APIKey: "secret"}, "deploy", []string{"Qwen/Qwen3-8B", "--idempotency-key", "release-1"})
	if err != nil || body["cloud"] != "runpod" || body["gpu"] != "L40S" {
		t.Fatalf("body=%#v err=%v", body, err)
	}
}

func TestServerlessDeployDefaultsToZeroMinimumWorkers(t *testing.T) {
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte(`{"operation":{"id":"op-1","status":"pending"}}`))
	}))
	defer server.Close()

	err := deployAPICommand(context.Background(), config.Config{ControlURL: server.URL, APIKey: "secret"}, "deploy", []string{"Qwen/Qwen3-8B", "--compute", "serverless", "--max", "4", "--idempotency-key", "release-serverless"})
	minimum, hasMinimum := body["min_replicas"]
	if err != nil || body["compute_mode"] != "serverless" || (hasMinimum && minimum != float64(0)) || body["max_replicas"] != float64(4) || body["cloud"] != "runpod" || body["gpu"] != "L40S" {
		t.Fatalf("body=%#v err=%v", body, err)
	}
}

func TestDeleteCLIOnlySubmitsControlPlaneRequest(t *testing.T) {
	var method, path string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, path = r.Method, r.URL.Path
		_, _ = w.Write([]byte(`{"operation":{"id":"op-delete","status":"pending"}}`))
	}))
	defer server.Close()

	err := deleteAPICommand(context.Background(), config.Config{ControlURL: server.URL, APIKey: "secret"}, []string{"qwen prod", "--yes", "--idempotency-key", "delete-1"})
	if err != nil || method != http.MethodDelete || path != "/api/v1/deployments/qwen prod" {
		t.Fatalf("method=%s path=%s err=%v", method, path, err)
	}
}
