package proofreport

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/optimizationreport"
	"github.com/infercrane/infercrane/internal/technique"
)

var update = flag.Bool("update", false, "rewrite the golden renderings in testdata")

func near(value *float64, want float64) bool {
	return value != nil && math.Abs(*value-want) < 1e-6*math.Max(1, math.Abs(want))
}

// A run whose gain a fresh machine confirmed, whose answers held, and whose
// price and traffic volume were recorded states its saving: output per GPU,
// cost per million tokens, and the month at the captured traffic.
func TestAVerifiedRunStatesItsSaving(t *testing.T) {
	report := assemble(t, goodRun("run-a", 16, 0.30))
	if len(report.Workloads) != 1 || len(report.Workloads[0].Runs) != 1 {
		t.Fatalf("workloads = %+v", report.Workloads)
	}
	run := report.Workloads[0].Runs[0]
	if run.Claim.Claim != technique.ClaimShown || len(run.Claim.Missing) != 0 {
		t.Fatalf("claim = %+v", run.Claim)
	}
	saving := run.Saving
	if saving.State != SavingVerified || len(saving.Reasons) != 0 {
		t.Fatalf("saving = %+v", saving)
	}
	if !near(saving.OutputPerGPUChange, 30) || !near(saving.CostPerMillionChange, (1/1.3-1)*100) || !near(saving.GPUHoursChange, (1/1.3-1)*100) {
		t.Fatalf("changes: output %v cost %v hours %v", *saving.OutputPerGPUChange, *saving.CostPerMillionChange, *saving.GPUHoursChange)
	}
	// 1,000 output tokens a second is 2.628 billion a month; $0.80 an hour
	// serves 1,000 tokens/s before and 1,300 after.
	monthly := 1000.0 * 730 * 3600
	before, after := monthly/1e6*(0.8/(1000*3600)*1e6), monthly/1e6*(0.8/(1300*3600)*1e6)
	if !near(saving.MonthlyOutputTokens, monthly) || !near(saving.MonthlyCostBeforeUSD, before) || !near(saving.MonthlyCostAfterUSD, after) || !near(saving.MonthlySavingUSD, before-after) || !near(saving.AnnualSavingUSD, (before-after)*12) {
		t.Fatalf("monthly = %+v", saving)
	}
	if !near(saving.MonthlyGPUHoursBefore, monthly/(1000*3600)) || !near(saving.MonthlyGPUHoursAfter, monthly/(1300*3600)) {
		t.Fatalf("GPU hours = %v → %v", *saving.MonthlyGPUHoursBefore, *saving.MonthlyGPUHoursAfter)
	}
	if report.Summary.VerifiedWorkloads != 1 || !near(report.Summary.MonthlySavingUSD, before-after) || report.Summary.SavingCovers != "1 of 1 workloads" || !strings.HasPrefix(report.Summary.Headline, "Saving verified on 1 of 1 workloads") {
		t.Fatalf("summary = %+v", report.Summary)
	}
	if !report.Finished || report.Period != (Period{From: "2026-11-02", To: "2026-11-03"}) {
		t.Fatalf("finished %v period %+v", report.Finished, report.Period)
	}
}

// Every number carries what it was measured on: both setups' hardware and
// software, the traffic, the baseline and its noise, the evidence, and the
// command that measures it again.
func TestEveryNumberSaysWhereItWasMeasured(t *testing.T) {
	run := assemble(t, goodRun("run-a", 16, 0.30)).Workloads[0].Runs[0]
	for _, setup := range []*Setup{run.Before, run.After} {
		if setup == nil || setup.Accelerator != "L4" || setup.AcceleratorCount == nil || *setup.AcceleratorCount != 1 || setup.Engine != "vllm" || setup.EngineVersion != "0.34.0" || !strings.HasPrefix(setup.ImageDigest, "sha256:") || setup.PriceSource == "" {
			t.Fatalf("setup = %+v", setup)
		}
	}
	if run.Workload.ShapeDigest != "sha256:"+fixtureTrace || run.Workload.RequestsPerRun == nil || run.Load == nil || *run.Load != 16 {
		t.Fatalf("workload = %+v load = %v", run.Workload, run.Load)
	}
	if run.Baseline.Basis != optimizationreport.BasisInterleaved || !run.Baseline.FreshServers || run.Baseline.ControlRuns != 3 || !near(run.Baseline.NoiseFloorPercent, 3) {
		t.Fatalf("baseline = %+v", run.Baseline)
	}
	keys := []string{}
	for _, number := range run.Numbers {
		keys = append(keys, number.Key)
	}
	if !slices.Equal(keys, []string{optimizationreport.MetricTokensPerAccel, optimizationreport.MetricCostPerMillion, optimizationreport.MetricTTFTP95, optimizationreport.MetricTokensPerUser}) {
		t.Fatalf("numbers = %v", keys)
	}
	if !strings.HasPrefix(run.Evidence.ReportDigest, "sha256:") || run.Evidence.ReportPath != "/api/v1/optimization/campaigns/run-a/report" || !strings.Contains(run.Evidence.Verify, "infercrane optimize report verify run.json") || len(run.Evidence.Records) == 0 {
		t.Fatalf("evidence = %+v", run.Evidence)
	}
	want := "infercrane optimize create Qwen/Qwen3-8B --provider aws --region eu-central-1 --gpu L4 --objective throughput --profile chat --workload-fingerprint sha256:" + fixtureTrace + " --target-concurrency 16 --max-ttft-p95-ms 1000 --max-candidates 4"
	if len(run.Rerun) != 3 || run.Rerun[0] != want {
		t.Fatalf("rerun = %q", run.Rerun)
	}
	if len(run.Targets) != 1 || run.Targets[0].Key != "ttft_p95_ms" || !near(run.Targets[0].Before, 420) || !near(run.Targets[0].After, 400) || run.Targets[0].BeforeMet == nil || !*run.Targets[0].AfterMet {
		t.Fatalf("targets = %+v", run.Targets)
	}
}

