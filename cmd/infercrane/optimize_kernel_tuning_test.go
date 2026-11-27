package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/kerneltuning"
	"github.com/infercrane/infercrane/internal/optimizationcampaign"
	"github.com/infercrane/infercrane/internal/technique"
)

// kernelTuningFixtures writes a tuned set and two windows in which the
// tuned server serves gain times the control around it.
func kernelTuningFixtures(t *testing.T, gain float64) (string, string, string, domain.KernelConfigSet) {
	t.Helper()
	shape := kerneltuning.FusedMoEShape{Experts: 256, Intermediate: 512, DType: "fp8_w8a8", BlockN: 128, BlockK: 128}
	file, err := kerneltuning.NewFile(shape, "NVIDIA_H100_80GB_HBM3", map[int]kerneltuning.TileConfig{
		128: {BlockSizeM: 16, BlockSizeN: 64, BlockSizeK: 128, GroupSizeM: 1, NumWarps: 4, NumStages: 3},
	})
	if err != nil {
		t.Fatal(err)
	}
	set := kerneltuning.NewSet("vllm", "0.34.0", "NVIDIA_H100_80GB_HBM3", []domain.KernelConfigFile{file})
	dir := t.TempDir()
	write := func(name string, value any) string {
		body, _ := json.Marshal(value)
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, body, 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	window := func(name string) optimizationcampaign.KernelTuningWindow {
		record := optimizationcampaign.KernelTuningWindow{Schema: optimizationcampaign.KernelTuningSchema, Window: name, Runtime: "vllm", RuntimeVersion: "0.34.0",
			Machine: optimizationcampaign.KernelTuningMachine{GPU: "NVIDIA H100 80GB HBM3", Driver: "620.70.06"}, SetDigest: set.Digest, Files: []string{file.Name}, Cells: []string{"short/128"}}
		controls := []float64{4000, 4012, 3990, 4004, 3998}
		for index := range 9 {
			value := controls[index/2]
			run := optimizationcampaign.KernelTuningRun{Arm: "control", Engine: kerneltuning.EngineReading{Backend: "TRITON", DefaultFor: []string{file.Name}}}
			if index%2 == 1 {
				value = (controls[index/2] + controls[index/2+1]) / 2 * gain * (1 + 0.002*float64(index%3-1))
				run.Arm, run.Engine = "candidate", kerneltuning.EngineReading{Backend: "TRITON", ConfigFiles: []string{kerneltuning.MountPath + "/" + file.Name}}
			}
			run.Cells = map[string]optimizationcampaign.KernelTuningCellRun{"short/128": {Requests: 700, OutputTokenThroughput: &value}}
			record.Runs = append(record.Runs, run)
		}
		if name == "first" {
			record.Quality = &optimizationcampaign.KernelTuningQuality{Answers: map[string][]optimizationcampaign.KernelTuningAnswer{}}
			for index := range 200 {
				id := fmt.Sprintf("gsm8k:%d", index)
				record.Quality.Items = append(record.Quality.Items, optimizationcampaign.KernelTuningTaskItem{ID: id, Gold: "7"})
				for _, arm := range []string{"control-1", "candidate-1", "control-2"} {
					record.Quality.Answers[arm] = append(record.Quality.Answers[arm], optimizationcampaign.KernelTuningAnswer{ID: id, OK: true, Text: "Answer: 7"})
				}
			}
		}
		return record
	}
	return write("first.json", window("first")), write("fresh.json", window("fresh")), write("kernel-configs.json", set), set
}

func TestOptimizeEvidenceKernelTuningJudgesRecordedWindows(t *testing.T) {
	first, fresh, setPath, set := kernelTuningFixtures(t, 1.06)
	output, err := captureStdout(t, func() error {
		return optimizeCommand(context.Background(), []string{"evidence", "kernel-tuning", "--file", first, "--fresh", fresh, "--set", setPath, "--output", "json"})
	})
	if err != nil {
		t.Fatalf("kernel-tuning: %v\n%s", err, output)
	}
	var verdict optimizationcampaign.KernelTuningVerdict
	if err = json.Unmarshal([]byte(output), &verdict); err != nil {
		t.Fatalf("decode: %v\n%s", err, output)
	}
	if verdict.Decision != optimizationcampaign.KernelTuningPropose || verdict.SetDigest != set.Digest || verdict.Cells[0].Decision != optimizationcampaign.KernelTuningConfirmed {
		t.Fatalf("verdict = %+v", verdict)
	}
	// The evidence file is written beside the verdict, and every result names its sha256.
	evidenceDir := t.TempDir()
	if _, err = captureStdout(t, func() error {
		return optimizeCommand(context.Background(), []string{"evidence", "kernel-tuning", "--file", first, "--fresh", fresh, "--evidence-dir", evidenceDir, "--output", "json"})
	}); err != nil {
		t.Fatal(err)
	}
	proof, err := os.ReadFile(filepath.Join(evidenceDir, kerneltuning.EvidenceKernelInvocation+".json"))
	if err != nil {
		t.Fatal(err)
	}
	confirmation, err := os.ReadFile(filepath.Join(evidenceDir, technique.EvidenceFreshMachineConfirmation+".json"))
	if err != nil {
		t.Fatal(err)
	}
	sum, freshSum := sha256.Sum256(proof), sha256.Sum256(confirmation)
	for _, result := range verdict.Results {
		named := map[string]string{}
		for _, item := range result.Evidence {
			named[item.Name] = item.Digest
		}
		if named[kerneltuning.EvidenceKernelInvocation] != "sha256:"+hex.EncodeToString(sum[:]) {
			t.Fatalf("result evidence %+v does not name the written proof", result.Evidence)
		}
		if result.Measured.Decision == technique.DecisionImproved && named[technique.EvidenceFreshMachineConfirmation] != "sha256:"+hex.EncodeToString(freshSum[:]) {
			t.Fatalf("result evidence %+v does not name the written fresh window", result.Evidence)
		}
	}
	human, err := captureStdout(t, func() error {
		return optimizeCommand(context.Background(), []string{"evidence", "kernel-tuning", "--file", first, "--fresh", fresh})
	})
	if err != nil || !strings.Contains(human, "Decision    propose") || !strings.Contains(human, "short/128") || !strings.Contains(human, "Task set    no_measurable_difference") {
		t.Fatalf("human = %q, %v", human, err)
	}

	// The windows must name the set given.
	_, _, otherSet, _ := kernelTuningFixtures(t, 1.0)
	other, _ := os.ReadFile(otherSet)
	var changed domain.KernelConfigSet
	_ = json.Unmarshal(other, &changed)
	changed.Files[0].Content = strings.Replace(changed.Files[0].Content, `"num_stages":3`, `"num_stages":4`, 1)
	body, _ := json.Marshal(changed)
	tampered := filepath.Join(t.TempDir(), "tampered.json")
	_ = os.WriteFile(tampered, body, 0o600)
	if _, err = captureStdout(t, func() error {
		return optimizeCommand(context.Background(), []string{"evidence", "kernel-tuning", "--file", first, "--set", tampered})
	}); err == nil || !strings.Contains(err.Error(), "sha256") {
		t.Fatalf("a tampered set was accepted: %v", err)
	}
	if _, err = captureStdout(t, func() error {
		return optimizeCommand(context.Background(), []string{"evidence", "golden", "--file", first, "--fresh", fresh})
	}); err == nil {
		t.Fatal("--fresh was accepted outside kernel-tuning")
	}
	if _, err = captureStdout(t, func() error {
		return optimizeCommand(context.Background(), []string{"evidence", "golden", "--file", first, "--evidence-dir", evidenceDir})
	}); err == nil {
		t.Fatal("--evidence-dir was accepted outside kernel-tuning")
	}
}
