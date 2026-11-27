package optimizationcapability_test

import (
	"context"
	"testing"

	"github.com/infercrane/infercrane/internal/curatedrecipe"
	"github.com/infercrane/infercrane/internal/integration"
	"github.com/infercrane/infercrane/internal/optimizationcapability"
	"github.com/infercrane/infercrane/internal/optimizer"
)

// Every setup the planner proposes for the recorded golden diagnoses passes
// the engine-argument check a deployment of it would meet.
func TestEveryPlannedSetupPassesTheEngineArgumentCheck(t *testing.T) {
	registry, err := integration.V1Catalog()
	if err != nil {
		t.Fatal(err)
	}
	source := optimizer.NewCatalogSource(curatedrecipe.All(), registry.Snapshot())
	manifest, err := optimizer.LoadPlanningGoldenManifest("../optimizationevidence/testdata/golden/planning.json")
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, task := range manifest.Tasks {
		proposal, err := source.Propose(context.Background(), task.Request)
		if err != nil {
			continue
		}
		for _, candidate := range proposal.Candidates {
			runtime := candidate.Deployment.Runtime
			if runtime.Workload.Empty() {
				err = optimizationcapability.ValidateEngineArgs(runtime.Engine, runtime.Args)
			} else if err = optimizationcapability.RefuseUnsafeEngineArgs(runtime.Args); err == nil {
				err = optimizationcapability.RefuseUnsafeEngineArgs(runtime.Workload.Command)
			}
			if err != nil {
				t.Errorf("%s %s: %v", task.ID, candidate.ID, err)
			}
			checked++
		}
	}
	if checked == 0 {
		t.Fatal("no planned setup was checked")
	}
}
