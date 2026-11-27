package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"text/tabwriter"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/kerneltuning"
	"github.com/infercrane/infercrane/internal/optimizationcampaign"
	"github.com/infercrane/infercrane/internal/technique"
)

// optimizeKernelTuningEvidence judges a kernel-tuning proof's recorded
// windows (tools/kernel-tune) with the campaign judge and prints the
// decision. A decision is not an error; a record that is not a bracketed
// window is.
//
// With evidenceDir, it also writes the evidence files the results name:
// kernel-invocation-proof.json, whose sha256 is the digest on every result,
// and, with a fresh window, fresh-machine-confirmation.json, whose sha256 a
// confirmed load names.
func optimizeKernelTuningEvidence(firstPath, freshPath, setPath, evidenceDir, output string) error {
	read := func(path string) (optimizationcampaign.KernelTuningWindow, error) {
		var window optimizationcampaign.KernelTuningWindow
		body, err := os.ReadFile(path)
		if err != nil {
			return window, fmt.Errorf("read kernel-tuning window: %w", err)
		}
		decoder := json.NewDecoder(strings.NewReader(string(body)))
		decoder.DisallowUnknownFields()
		if err = decoder.Decode(&window); err != nil {
			return window, fmt.Errorf("kernel-tuning window %s: %w", path, err)
		}
		return window, nil
	}
	first, err := read(firstPath)
	if err != nil {
		return err
	}
	var fresh *optimizationcampaign.KernelTuningWindow
	if strings.TrimSpace(freshPath) != "" {
		window, readErr := read(freshPath)
		if readErr != nil {
			return readErr
		}
		fresh = &window
	}
	if strings.TrimSpace(setPath) != "" {
		if err = checkKernelTuningSet(setPath, first); err != nil {
			return err
		}
	}
	verdict, err := optimizationcampaign.JudgeKernelTuning(first, fresh)
	if err != nil {
		return err
	}
	if strings.TrimSpace(evidenceDir) != "" && verdict.InvocationProof != nil {
		body, encodeErr := verdict.InvocationProof.Encode()
		if encodeErr != nil {
			return encodeErr
		}
		if err = os.WriteFile(filepath.Join(evidenceDir, kerneltuning.EvidenceKernelInvocation+".json"), body, 0o600); err != nil {
			return fmt.Errorf("write kernel-invocation proof: %w", err)
		}
	}
	if strings.TrimSpace(evidenceDir) != "" && fresh != nil {
		body, encodeErr := fresh.Encode()
		if encodeErr != nil {
			return encodeErr
		}
		if err = os.WriteFile(filepath.Join(evidenceDir, technique.EvidenceFreshMachineConfirmation+".json"), body, 0o600); err != nil {
			return fmt.Errorf("write fresh-machine confirmation: %w", err)
		}
	}
	if output == "json" {
		return printJSON(verdict)
	}
	fmt.Printf("Kernels tuned for your shapes · %s\n", verdict.SetDigest)
	fmt.Printf("Decision    %s\n", verdict.Decision)
	for _, reason := range verdict.Reasons {
		fmt.Printf("            %s\n", reason)
	}
	fmt.Println()
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "LOAD\tDECISION\tFIRST WINDOW\tFRESH MACHINE")
	for _, cell := range verdict.Cells {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", cell.Cell, cell.Decision, kernelTuningWindowText(&cell.First), kernelTuningWindowText(cell.Fresh))
	}
	if err = w.Flush(); err != nil {
		return err
	}
	if verdict.Quality != nil {
		fmt.Printf("\nTask set    %s: %.1f%% vs %.1f%% right on %d items (difference %+.1f, interval %+.1f to %+.1f points)\n", verdict.Quality.Verdict,
			verdict.Quality.CandidateScore*100, verdict.Quality.ReferenceScore*100, verdict.Quality.Items, verdict.Quality.Difference*100, verdict.Quality.Low*100, verdict.Quality.High*100)
		for _, pair := range []string{"control-1/control-2", "control-1/candidate-1"} {
			if share, ok := verdict.IdenticalAnswers[pair]; ok {
				fmt.Printf("Same text   %s: %.1f%% of answers\n", pair, share*100)
			}
		}
	}
	for _, warning := range verdict.Warnings {
		fmt.Printf("Warning     %s\n", warning)
	}
	return nil
}

// checkKernelTuningSet validates the tuned set as a revision would carry it
// and requires the windows to name it: its digest and its file names.
func checkKernelTuningSet(path string, window optimizationcampaign.KernelTuningWindow) error {
	body, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read tuned set: %w", err)
	}
	var set domain.KernelConfigSet
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&set); err != nil {
		return fmt.Errorf("tuned set %s: %w", path, err)
	}
	if err = kerneltuning.Validate(set); err != nil {
		return fmt.Errorf("tuned set %s: %w", path, err)
	}
	names := make([]string, len(set.Files))
	for index, file := range set.Files {
		names[index] = file.Name
	}
	if set.Digest != window.SetDigest || !slices.Equal(names, window.Files) {
		return fmt.Errorf("the windows measured set %s (%v), not %s (%v)", window.SetDigest, window.Files, set.Digest, names)
	}
	return nil
}

func kernelTuningWindowText(window *optimizationcampaign.KernelTuningWindowVerdict) string {
	switch {
	case window == nil:
		return "not measured"
	case !window.Valid:
		return "unknown: " + window.Reason
	}
	comparison := window.Comparison["output_token_throughput"]
	return fmt.Sprintf("%+.1f%% output (±%.1f%%), %s", comparison.Delta*100, comparison.NoiseFloor*100, comparison.Decision)
}
