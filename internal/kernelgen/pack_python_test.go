package kernelgen

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// Reviewed packs keep their serving integration and hidden evaluator in
// Python. Their stub-based suites need no GPU, torch, or vLLM, so the Go suite
// runs them and CI covers the runtime version gate and roofline arithmetic.
func TestRepositoryPackPythonSuites(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is not installed")
	}
	suites, err := filepath.Glob("../../tools/kernel-lab/packs/*/test_*.py")
	if err != nil || len(suites) == 0 {
		t.Fatalf("no pack Python suites found: %v", err)
	}
	directories := map[string]struct{}{}
	for _, suite := range suites {
		directories[filepath.Dir(suite)] = struct{}{}
	}
	for directory := range directories {
		command := exec.Command(python, "-I", "-m", "unittest", "discover", "-s", directory, "-p", "test_*.py")
		command.Env = []string{"PATH=" + os.Getenv("PATH"), "PYTHONDONTWRITEBYTECODE=1"}
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("%s Python suites failed: %v\n%s", filepath.Base(directory), err, output)
		}
	}
}