// A gain one window measured, without a fresh machine measuring it again, is
// "not shown", and no saving is stated for it, however large.
func TestAGainWithoutAFreshMachineIsNotShown(t *testing.T) {
	first := goodRun("run-a", 16, 0.60)
	first.confirmed = false
	report := assemble(t, first)
	run := report.Workloads[0].Runs[0]
	if run.Claim.Claim != technique.ClaimNotShown || !slices.Contains(run.Claim.Missing, "a fresh machine's confirmation of the gain") {
		t.Fatalf("claim = %+v", run.Claim)
	}
	if run.Saving.State != SavingNotShown || run.Saving.MonthlySavingUSD != nil || run.Saving.OutputPerGPUChange != nil || len(run.Saving.Reasons) != 1 || !strings.Contains(run.Saving.Reasons[0], "fresh machine") {
		t.Fatalf("saving = %+v", run.Saving)
	}
	if report.Workloads[0].Verdict != WorkloadNotShown || report.Summary.MonthlySavingUSD != nil || report.Summary.VerifiedWorkloads != 0 || !strings.HasPrefix(report.Summary.Headline, "No saving is verified") {
		t.Fatalf("workload %s summary %+v", report.Workloads[0].Verdict, report.Summary)
	}
	// The measured change itself is still shown as measured.
	if number := run.Numbers[0]; number.Decision != optimizationreport.DecisionImproved || !near(number.ChangePercent, 60) {
		t.Fatalf("number = %+v", number)
	}
}

// A binary that recorded no build commit cannot say what measured the gain,
// so its claim is "not shown" and says so.
func TestAResultWithoutTheBuildCommitIsNotShown(t *testing.T) {
	unstamped := goodRun("run-a", 16, 0.30)
	unstamped.unstamped = true
	run := assemble(t, unstamped).Workloads[0].Runs[0]
	if run.Claim.Claim != technique.ClaimNotShown || !slices.Equal(run.Claim.Missing, []string{"the build commit that measured it"}) || run.Saving.State != SavingNotShown {
		t.Fatalf("claim = %+v saving = %+v", run.Claim, run.Saving)
	}
}

// Answers that did not hold withhold the saving, with every reason named.
func TestAnswersThatDidNotHoldWithholdTheSaving(t *testing.T) {
	failed := goodRun("run-a", 16, 0.30)
	failed.passed = false
	run := assemble(t, failed).Workloads[0].Runs[0]
	if run.Claim.Claim != technique.ClaimNotShown || !slices.Contains(run.Claim.Missing, "answers shown to hold") {
		t.Fatalf("claim = %+v", run.Claim)
	}
	if run.Saving.State != SavingNotShown || len(run.Saving.Reasons) != 2 || !strings.Contains(run.Saving.Reasons[1], "answer-quality verdict is") {
		t.Fatalf("saving = %+v", run.Saving)
	}
}

