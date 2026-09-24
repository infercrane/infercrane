package kernelresearch

import (
	"strings"
	"testing"
	"time"
)

func TestResearchLoopKeepsImprovementsAndStopsAtTarget(t *testing.T) {
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	intent, err := NewIntent(identity(), Policy{
		Enabled: true, MaxIterations: 10, MaxConsecutiveRejections: 5,
		MaxDurationSeconds: 3600, MaxCostUSD: 5, MinRelativeImprovement: 1.01,
		TargetKernelSpeedup: 1.5, MaxRooflineUtilization: .95, HotspotFraction: .20,
	})
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := Start(intent, now)
	if err != nil {
		t.Fatal(err)
	}
	receipt, first, err := Observe(receipt, attempt(1, 80, now.Add(time.Minute)))
	if err != nil || first.Decision != "keep" || receipt.State != "searching" {
		t.Fatalf("first=%+v receipt=%+v err=%v", first, receipt, err)
	}
	receipt, second, err := Observe(receipt, attempt(2, 65, now.Add(2*time.Minute)))
	if err != nil || second.Decision != "keep" || receipt.State != "target_reached" {
		t.Fatalf("second=%+v receipt=%+v err=%v", second, receipt, err)
	}
	if receipt.BestKernelSpeedup < 1.53 || receipt.BestEndToEndSpeedup <= 1 {
		t.Fatalf("speedups=%f/%f", receipt.BestKernelSpeedup, receipt.BestEndToEndSpeedup)
	}
	if err = ValidateReceipt(intent, receipt); err != nil {
		t.Fatal(err)
	}
}

func TestResearchLoopRejectsIncorrectAndStopsOnPlateau(t *testing.T) {
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	intent, err := NewIntent(identity(), Policy{
		Enabled: true, MaxIterations: 10, MaxConsecutiveRejections: 2,
		MaxDurationSeconds: 3600, MaxCostUSD: 5, MinRelativeImprovement: 1.01,
		TargetKernelSpeedup: 3, MaxRooflineUtilization: .99, HotspotFraction: .10,
	})
	if err != nil {
		t.Fatal(err)
	}
	receipt, _ := Start(intent, now)
	broken := attempt(1, 50, now.Add(time.Minute))
	broken.Correctness.Determinism = false
	receipt, observation, err := Observe(receipt, broken)
	if err != nil || observation.Reason != "five_stage_correctness_failed" {
		t.Fatalf("observation=%+v err=%v", observation, err)
	}
	slow := attempt(2, 101, now.Add(2*time.Minute))
	receipt, observation, err = Observe(receipt, slow)
	if err != nil || observation.Decision != "reject" || receipt.State != "plateau" {
		t.Fatalf("observation=%+v receipt=%+v err=%v", observation, receipt, err)
	}
	if receipt.BestSourceRevision != "" {
		t.Fatalf("incorrect candidate became best: %+v", receipt)
	}
}

func TestResearchLoopFailsClosedOnBudgetAndReceiptTampering(t *testing.T) {
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	intent, err := NewIntent(identity(), Policy{
		Enabled: true, MaxIterations: 2, MaxConsecutiveRejections: 2,
		MaxDurationSeconds: 3600, MaxCostUSD: .05, MinRelativeImprovement: 1.01,
		TargetKernelSpeedup: 2, MaxRooflineUtilization: .9, HotspotFraction: .1,
	})
	if err != nil {
		t.Fatal(err)
	}
	receipt, _ := Start(intent, now)
	over := attempt(1, 50, now.Add(time.Minute))
	over.CostUSD = .10
	receipt, _, err = Observe(receipt, over)
	if err == nil || receipt.State != "budget_exhausted" {
		t.Fatalf("receipt=%+v err=%v", receipt, err)
	}

	intent.Policy.MaxCostUSD = 1
	receipt, _ = Start(intent, now)
	receipt, _, _ = Observe(receipt, attempt(1, 80, now.Add(time.Minute)))
	receipt, _ = Complete(receipt, now.Add(2*time.Minute))
	receipt.BestKernelSpeedup = 99
	if ValidateReceipt(intent, receipt) == nil {
		t.Fatal("tampered receipt accepted")
	}
}

func identity() Identity {
	return Identity{
		InputDigest: "sha256:" + strings.Repeat("1", 64), CandidateID: "candidate-1",
		ProfileArtifactDigest: "sha256:" + strings.Repeat("2", 64),
		ModelRevision:         strings.Repeat("3", 40), RuntimeImageDigest: "sha256:" + strings.Repeat("4", 64),
		Hardware: "nvidia/H200/sm90", WorkloadDigest: "sha256:" + strings.Repeat("5", 64),
	}
}

func attempt(iteration int, candidateUS float64, completed time.Time) Attempt {
	return Attempt{
		Iteration: iteration, SourceRevision: strings.Repeat(string(rune('a'+iteration)), 40),
		Backend: "triton", ArtifactDigest: "sha256:" + strings.Repeat("6", 64),
		Correctness:      Correctness{Smoke: true, ShapeSweep: true, NumericalStability: true, Determinism: true, EdgeCases: true, Sanitizer: true},
		BaselineMedianUS: 100, CandidateMedianUS: candidateUS, RooflineUtilization: .70,
		CostUSD: .01, CompletedAt: completed,
	}
}
