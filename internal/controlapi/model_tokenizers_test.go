package controlapi

import (
	"context"
	"strings"
	"testing"

	"github.com/infercrane/infercrane/internal/optimizer"
)

// A proposal whose setups serve published checkpoints beside the starting
// setup's model records each model's tokenizer digest from the server's own
// reading; a client's value is dropped, and a proposal of one model records
// none.
func TestAProposalRecordsTheTokenizersOfTheModelsItServes(t *testing.T) {
	base := [2]string{"Qwen/Qwen3-8B", "b968826d9c46dd6066d109eabc6255188de91218"}
	qwen := [2]string{"Qwen/Qwen3-8B-FP8", "220b46e3b2180893580a4454f21f22d3ebb187d3"}
	nvidia := [2]string{"nvidia/Qwen3-8B-FP8", "2cebc4c89e25abc17668c81b01dceaf3d8b914d5"}
	serving := func(models ...[2]string) func(optimizer.Request) (optimizer.Proposal, error) {
		return func(optimizer.Request) (optimizer.Proposal, error) {
			var proposal optimizer.Proposal
			for _, model := range models {
				var candidate optimizer.Candidate
				candidate.Deployment.Model.ID, candidate.Deployment.Model.Revision = model[0], model[1]
				proposal.Candidates = append(proposal.Candidates, candidate)
			}
			return proposal, nil
		}
	}
	key := func(model [2]string) string { return optimizer.TokenizerKey(model[0], model[1]) }
	vouched := optimizer.Request{ModelTokenizers: map[string]string{key(base): "sha256:" + strings.Repeat("c", 64), key(nvidia): "sha256:" + strings.Repeat("c", 64)}}

	if got := (API{}).withModelTokenizers(context.Background(), vouched, serving(base, nvidia)); got.ModelTokenizers != nil {
		t.Fatalf("a client's digests were kept: %v", got.ModelTokenizers)
	}
	resolver, _ := recordedHub(t)
	api := API{ModelCheck: resolver}
	got := api.withModelTokenizers(context.Background(), vouched, serving(base, qwen, nvidia)).ModelTokenizers
	if len(got) != 3 || got[key(base)] == "" || got[key(base)] != got[key(qwen)] || got[key(nvidia)] == got[key(base)] || got[key(nvidia)] == "" {
		t.Fatalf("recorded = %v", got)
	}
	for model, digest := range got {
		if !strings.HasPrefix(digest, "sha256:") || len(digest) != len("sha256:")+64 {
			t.Fatalf("%s: digest %q", model, digest)
		}
	}
	if one := api.withModelTokenizers(context.Background(), optimizer.Request{}, serving(base, base)).ModelTokenizers; one != nil {
		t.Fatalf("one model recorded %v", one)
	}
}