// A setup that can change answers and could not show them held within 1
// point reads "not shown", and its quality record names the rule.
func TestTheOnePointRuleIsCarriedFromTheAnswerRecord(t *testing.T) {
	evidence := goodRun("run-a", 16, 0.30).evidence(t)
	answers := *evidence.Answers
	answers.Passed, answers.DistributionJSON = false, `{"task_verdict":"task_score_not_shown_held_within_1_point","task_rule":"held_within_1_point","task_servers":2}`
	evidence.Answers = &answers
	evidence.Result.Correctness.Verdict = technique.DecisionUnknown
	evidence.Claim = evidence.Result.Claim()
	run := Assemble(Input{Runs: []RunEvidence{evidence}}).Workloads[0].Runs[0]
	if run.Quality.AnswerRule != "held_within_1_point" || run.Quality.Servers == nil || *run.Quality.Servers != 2 || run.Quality.Prompts == nil || *run.Quality.Prompts != 1000 {
		t.Fatalf("quality = %+v", run.Quality)
	}
	if run.Claim.Claim != technique.ClaimNotShown || !slices.Contains(run.Claim.Missing, "answers shown to hold") || run.Saving.State != SavingNotShown {
		t.Fatalf("claim = %+v saving = %s", run.Claim, run.Saving.State)
	}
}

// Keeping the current setup is no saving, and the report says nothing
// changes.
func TestAKeptSetupHasNoSaving(t *testing.T) {
	kept := goodRun("run-a", 16, 0.01)
	kept.kept = true
	report := assemble(t, kept)
	run := report.Workloads[0].Runs[0]
	if run.Outcome != optimizationreport.OutcomeBaselineKept || run.After != nil || run.Saving.State != SavingNone || report.Workloads[0].Verdict != WorkloadNoSaving {
		t.Fatalf("run = %+v verdict = %s", run, report.Workloads[0].Verdict)
	}
}

// Without a recorded price, cost stays unknown: the capacity saving is
// verified, the dollars are not stated, and Unknown says why.
func TestAMissingPriceKeepsCostUnknown(t *testing.T) {
	unpriced := goodRun("run-a", 16, 0.30)
	unpriced.price = nil
	report := assemble(t, unpriced)
	saving := report.Workloads[0].Runs[0].Saving
	if saving.State != SavingVerified || saving.CostPerMillionChange != nil || saving.MonthlySavingUSD != nil || !near(saving.OutputPerGPUChange, 30) || saving.MonthlyGPUHoursAfter == nil {
		t.Fatalf("saving = %+v", saving)
	}
	if !strings.Contains(saving.MonthlyUnavailableReason, "no sourced hourly price") {
		t.Fatalf("reason = %q", saving.MonthlyUnavailableReason)
	}
	if !slices.ContainsFunc(report.Unknown, func(line string) bool { return strings.Contains(line, "Cost per 1M output tokens") }) || report.Summary.MonthlySavingUSD != nil {
		t.Fatalf("unknown = %q summary = %+v", report.Unknown, report.Summary)
	}
}

// A workload's saving is the smallest across its loads, and only when every
// load verified one; the judge's verdict per load comes with it.
func TestAWorkloadSavingHoldsAtEveryLoad(t *testing.T) {
	report := assemble(t, goodRun("run-high", 64, 0.20), goodRun("run-low", 16, 0.40))
	workload := report.Workloads[0]
	if len(report.Workloads) != 1 || len(workload.Runs) != 2 || *workload.Runs[0].Load != 16 || *workload.Runs[1].Load != 64 {
		t.Fatalf("workloads = %+v", report.Workloads)
	}
	if workload.Verdict != WorkloadVerified || !near(workload.OutputPerGPUChange, 20) || !near(workload.MonthlySavingUSD, *workload.Runs[1].Saving.MonthlySavingUSD) {
		t.Fatalf("workload = %+v", workload)
	}
	if workload.Loads == nil || len(workload.Loads.Bands) != 2 || workload.Loads.Bands[0].Decision != "winner" || workload.LoadsReason != "" {
		t.Fatalf("loads = %+v reason %q", workload.Loads, workload.LoadsReason)
	}

	unconfirmed := goodRun("run-high", 64, 0.20)
	unconfirmed.confirmed = false
	workload = assemble(t, goodRun("run-low", 16, 0.40), unconfirmed).Workloads[0]
	if workload.Verdict != WorkloadPartial || workload.MonthlySavingUSD != nil || workload.OutputPerGPUChange != nil || !strings.Contains(workload.Summary, "not at 64 at once") {
		t.Fatalf("partial workload = %+v", workload)
	}
}

