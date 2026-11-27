package kerneltuning

import (
	"os"
	"os/exec"
	"testing"
)

// The tuner and the proof harness are Python that runs in the engine image
// (tools/kernel-tune). Their offline suite needs no GPU, torch, vLLM or Modal,
// and pins values this package computes (file bytes, digests, key plans, log
// readings), so the Go suite runs it.
func TestKernelTuneHarnessPythonSuite(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is not installed")
	}
	command := exec.Command(python, "-I", "-m", "unittest", "discover", "-s", "../../tools/kernel-tune", "-p", "test_*.py")
	command.Env = []string{"PATH=" + os.Getenv("PATH"), "PYTHONDONTWRITEBYTECODE=1"}
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("tools/kernel-tune Python suite failed: %v\n%s", err, output)
	}
}
