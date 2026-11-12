package runtimecontract

import (
	"strings"
	"testing"
)

func validWorkload() Workload {
	return Workload{Image: "registry.example/runtime@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Command: []string{"serve", "--model", "${MODEL}"}, Protocol: "openai", Port: 8000, ReadinessPath: "/health", ModelsPath: "/v1/models", MetricsPath: "/metrics", Cancellation: "http-disconnect", Drain: "connection", ShutdownGraceSeconds: 30}
}

func TestWorkloadValidation(t *testing.T) {
	if err := validWorkload().Validate(); err != nil {
		t.Fatal(err)
	}
	cases := []func(*Workload){
		func(w *Workload) { w.Image = "registry.example/runtime:latest" },
		func(w *Workload) { w.Command = nil },
		func(w *Workload) { w.Protocol = "grpc" },
		func(w *Workload) { w.ReadinessPath = "health" },
		func(w *Workload) { w.ReadinessPath = "/readyz" },
		func(w *Workload) { w.ShutdownGraceSeconds = 0 },
		func(w *Workload) { w.Command = append(w.Command, "--api-key", "${WORKER_API_KEY}") },
	}
	for i, mutate := range cases {
		w := validWorkload()
		mutate(&w)
		if err := w.Validate(); err == nil {
			t.Fatalf("case %d accepted", i)
		}
	}
}

func TestValidateImageAcceptsRegistryPortsAndRejectsMutableReferences(t *testing.T) {
	digest := "@sha256:" + strings.Repeat("a", 64)
	for _, image := range []string{
		"registry.example/runtime" + digest,
		"registry.example/runtime:1.2" + digest,
		"localhost:5001/team/runtime" + digest,
		"registry.internal:5000/team/runtime:v1" + digest,
		"ghcr.io/org/server:server" + digest,
	} {
		if err := ValidateImage(image); err != nil {
			t.Fatalf("pinned image %q was rejected: %v", image, err)
		}
	}
	for _, image := range []string{
		"localhost:5001/team/runtime:latest",
		"localhost:5001/team/runtime",
		"localhost:99999999/team/runtime" + digest,
		":5001/team/runtime" + digest,
		"localhost:5001/team/runtime@sha256:" + strings.Repeat("A", 64),
	} {
		if err := ValidateImage(image); err == nil {
			t.Fatalf("image %q must be rejected", image)
		}
	}
}
