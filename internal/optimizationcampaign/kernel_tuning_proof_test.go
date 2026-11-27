package optimizationcampaign

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/infercrane/infercrane/internal/kerneltuning"
	"github.com/infercrane/infercrane/internal/qualitycheck"
	"github.com/infercrane/infercrane/internal/technique"
)

const tunedFile = "E=256,N=512,device_name=NVIDIA_H100_80GB_HBM3,dtype=fp8_w8a8,block_shape=[128,128].json"

var tunedReading = kerneltuning.EngineReading{Backend: "TRITON", ConfigFiles: []string{kerneltuning.MountPath + "/" + tunedFile}}
var defaultReading = kerneltuning.EngineReading{Backend: "TRITON", DefaultFor: []string{tunedFile}}

// proofWindow builds a bracketed window: control output follows controls in
// run order, and each candidate run is gain times the mean of the control
// runs around it, times wiggle[i] for small run noise.
func proofWindow(window string, cells []string, controls []float64, gain map[string]float64, wiggle []float64) KernelTuningWindow {
	record := KernelTuningWindow{Schema: KernelTuningSchema, Window: window, Machine: KernelTuningMachine{GPU: "NVIDIA H100 80GB HBM3", Driver: "620.70.06"},
		Runtime: "vllm", RuntimeVersion: "0.34.0", SetDigest: "sha256:" + strings.Repeat("a", 64), Files: []string{tunedFile}, Cells: cells}
	for index := range 2*measurementRunsPerArm + 1 {
		run := KernelTuningRun{Arm: "control", Engine: defaultReading, Cells: map[string]KernelTuningCellRun{}}
		if index%2 == 1 {
			run.Arm, run.Engine = "candidate", tunedReading
		}
		for _, cell := range cells {
			value := controls[index/2]
			if run.Arm == "candidate" {
				value = (controls[index/2] + controls[index/2+1]) / 2 * gain[cell] * wiggle[index/2]
			}
			ttft, tpot := 900.0, 30.0
			if run.Arm == "candidate" {
				tpot = 30.0 / gain[cell]
			}
			run.Cells[cell] = KernelTuningCellRun{Requests: 800, OutputTokenThroughput: &value, TTFTP95MS: &ttft, TPOTP95MS: &tpot}
		}
		record.Runs = append(record.Runs, run)
	}
	return record
}

var steady = []float64{4000, 4010, 3995, 4005, 4000}
var noise = []float64{1.004, 0.997, 1.002, 0.998}

func taskQuality(items int, controlRight, candidateRight func(int) bool) *KernelTuningQuality {
	quality := &KernelTuningQuality{Answers: map[string][]KernelTuningAnswer{}}
	for index := range items {
		quality.Items = append(quality.Items, KernelTuningTaskItem{ID: fmt.Sprintf("gsm8k:%d", index), Gold: "42"})
	}
	answer := func(right func(int) bool) []KernelTuningAnswer {
		var out []KernelTuningAnswer
		for index := range items {
			text := "Working.\nAnswer: 41"
			if right(index) {
				text = "Working.\nAnswer: 42"
			}
			out = append(out, KernelTuningAnswer{ID: fmt.Sprintf("gsm8k:%d", index), OK: true, Text: text})
		}
		return out
	}
	quality.Answers["control-1"], quality.Answers["control-2"], quality.Answers["candidate-1"] = answer(controlRight), answer(controlRight), answer(candidateRight)
	return quality
}

func everyOther(index int) bool { return index%2 == 0 }

