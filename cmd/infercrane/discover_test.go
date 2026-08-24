package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/infercrane/infercrane/internal/nodediscovery"
)

func TestDiscoverLocalCLIIsOfflineReadOnlyAndMachineReadable(t *testing.T) {
	previous := discoverLocal
	t.Cleanup(func() { discoverLocal = previous })
	discoverLocal = func(context.Context) (nodediscovery.Report, error) {
		return nodediscovery.Report{Contract: nodediscovery.ContractVersion, State: "ready", Source: "nvidia-smi", GPUs: []nodediscovery.GPU{{Index: 0, UUID: "GPU-abc", Name: "NVIDIA L40S", MemoryTotalMiB: 46068, DriverVersion: "570.124"}}, Limitations: []string{"read-only"}}, nil
	}
	output, err := captureStdout(t, func() error { return discoverCommand(t.Context(), []string{"local", "--output", "json"}) })
	if err != nil {
		t.Fatal(err)
	}
	var report nodediscovery.Report
	if json.Unmarshal([]byte(output), &report) != nil || report.State != "ready" || len(report.GPUs) != 1 {
		t.Fatalf("unexpected discovery JSON %q", output)
	}
}

func TestDiscoverLocalHumanOutputExplainsAdoptionBoundary(t *testing.T) {
	previous := discoverLocal
	t.Cleanup(func() { discoverLocal = previous })
	discoverLocal = func(context.Context) (nodediscovery.Report, error) {
		return nodediscovery.Report{Contract: nodediscovery.ContractVersion, State: "unavailable", Source: "nvidia-smi", GPUs: []nodediscovery.GPU{}, Limitations: []string{"Discovery is read-only and does not transfer lifecycle ownership."}}, nil
	}
	output, err := captureStdout(t, func() error { return discoverCommand(t.Context(), []string{"local"}) })
	if err != nil || !strings.Contains(output, "No concrete NVIDIA GPU") || !strings.Contains(output, "infercrane connect") || !strings.Contains(output, "read-only") {
		t.Fatalf("output=%q err=%v", output, err)
	}
}
