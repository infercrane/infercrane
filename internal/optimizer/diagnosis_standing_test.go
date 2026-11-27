package optimizer

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func standingFixture(t *testing.T, maxCandidates int) Proposal {
	t.Helper()
	body, err := os.ReadFile("testdata/diagnosis/kv-cache-pressure-overload.json")
	if err != nil {
		t.Fatal(err)
	}
	var diagnosis Diagnosis
	if err = json.Unmarshal(body, &diagnosis); err != nil {
		t.Fatal(err)
	}
	// The endpoint runs the publisher's command with its documented option of
	// three draft tokens.
	request := Request{ModelIdentity: "Qwen/Qwen3.8-27B-FP8", ModelRevision: "c3a462cc3d38cf351f3b26cbd763469eeb74ef81", Provider: "kubernetes", GPU: "H100",
		Runtimes: []string{"vllm"}, Objective: "throughput", WorkloadProfile: "overload", MaxCandidates: maxCandidates, IncludeSimulated: true,
		ModelFeatures: []string{FeatureNativeMTP}, ModelConfigDigest: recipeTestDigest, Diagnosis: &diagnosis,
		StartingSetup: &StartingSetup{Deployment: "chat", Revision: 1, Engine: "vllm", Version: "0.22.1", GPU: "H100", GPUCount: 1,
			Args: []string{"--speculative-config", `{"method":"mtp","num_speculative_tokens":3}`}}}
	proposal, err := catalogSource(t).Propose(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if err = ValidateProposal(proposal); err != nil {
		t.Fatal(err)
	}
	return proposal
}

func roleOrder(proposal Proposal) []string {
	var out []string
	for _, candidate := range proposal.Candidates {
		label := candidate.Reason.Role
		if candidate.Reason.Role == ReasonDiagnosis {
			label = candidate.Reason.Technique
		}
		out = append(out, label+" "+strings.Join(candidate.Deployment.Runtime.Args, " "))
	}
	return out
}

// Every list carries the setups that test the plan: the starting setup
// without its speculation (the publisher's plain command here) and a copy
// right after the first choice, and the draft length next to the chosen one
// after the diagnosed setups.
func TestEveryListCarriesThePlainCommandACopyAndTheNeighbouringDraftLength(t *testing.T) {
	proposal := standingFixture(t, 10)
	order := roleOrder(proposal)
	if len(order) < 5 {
		t.Fatalf("order=%q", order)
	}
	want := []string{
		`control --speculative-config {"method":"mtp","num_speculative_tokens":3}`,
		`speculative-mtp-one-draft --speculative-config {"method":"mtp","num_speculative_tokens":1}`,
		`reference `,
		`copy --speculative-config {"method":"mtp","num_speculative_tokens":3} --seed 1`,
	}
	for index, entry := range want {
		if order[index] != entry {
			t.Fatalf("setup %d = %q, want %q (order %q)", index+1, order[index], entry, order)
		}
	}
	last := proposal.Candidates[len(proposal.Candidates)-1]
	if last.Reason.Role != ReasonReference || strings.Join(last.Deployment.Runtime.Args, " ") != `--speculative-config {"method":"mtp","num_speculative_tokens":2}` || !strings.Contains(last.Reason.Summary, "one draft token") {
		t.Fatalf("the neighbouring draft length comes last: %q", order)
	}
	plain, copied := proposal.Candidates[2], proposal.Candidates[3]
	if plain.Reason.Label != "The starting setup without its speculation" || !strings.Contains(plain.Reason.Summary, "publisher's plain command") || !contains(plain.RequiredEvidence, TaskScoreEvidence) {
		t.Fatalf("plain=%+v evidence=%v", plain.Reason, plain.RequiredEvidence)
	}
	if !hasFeature(copied.Features, planCheckFeature, standingCopy) || hasFeature(copied.Features, "diagnosis_technique", standingCopy) || contains(copied.RequiredEvidence, TaskScoreEvidence) {
		t.Fatalf("copy features=%v evidence=%v", copied.Features, copied.RequiredEvidence)
	}
	// The plan's own record still lists diagnosed techniques only.
	for _, technique := range proposal.Diagnosis.Techniques {
		for _, id := range technique.CandidateIDs {
			if id == plain.ID || id == copied.ID {
				t.Fatalf("a standing setup is linked to a diagnosed technique: %+v", technique)
			}
		}
	}
}

// A limit on setups cuts the neighbour and the lower diagnosed choices before
// the plain command and the copy.
func TestALimitCutsTheNeighbourBeforeThePlainCommandAndTheCopy(t *testing.T) {
	order := roleOrder(standingFixture(t, 4))
	if len(order) != 4 || !strings.HasPrefix(order[2], "reference") || !strings.HasPrefix(order[3], "copy") {
		t.Fatalf("order=%q", order)
	}
}

func TestNeighbourDraftsFollowsTheCacheRoom(t *testing.T) {
	room := &CacheRoom{Running: 58, CurrentDrafts: 3, DraftLengths: []DraftRoom{{Drafts: 1, Room: 111}, {Drafts: 2, Room: 77}, {Drafts: 3, Room: 59}}}
	if drafts, why := neighbourDrafts(1, 3, room); drafts != 2 || why != "" {
		t.Fatalf("between one and three: %d %q", drafts, why)
	}
	// A neighbour the cache predicts holds fewer requests than run now is
	// left out.
	tight := &CacheRoom{Running: 90, CurrentDrafts: 3, DraftLengths: []DraftRoom{{Drafts: 2, Room: 77}}}
	if drafts, why := neighbourDrafts(1, 3, tight); drafts != 0 || !strings.Contains(why, "fewer than the 90 running now") {
		t.Fatalf("tight: %d %q", drafts, why)
	}
	// Chosen above the current length: the one between comes first.
	if drafts, _ := neighbourDrafts(3, 1, nil); drafts != 2 {
		t.Fatalf("between one and three, upward: %d", drafts)
	}
	if drafts, why := neighbourDrafts(1, 0, nil); drafts != 2 || why != "" {
		t.Fatalf("from no speculation: %d %q", drafts, why)
	}
}

func TestWithDraftTokensKeepsTheFlagAndItsOtherFields(t *testing.T) {
	args, ok := withDraftTokens([]string{"--max-num-seqs", "512", "--speculative-config", `{"method":"mtp","num_speculative_tokens":1,"draft_tensor_parallel_size":1}`}, 2)
	if !ok || strings.Join(args, " ") != `--max-num-seqs 512 --speculative-config {"method":"mtp","num_speculative_tokens":2,"draft_tensor_parallel_size":1}` {
		t.Fatalf("args=%q", args)
	}
	inline, ok := withDraftTokens([]string{`--speculative-config={"method":"mtp","num_speculative_tokens":3}`}, 2)
	if !ok || inline[0] != `--speculative-config={"method":"mtp","num_speculative_tokens":2}` {
		t.Fatalf("inline=%q", inline)
	}
}
