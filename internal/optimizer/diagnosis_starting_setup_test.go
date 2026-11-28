package optimizer

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// A server whose KV cache is full under overload, running the publisher's
// three-draft option, must get a runnable setup with one draft token first.
// A planner run once refused every technique with "The starting setup is a
// Dynamo serving graph": the overload profile let the Dynamo graph come first
// by content hash (fixed by c425fcf6). The diagnosis is synthetic
// (testdata/diagnosis) and trips the same kv_cache.saturation rule. The
// control plane is configured for Dynamo, so the graph is still a candidate.
func TestAFullKVCacheUnderOverloadYieldsARunnableOneDraftSetup(t *testing.T) {
	body, err := os.ReadFile("testdata/diagnosis/kv-cache-pressure-overload.json")
	if err != nil {
		t.Fatal(err)
	}
	var diagnosis Diagnosis
	if err = json.Unmarshal(body, &diagnosis); err != nil {
		t.Fatal(err)
	}
	for _, profile := range []string{"overload", "throughput", "balanced", "long-generation"} {
		request := Request{ModelIdentity: "Qwen/Qwen3.8-27B-FP8", ModelRevision: "c3a462cc3d38cf351f3b26cbd763469eeb74ef81", Provider: "kubernetes", GPU: "H100",
			Runtimes: []string{"vllm"}, Objective: "throughput", WorkloadProfile: profile, MaxCandidates: 6, IncludeSimulated: true,
			ModelFeatures: []string{FeatureNativeMTP}, ModelConfigDigest: recipeTestDigest, Diagnosis: &diagnosis}
		proposal, err := dynamoCatalogSource(t, "vllm").Propose(context.Background(), request)
		if err != nil {
			t.Fatalf("%s: %v", profile, err)
		}
		if err = ValidateProposal(proposal); err != nil {
			t.Fatalf("%s: %v", profile, err)
		}
		control := proposal.Candidates[0]
		if !control.Deployment.Serving.Empty() {
			t.Fatalf("%s: the starting setup is a serving graph: %+v", profile, control.Deployment.Serving)
		}
		order := reasonOrder(proposal)
		if len(order) < 2 || order[1] != TechniqueSpeculativeMTPOne || proposal.Diagnosis.State != DiagnosisApplied {
			t.Fatalf("%s: order=%v state=%s summary=%s", profile, order, proposal.Diagnosis.State, proposal.Diagnosis.Summary)
		}
		if args := strings.Join(proposal.Candidates[1].Deployment.Runtime.Args, " "); !strings.Contains(args, `"num_speculative_tokens":1`) {
			t.Fatalf("%s: args %s", profile, args)
		}
		for _, technique := range proposal.Diagnosis.Techniques {
			if strings.Contains(technique.Skipped, "Dynamo serving graph") {
				t.Fatalf("%s: %s refused as a serving graph", profile, technique.Technique)
			}
		}
	}
}
