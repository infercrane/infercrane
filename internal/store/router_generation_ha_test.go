package store

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/workflows"
)

func TestRouterGenerationNumbersAreIndependentPerControlPlaneOwner(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, ctx)
	name := "router-generation-ha"
	requestJSON := fmt.Sprintf(`{"name":%q,"model":"Qwen/Qwen3-8B","cloud":"runpod","gpu":"L40S","tenant_id":"global","min_replicas":1,"max_replicas":1}`, name)
	deployment, _, _, err := s.SubmitCloudDeployment(ctx,
		domain.Deployment{Name: name, Model: "Qwen/Qwen3-8B", MinReplicas: 1, MaxReplicas: 1},
		domain.Operation{Kind: workflows.ConvergeKind, IdempotencyKey: "router-generation-ha", RequestJSON: requestJSON},
	)
	if err != nil {
		t.Fatal(err)
	}

	start := make(chan struct{})
	errorsByOwner := make(chan error, 2)
	var workers sync.WaitGroup
	for _, owner := range []string{"control-plane-a", "control-plane-b"} {
		owner := owner
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			_, recordErr := s.RecordGeneration(ctx, domain.RouterGeneration{
				DeploymentID:     deployment.ID,
				OwnerID:          owner,
				Generation:       1,
				Strategy:         "round-robin",
				WorkerSetHash:    "same-workers",
				InternalEndpoint: "http://" + owner,
			})
			errorsByOwner <- recordErr
		}()
	}
	close(start)
	workers.Wait()
	close(errorsByOwner)
	for recordErr := range errorsByOwner {
		if recordErr != nil {
			t.Fatalf("concurrent HA router generation: %v", recordErr)
		}
	}

	for _, owner := range []string{"control-plane-a", "control-plane-b"} {
		generation, generationErr := s.ActiveGeneration(ctx, deployment.ID, owner)
		if generationErr != nil {
			t.Fatalf("active generation for %s: %v", owner, generationErr)
		}
		if generation.Generation != 1 || generation.OwnerID != owner {
			t.Fatalf("generation for %s = %#v", owner, generation)
		}
	}
}