// Workloads add up: the summary's saving is the sum of the workloads that
// verified one, and says how many that is.
func TestWorkloadsAddUp(t *testing.T) {
	other := goodRun("run-b", 16, 0.10)
	other.model = "deepseek-ai/DeepSeek-V4-Flash"
	unshown := goodRun("run-c", 16, 0.10)
	unshown.model, unshown.confirmed = "zai-org/GLM-5", false
	report := assemble(t, goodRun("run-a", 16, 0.30), other, unshown)
	if len(report.Workloads) != 3 || report.Summary.VerifiedWorkloads != 2 || report.Summary.SavingCovers != "2 of 3 workloads" {
		t.Fatalf("summary = %+v", report.Summary)
	}
	sum := *report.Workloads[0].MonthlySavingUSD + *report.Workloads[1].MonthlySavingUSD
	if !near(report.Summary.MonthlySavingUSD, sum) || !near(report.Summary.AnnualSavingUSD, sum*12) || !near(report.Summary.OutputPerGPUChange, 10) {
		t.Fatalf("summary = %+v want %v", report.Summary, sum)
	}
}

// A run on CPUs never counts as a saving.
func TestASimulatedRunNeverCounts(t *testing.T) {
	cpu := goodRun("run-a", 16, 0.30)
	cpu.gpu = "SIMULATED-CPU"
	report := assemble(t, cpu)
	run := report.Workloads[0].Runs[0]
	if !run.Simulated || run.Saving.State != SavingNotShown || !strings.Contains(run.Saving.Reasons[0], "simulated accelerator") || !slices.ContainsFunc(report.Limits, func(line string) bool { return strings.Contains(line, "proves the pipeline") }) {
		t.Fatalf("run = %+v limits = %q", run.Saving, report.Limits)
	}
}

// The same records give the same report and digest; a changed record gives
// another digest.
func TestTheDigestFollowsTheRecords(t *testing.T) {
	first, second := assemble(t, goodRun("run-a", 16, 0.30)), assemble(t, goodRun("run-a", 16, 0.30))
	if first.ReportDigest == "" || first.ReportDigest != second.ReportDigest {
		t.Fatalf("digests %s %s", first.ReportDigest, second.ReportDigest)
	}
	if changed := assemble(t, goodRun("run-a", 16, 0.31)); changed.ReportDigest == first.ReportDigest {
		t.Fatal("a changed measurement kept the digest")
	}
}

// A finished report is signed; it verifies as saved, with whitespace changed,
// and against the trusted key, and any change to it fails.
func TestASignedReportVerifies(t *testing.T) {
	seed := sha256.Sum256([]byte("proof report test key"))
	key := ed25519.NewKeyFromSeed(seed[:])
	document, err := Sign(assemble(t, goodRun("run-a", 16, 0.30)), key)
	if err != nil || document.Signature == nil {
		t.Fatalf("sign: %v %+v", err, document)
	}
	saved, _ := json.MarshalIndent(document, "", "  ")
	verified, err := Verify(saved, key.Public().(ed25519.PublicKey))
	if err != nil || !verified.Pinned || verified.Digest != document.Report.ReportDigest || !slices.Equal(verified.Runs, []string{"run-a"}) {
		t.Fatalf("verify: %v %+v", err, verified)
	}
	tampered := strings.Replace(string(saved), `"state": "verified"`, `"state": "not_shown"`, 1)
	if tampered == string(saved) {
		t.Fatal("the fixture has no verified saving to tamper with")
	}
	if _, err = Verify([]byte(tampered), nil); err == nil || !strings.Contains(err.Error(), "the report was changed") {
		t.Fatalf("tampered: %v", err)
	}
	other := ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize))
	if _, err = Verify(saved, other.Public().(ed25519.PublicKey)); err == nil || !strings.Contains(err.Error(), "not the key you trust") {
		t.Fatalf("other key: %v", err)
	}
	if _, err = Verify([]byte(`{"report":{"schema_version":"x"}}`), nil); err == nil || !strings.Contains(err.Error(), "unsupported report schema") {
		t.Fatalf("schema: %v", err)
	}
}

// A report is not signed before every run finished, nor without a key; it
// says which.
func TestAnUnsignedReportSaysWhy(t *testing.T) {
	report := assemble(t, goodRun("run-a", 16, 0.30))
	document, _ := Sign(report, nil)
	if document.Signature != nil || document.UnsignedReason != UnsignedNoSigningKey {
		t.Fatalf("no key: %+v", document)
	}
	report.Finished = false
	document, _ = Sign(report, ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize)))
	if document.Signature != nil || document.UnsignedReason != UnsignedNotFinished {
		t.Fatalf("unfinished: %+v", document)
	}
	saved, _ := json.Marshal(document)
	if _, err := Verify(saved, nil); err == nil || !strings.Contains(err.Error(), "a run has not finished") {
		t.Fatalf("verify unsigned: %v", err)
	}
}