func TestKernelTuningProposesOnlyAGainThatHoldsOnAFreshMachine(t *testing.T) {
	cells := []string{"short/128", "short/32"}
	first := proofWindow("first", cells, steady, map[string]float64{"short/128": 1.08, "short/32": 1.0}, noise)
	first.Quality = taskQuality(1000, everyOther, everyOther)
	fresh := proofWindow("fresh", cells, []float64{3700, 3690, 3710, 3700, 3705}, map[string]float64{"short/128": 1.07, "short/32": 1.0}, noise)

	verdict, err := JudgeKernelTuning(first, &fresh)
	if err != nil {
		t.Fatal(err)
	}
	if verdict.Decision != KernelTuningPropose || verdict.Cells[0].Decision != KernelTuningConfirmed || verdict.Cells[1].Decision != DecisionNoMeasurableDifference {
		t.Fatalf("verdict = %+v", verdict)
	}
	if verdict.Quality == nil || verdict.Quality.Verdict != qualitycheck.TaskNoDifference || verdict.IdenticalAnswers["control-1/control-2"] != 1 {
		t.Fatalf("quality = %+v, identical = %v", verdict.Quality, verdict.IdenticalAnswers)
	}
	comparison := verdict.Cells[0].First.Comparison["output_token_throughput"]
	// Two loads judged: each interval is widened for two comparisons and
	// carries the campaign's machine allowance.
	if comparison.Comparisons != 2 || comparison.MachineDifference != machineDifference || comparison.Delta < 0.07 || comparison.Delta > 0.09 {
		t.Fatalf("comparison = %+v", comparison)
	}

	// The first window alone proposes nothing.
	if alone, _ := JudgeKernelTuning(first, nil); alone.Decision != KernelTuningUnknown || alone.Cells[0].Decision != KernelTuningUnknown {
		t.Fatalf("first window alone = %+v", alone)
	}
	// A fresh machine that does not repeat the gain.
	flat := proofWindow("fresh", cells, steady, map[string]float64{"short/128": 1.0, "short/32": 1.0}, noise)
	if verdict, _ = JudgeKernelTuning(first, &flat); verdict.Decision != KernelTuningNoGain || verdict.Cells[0].Decision != KernelTuningNotConfirmed {
		t.Fatalf("not repeated = %+v", verdict)
	}
}

func TestKernelTuningRejectsASlowerLoadOrWorseAnswers(t *testing.T) {
	cells := []string{"short/128", "short/32"}
	first := proofWindow("first", cells, steady, map[string]float64{"short/128": 1.08, "short/32": 0.93}, noise)
	first.Quality = taskQuality(1000, everyOther, everyOther)
	fresh := proofWindow("fresh", cells, steady, map[string]float64{"short/128": 1.08, "short/32": 0.93}, noise)
	if verdict, _ := JudgeKernelTuning(first, &fresh); verdict.Decision != KernelTuningReject || verdict.Cells[1].Decision != DecisionRegressed {
		t.Fatalf("slower load = %+v", verdict)
	}

	first = proofWindow("first", cells, steady, map[string]float64{"short/128": 1.08, "short/32": 1.0}, noise)
	first.Quality = taskQuality(1000, everyOther, func(index int) bool { return index%4 == 0 })
	fresh = proofWindow("fresh", cells, steady, map[string]float64{"short/128": 1.08, "short/32": 1.0}, noise)
	if verdict, _ := JudgeKernelTuning(first, &fresh); verdict.Decision != KernelTuningReject || verdict.Quality.Verdict != qualitycheck.TaskWorse {
		t.Fatalf("worse answers = %+v", verdict)
	}

	// Answers that differ item by item on too few items do not settle: the
	// gain is not proposed.
	first.Quality = taskQuality(40, everyOther, func(index int) bool { return index%2 == 1 })
	if verdict, _ := JudgeKernelTuning(first, &fresh); verdict.Decision != KernelTuningUnknown {
		t.Fatalf("unsettled answers = %+v", verdict)
	}
	first.Quality = nil
	if verdict, _ := JudgeKernelTuning(first, &fresh); verdict.Decision != KernelTuningUnknown {
		t.Fatalf("no answers checked = %+v", verdict)
	}
}

func TestKernelTuningNeedsProofTheTunedFilesServed(t *testing.T) {
	cells := []string{"short/128"}
	gain := map[string]float64{"short/128": 1.08}
	fresh := proofWindow("fresh", cells, steady, gain, noise)
	for name, change := range map[string]func(*KernelTuningWindow){
		"candidate fell back": func(w *KernelTuningWindow) { w.Runs[3].Engine = defaultReading },
		"control read the set": func(w *KernelTuningWindow) {
			w.Runs[4].Engine = kerneltuning.EngineReading{Backend: "TRITON", ConfigFiles: tunedReading.ConfigFiles}
		},
		"control had a shipped file": func(w *KernelTuningWindow) {
			w.Runs[0].Engine = kerneltuning.EngineReading{Backend: "TRITON", ConfigFiles: []string{"/vllm/configs/" + tunedFile}}
		},
		"another backend": func(w *KernelTuningWindow) { w.Runs[1].Engine.Backend = "DEEPGEMM" },
		"a request failed": func(w *KernelTuningWindow) {
			cell := w.Runs[2].Cells["short/128"]
			cell.Failed = 1
			w.Runs[2].Cells["short/128"] = cell
		},
		"control drifted": func(w *KernelTuningWindow) {
			value := 3000.0
			cell := w.Runs[4].Cells["short/128"]
			cell.OutputTokenThroughput = &value
			w.Runs[4].Cells["short/128"] = cell
		},
		"output not counted": func(w *KernelTuningWindow) {
			cell := w.Runs[6].Cells["short/128"]
			cell.OutputTokenThroughput = nil
			w.Runs[6].Cells["short/128"] = cell
		},
	} {
		first := proofWindow("first", cells, steady, gain, noise)
		first.Quality = taskQuality(1000, everyOther, everyOther)
		change(&first)
		verdict, err := JudgeKernelTuning(first, &fresh)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if verdict.Decision != KernelTuningUnknown || verdict.Cells[0].First.Valid || verdict.Cells[0].First.Reason == "" {
			t.Errorf("%s: verdict = %+v", name, verdict)
		}
	}
}

