package router

import (
	"slices"
	"testing"
)

func TestCommandUsesUpstreamPolicyAndSingleAttempt(t *testing.T) {
	backend := NewVLLM("vllm-router", "secret")
	command := backend.Command("vllm-router", Spec{DeploymentID: "d1", Workers: []string{"http://a", "http://b"}, Strategy: "cache-aware", Host: "127.0.0.1", Port: 19001})
	if !slices.Contains(command, "cache_aware") {
		t.Fatalf("command does not contain upstream policy: %v", command)
	}
	if !slices.Contains(command, "--prometheus-port") || command[len(command)-1] != "0" {
		t.Fatalf("command metrics isolation contract = %v", command)
	}
	if !slices.Contains(command, "--retry-max-retries") {
		t.Fatalf("command retry contract = %v", command)
	}
}
