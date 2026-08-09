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
	if command[len(command)-2] != "--retry-max-retries" || command[len(command)-1] != "1" {
		t.Fatalf("command retry contract = %v", command)
	}
}