func TestKernelTuningRefusesARecordThatIsNotABracketedWindow(t *testing.T) {
	cells := []string{"short/128"}
	gain := map[string]float64{"short/128": 1.08}
	for name, change := range map[string]func(*KernelTuningWindow){
		"three candidate runs": func(w *KernelTuningWindow) { w.Runs = w.Runs[:7] },
		"two controls in a row": func(w *KernelTuningWindow) {
			w.Runs[1].Arm = "control"
		},
		"a cell missing":  func(w *KernelTuningWindow) { delete(w.Runs[5].Cells, "short/128") },
		"other schema":    func(w *KernelTuningWindow) { w.Schema = "v0" },
		"no set named":    func(w *KernelTuningWindow) { w.Files = nil },
		"labelled fresh":  func(w *KernelTuningWindow) { w.Window = "fresh" },
		"too many loads":  func(w *KernelTuningWindow) { w.Cells = make([]string, 9) },
		"no loads at all": func(w *KernelTuningWindow) { w.Cells = nil },
	} {
		first := proofWindow("first", cells, steady, gain, noise)
		change(&first)
		if _, err := JudgeKernelTuning(first, nil); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	first := proofWindow("first", cells, steady, gain, noise)
	other := proofWindow("fresh", cells, steady, gain, noise)
	other.SetDigest = "sha256:" + strings.Repeat("b", 64)
	if _, err := JudgeKernelTuning(first, &other); err == nil {
		t.Fatal("a fresh window of another set was accepted")
	}
}

func TestKernelTuningWarnsOfSlowerTokens(t *testing.T) {
	cells := []string{"short/128"}
	first := proofWindow("first", cells, steady, map[string]float64{"short/128": 1.08}, noise)
	first.Quality = taskQuality(1000, everyOther, everyOther)
	for index := 1; index < len(first.Runs); index += 2 {
		slow := 1400.0
		cell := first.Runs[index].Cells["short/128"]
		cell.TTFTP95MS = &slow
		first.Runs[index].Cells["short/128"] = cell
	}
	fresh := proofWindow("fresh", cells, steady, map[string]float64{"short/128": 1.08}, noise)
	verdict, err := JudgeKernelTuning(first, &fresh)
	if err != nil || verdict.Decision != KernelTuningPropose || len(verdict.Warnings) != 1 || !strings.Contains(verdict.Warnings[0], "ttft_p95_ms") {
		t.Fatalf("verdict = %+v, %v", verdict, err)
	}
}

func TestKernelTuningSaysWhetherAnswersChangeOneAtATime(t *testing.T) {
	sequential := func(candidateText string, againText string) *KernelTuningSequential {
		set := &KernelTuningSequential{Prompts: []string{"a", "b"}, Answers: map[string][]KernelTuningAnswer{}}
		for _, id := range set.Prompts {
			set.Answers["control-1"] = append(set.Answers["control-1"], KernelTuningAnswer{ID: id, OK: true, Text: "same " + id})
			set.Answers["control-2"] = append(set.Answers["control-2"], KernelTuningAnswer{ID: id, OK: true, Text: againText + id})
			set.Answers["candidate-1"] = append(set.Answers["candidate-1"], KernelTuningAnswer{ID: id, OK: true, Text: candidateText + id})
		}
		return set
	}
	cells := []string{"short/128"}
	for name, test := range map[string]struct {
		set  *KernelTuningSequential
		want string
	}{
		"identical":            {sequential("same ", "same "), AnswersUnchanged},
		"tuned answers differ": {sequential("other ", "same "), AnswersChanged},
		"controls differ":      {sequential("same ", "other "), AnswersUnknown},
	} {
		first := proofWindow("first", cells, steady, map[string]float64{"short/128": 1.0}, noise)
		first.Sequential = test.set
		verdict, err := JudgeKernelTuning(first, nil)
		if err != nil || verdict.ChangesAnswers != test.want {
			t.Errorf("%s: changes_answers = %q (%v), want %q", name, verdict.ChangesAnswers, err, test.want)
		}
	}
	first := proofWindow("first", cells, steady, map[string]float64{"short/128": 1.0}, noise)
	if verdict, _ := JudgeKernelTuning(first, nil); verdict.ChangesAnswers != "" {
		t.Fatalf("no sequential answers, changes_answers = %q", verdict.ChangesAnswers)
	}
}

func TestKernelTuningWritesContractResultsPerLoad(t *testing.T) {
	cells := []string{"short/128", "short/32"}
	first := proofWindow("first", cells, steady, map[string]float64{"short/128": 1.08, "short/32": 1.0}, noise)
	first.Quality = taskQuality(1000, everyOther, everyOther)
	fresh := proofWindow("fresh", cells, steady, map[string]float64{"short/128": 1.08, "short/32": 1.0}, noise)

	verdict, err := JudgeKernelTuning(first, &fresh)
	if err != nil || len(verdict.Results) != 2 {
		t.Fatalf("results = %+v, %v", verdict.Results, err)
	}
	// Without a reproduction block the records stay "not shown" and say why.
	if verdict.Results[0].Claim() != technique.ClaimNotShown || !strings.Contains(strings.Join(verdict.Results[0].Unknown(), " "), "reproduction.command") {
		t.Fatalf("result without reproduction = %+v", verdict.Results[0])
	}
	digest := "sha256:" + strings.Repeat("c", 64)
	first.Reproduction = &KernelTuningReproduction{Command: "kernel_tune_modal.py run", Commit: "6ff48fef", ImageDigest: digest, TrafficShape: "prompts-v2.json kinds.short.levels.128",
		ShapeDigests: map[string]string{"short/128": digest, "short/32": digest}, Weights: "fp8_w8a8 block 128x128", KVCache: "bf16"}
	verdict, err = JudgeKernelTuning(first, &fresh)
	if err != nil {
		t.Fatal(err)
	}
	confirmed, flat := verdict.Results[0], verdict.Results[1]
	if confirmed.Workload.Load != 128 || confirmed.Measured.Decision != technique.DecisionImproved || confirmed.Claim() != technique.ClaimShown || confirmed.Validate() != nil {
		t.Fatalf("confirmed load = %+v (unknown %v, validate %v)", confirmed, confirmed.Unknown(), confirmed.Validate())
	}
	if flat.Workload.Load != 32 || flat.Measured.Decision != technique.DecisionSame || flat.Claim() != technique.ClaimNotShown {
		t.Fatalf("flat load = %+v", flat)
	}
	manifest := kerneltuning.Technique{}.Manifest()
	if manifest.ClaimOf(confirmed) != technique.ClaimShown {
		t.Fatalf("the manifest's own evidence rule refused a complete record: %v", confirmed.Unknown())
	}
	// A record that drops its required evidence is judged by the manifest anyway.
	dropped := confirmed
	dropped.RequiredEvidence = nil
	dropped.Evidence = slices.DeleteFunc(slices.Clone(dropped.Evidence), func(item technique.EvidenceItem) bool { return item.Name == kerneltuning.EvidenceKernelInvocation })
	if dropped.Claim() != technique.ClaimShown || manifest.ClaimOf(dropped) != technique.ClaimNotShown {
		t.Fatalf("a record without its evidence was shown by the manifest")
	}
}

// The same machine's proof alone is never shown: a load the first window
// measured gaining reads "not shown" and names the fresh machine's
// confirmation until a fresh window repeats it, and the confirmed load then
// carries that window's digest.
func TestKernelTuningOnOneMachineIsNotShownUntilAFreshMachineConfirmsIt(t *testing.T) {
	cells := []string{"short/128"}
	gain := map[string]float64{"short/128": 1.08}
	digest := "sha256:" + strings.Repeat("c", 64)
	first := proofWindow("first", cells, steady, gain, noise)
	first.Quality = taskQuality(1000, everyOther, everyOther)
	first.Reproduction = &KernelTuningReproduction{Command: "kernel_tune_modal.py run", Commit: "306b7700", ImageDigest: digest, TrafficShape: "short",
		ShapeDigests: map[string]string{"short/128": digest}, Weights: "fp8_w8a8", KVCache: "bf16"}
	alone, err := JudgeKernelTuning(first, nil)
	if err != nil {
		t.Fatal(err)
	}
	manifest := kerneltuning.Technique{}.Manifest()
	result := alone.Results[0]
	if manifest.ClaimOf(result) != technique.ClaimNotShown || !slices.Contains(result.Unknown(), "evidence."+technique.EvidenceFreshMachineConfirmation) || result.Baseline.Basis != "same-machine" {
		t.Fatalf("one machine: claim %s, unknown %v, basis %s", manifest.ClaimOf(result), result.Unknown(), result.Baseline.Basis)
	}
	fresh := proofWindow("fresh", cells, steady, gain, noise)
	confirmed, err := JudgeKernelTuning(first, &fresh)
	if err != nil {
		t.Fatal(err)
	}
	result = confirmed.Results[0]
	body, err := json.Marshal(fresh)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	index := slices.IndexFunc(result.Evidence, func(item technique.EvidenceItem) bool { return item.Name == technique.EvidenceFreshMachineConfirmation })
	if manifest.ClaimOf(result) != technique.ClaimShown || index < 0 || result.Evidence[index].Digest != "sha256:"+hex.EncodeToString(sum[:]) || result.Baseline.Basis != "same-machine+fresh-servers" {
		t.Fatalf("confirmed: claim %s, unknown %v, evidence %+v", manifest.ClaimOf(result), result.Unknown(), result.Evidence)
	}
}

func TestKernelTuningResultsCarryTheInvocationProof(t *testing.T) {
	cells := []string{"short/128"}
	gain := map[string]float64{"short/128": 1.08}
	digest := "sha256:" + strings.Repeat("c", 64)
	reproduction := &KernelTuningReproduction{Command: "kernel_tune_modal.py run", Commit: "306b7700", ImageDigest: digest, TrafficShape: "short",
		ShapeDigests: map[string]string{"short/128": digest}, Weights: "fp8_w8a8", KVCache: "bf16"}
	first := proofWindow("first", cells, steady, gain, noise)
	first.Quality, first.Reproduction = taskQuality(1000, everyOther, everyOther), reproduction
	fresh := proofWindow("fresh", cells, steady, gain, noise)

	verdict, err := JudgeKernelTuning(first, &fresh)
	if err != nil {
		t.Fatal(err)
	}
	proof := verdict.InvocationProof
	if proof == nil || len(proof.Starts) != 2*(2*measurementRunsPerArm+1) || proof.Schema != KernelInvocationProofSchema {
		t.Fatalf("proof = %+v", proof)
	}
	body, err := proof.Encode()
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	result := verdict.Results[0]
	invocation := slices.IndexFunc(result.Evidence, func(item technique.EvidenceItem) bool { return item.Name == kerneltuning.EvidenceKernelInvocation })
	if !slices.Equal(result.RequiredEvidence, []string{kerneltuning.EvidenceKernelInvocation}) || len(result.Evidence) != 2 || invocation < 0 ||
		result.Evidence[invocation].Digest != "sha256:"+hex.EncodeToString(sum[:]) || result.Validate() != nil || result.Claim() != technique.ClaimShown {
		t.Fatalf("result evidence = %+v (unknown %v, validate %v)", result.Evidence, result.Unknown(), result.Validate())
	}

	// One tuned start that did not read the set: no proof, so no record is shown.
	first.Runs[5].Engine = defaultReading
	verdict, err = JudgeKernelTuning(first, &fresh)
	if err != nil {
		t.Fatal(err)
	}
	if served := verdict.InvocationProof.Starts[5]; served.ServedItsArm || served.Reason == "" {
		t.Fatalf("start 6 = %+v", served)
	}
	for _, result := range verdict.Results {
		if len(result.Evidence) != 0 || !slices.Contains(result.Unknown(), "evidence."+kerneltuning.EvidenceKernelInvocation) || result.Claim() != technique.ClaimNotShown {
			t.Fatalf("a result without the proof = %+v", result)
		}
	}
}