// A request names 1 to 16 distinct runs and one line of text each.
func TestARequestIsChecked(t *testing.T) {
	many := make([]string, MaxRuns+1)
	for index := range many {
		many[index] = "run-" + string(rune('a'+index))
	}
	for name, request := range map[string]Request{
		"none":      {},
		"too many":  {CampaignIDs: many},
		"twice":     {CampaignIDs: []string{"run-a", "run-a"}},
		"two lines": {Title: "a\nb", CampaignIDs: []string{"run-a"}},
		"too long":  {PreparedFor: strings.Repeat("x", MaxTextLength+1), CampaignIDs: []string{"run-a"}},
	} {
		if _, err := request.Check(); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("%s: %v", name, err)
		}
	}
	checked, err := Request{Title: "  Proof for Example Labs ", CampaignIDs: []string{" run-a ", "", "run-b"}}.Check()
	if err != nil || checked.Title != "Proof for Example Labs" || !slices.Equal(checked.CampaignIDs, []string{"run-a", "run-b"}) {
		t.Fatalf("checked = %+v %v", checked, err)
	}
}

// Load reads every run inside the tenant: another tenant's run is not found,
// and a binary built by `go test` records no commit, so its claim reads "not
// shown" and says why.
func TestLoadReadsInsideTheTenant(t *testing.T) {
	records := newStore("tenant-a", goodRun("run-a", 16, 0.30))
	report, err := Load(t.Context(), records, "tenant-a", Request{CampaignIDs: []string{"run-a"}})
	if err != nil {
		t.Fatal(err)
	}
	run := report.Workloads[0].Runs[0]
	if run.Outcome != optimizationreport.OutcomeWinnerSelected || run.Quality.Prompts == nil || run.Claim.Claim != technique.ClaimNotShown || !slices.Contains(run.Claim.Missing, "the build commit that measured it") {
		t.Fatalf("run = %+v", run)
	}
	if run.Saving.MonthlyOutputTokens != nil || run.Saving.State != SavingNotShown {
		t.Fatalf("saving = %+v", run.Saving)
	}
	if _, err = Load(t.Context(), records, "tenant-b", Request{CampaignIDs: []string{"run-a"}}); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("other tenant: %v", err)
	}
	if _, err = Load(t.Context(), records, "tenant-a", Request{}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("empty: %v", err)
	}
}

// The renderings escape every text they did not write, and match the
// reviewed golden files.
func TestRenderings(t *testing.T) {
	seed := sha256.Sum256([]byte("proof report test key"))
	unpriced := goodRun("run-c", 16, 0.10)
	unpriced.model, unpriced.price = "zai-org/GLM-5", nil
	unconfirmed := goodRun("run-d", 128, 0.25)
	unconfirmed.confirmed = false
	in := Input{Title: "Proof Report", PreparedFor: "Example Labs"}
	for _, f := range []runFixture{goodRun("run-a", 16, 0.30), goodRun("run-b", 64, 0.20), unpriced, unconfirmed} {
		in.Runs = append(in.Runs, f.evidence(t))
	}
	document, err := Sign(Assemble(in), ed25519.NewKeyFromSeed(seed[:]))
	if err != nil {
		t.Fatal(err)
	}
	markdown := Markdown(document)
	page, err := HTML(document)
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "proof-report.md", markdown)
	golden(t, "proof-report.html", page)
	for _, want := range []string{"Saving verified on", "Verdict per load", "Measure it again", "Unknown, never estimated", "Signed by key"} {
		if !strings.Contains(string(markdown), want) {
			t.Errorf("markdown lacks %q", want)
		}
	}

	hostile := Assemble(Input{Title: "<script>alert(1)</script> [x](javascript:alert(1))", PreparedFor: "a | b", Runs: in.Runs[:1]})
	hostileDocument, _ := Sign(hostile, nil)
	escaped := string(Markdown(hostileDocument))
	if strings.Contains(escaped, "<script>") || strings.Contains(escaped, "[x](") || !strings.Contains(escaped, `\<script\>`) || !strings.Contains(escaped, `a \| b`) {
		t.Fatalf("markdown did not escape: %s", escaped[:200])
	}
	hostilePage, _ := HTML(hostileDocument)
	if strings.Contains(string(hostilePage), "<script>") || strings.Contains(string(hostilePage), "src=") || strings.Contains(string(hostilePage), "http") {
		t.Fatal("the page carries a script or a network reference")
	}
}

func golden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run go test ./internal/proofreport -update)", err)
	}
	if string(want) != string(got) {
		t.Fatalf("%s differs from the golden file; rerun with -update and review the diff", name)
	}
}
