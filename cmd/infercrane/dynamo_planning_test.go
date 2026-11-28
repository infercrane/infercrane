package main

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/infercrane/infercrane/internal/config"
	"github.com/infercrane/infercrane/internal/curatedrecipe"
	"github.com/infercrane/infercrane/internal/integration"
	"github.com/infercrane/infercrane/internal/optimizer"
)

// The control plane's planner offers a kubernetes-dynamo setup for provider
// kubernetes only for the runtimes it registers Dynamo backends for: those
// with a complete Dynamo image and version pair.
func TestControlPlanePlansDynamoOnlyForItsConfiguredRuntimes(t *testing.T) {
	digest := "nvcr.io/nvidia/ai-dynamo/vllm-runtime@sha256:" + strings.Repeat("b", 64)
	request := optimizer.Request{ModelIdentity: "acme/novel-open-model", ModelRevision: strings.Repeat("a", 40), Provider: "kubernetes", GPU: "H100", Objective: "throughput", IncludeSimulated: true, MaxCandidates: 10}
	for name, test := range map[string]struct {
		cfg  config.Config
		want []string
	}{
		"no Dynamo":     {cfg: config.Config{}},
		"vLLM image":    {cfg: config.Config{DynamoVLLMImageDigest: digest, DynamoVLLMRuntimeVersion: "1.4.0"}, want: []string{"vllm"}},
		"half a pair":   {cfg: config.Config{DynamoSGLangImageDigest: digest}},
		"both runtimes": {cfg: config.Config{DynamoVLLMImageDigest: digest, DynamoVLLMRuntimeVersion: "1.4.0", DynamoSGLangImageDigest: digest, DynamoSGLangRuntimeVersion: "1.4.0"}, want: []string{"sglang", "vllm"}},
	} {
		t.Run(name, func(t *testing.T) {
			registry, err := integration.V1Catalog()
			if err != nil {
				t.Fatal(err)
			}
			if err = configureDynamoPlanning(registry, test.cfg); err != nil {
				t.Fatal(err)
			}
			proposal, err := optimizer.NewCatalogSource(curatedrecipe.All(), registry.Snapshot()).Propose(context.Background(), request)
			if err != nil {
				t.Fatal(err)
			}
			var planned []string
			for _, candidate := range proposal.Candidates {
				if candidate.Deployment.Provider.Adapter == "kubernetes-dynamo" {
					planned = append(planned, candidate.Deployment.Runtime.Engine)
				}
			}
			slices.Sort(planned)
			if !slices.Equal(planned, test.want) {
				t.Fatalf("Dynamo setups for %v, want %v", planned, test.want)
			}
			backends := configuredDynamoRuntimes(test.cfg)
			slices.Sort(backends)
			if !slices.Equal(backends, test.want) {
				t.Fatalf("Dynamo backends would register for %v, want %v", backends, test.want)
			}
		})
	}
}
