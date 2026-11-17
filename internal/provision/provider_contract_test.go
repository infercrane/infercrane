package provision

import (
	"context"
	"net/http/httptest"
	"testing"
)

// These contract tests are intentionally adapter-facing. Every provider added
// to InferCrane must prove replay-safe create/adopt, observable identity, and
// idempotent deletion without invoking a real cloud API.
func TestProviderContractSkyPilotLifecycle(t *testing.T) {
	runner := &fakeSkyRunner{}
	provider := SkyPilot{APIKey: "test-worker-key", Runner: runner}
	spec := ReplicaSpec{ExternalKey: "prod-r0", Model: "Qwen/Qwen3-8B", ModelRevision: "immutable", Cloud: "test-cloud", GPU: "test-gpu"}

	first, err := provider.EnsureReplica(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	spec.RequestID = first.RequestID
	second, err := provider.EnsureReplica(context.Background(), spec)
	if err != nil || second.ResourceID != first.ResourceID || runner.launches != 1 {
		t.Fatalf("replay created another resource: first=%+v second=%+v launches=%d err=%v", first, second, runner.launches, err)
	}
	observed, err := provider.ObserveReplica(context.Background(), second, 8000)
	if err != nil || !observed.Exists || observed.State != "ready" || observed.Endpoint == "" {
		t.Fatalf("observation=%+v err=%v", observed, err)
	}
	if err = provider.DeleteReplica(context.Background(), second); err != nil {
		t.Fatal(err)
	}
	if err = provider.DeleteReplica(context.Background(), second); err != nil || runner.downs != 1 {
		t.Fatalf("delete is not idempotent: downs=%d err=%v", runner.downs, err)
	}
}

func TestProviderContractRunPodServerlessLifecycleAndLostResponseAdoption(t *testing.T) {
	fake := newRunPodV2ServerlessFake(t, "Qwen/Qwen3-8B", "immutable")
	fake.dropNextCreate = true
	server := httptest.NewServer(fake)
	defer server.Close()

	provider := RunPodServerless{APIKey: "test-provider-key", BaseURL: server.URL, TemplateID: fakeServerlessTemplateID, Client: server.Client()}
	spec := ServerlessEndpointSpec{ExternalKey: "deployment-revision", Model: "Qwen/Qwen3-8B", ModelRevision: "immutable", GPU: "L40S", WorkersMax: 2}
	if _, err := provider.EnsureEndpoint(context.Background(), spec); err == nil {
		t.Fatal("lost provider response was not surfaced")
	}
	adopted, err := provider.EnsureEndpoint(context.Background(), spec)
	if createCount, _, _, _ := fake.snapshot(); err != nil || adopted.ID != "endpoint-1" || createCount != 1 {
		t.Fatalf("provider response loss created a duplicate: endpoint=%+v creates=%d err=%v", adopted, createCount, err)
	}
	if err = provider.DeleteEndpoint(context.Background(), adopted.ID); err != nil {
		t.Fatal(err)
	}
	if err = provider.DeleteEndpoint(context.Background(), adopted.ID); err != nil {
		// The fake returns 404 on the replay, which the adapter must treat as success.
		t.Fatal(err)
	}
	if _, deleteCount, endpointCount, unexpected := fake.snapshot(); deleteCount != 1 || endpointCount != 0 || len(unexpected) != 0 {
		t.Fatalf("cleanup incomplete: deletes=%d endpoints=%d unexpected=%v", deleteCount, endpointCount, unexpected)
	}
}
