package nodediscovery

import (
	"context"
	"os/exec"
	"strings"
	"testing"
)

type fakeRunner struct {
	output []byte
	err    error
	name   string
	args   []string
}

func (f *fakeRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	f.name, f.args = name, append([]string(nil), args...)
	return f.output, f.err
}

func TestDiscoverLocalReturnsConcreteReadOnlyGPUInventory(t *testing.T) {
	runner := &fakeRunner{output: []byte("0, GPU-abc, NVIDIA L40S, 46068, 570.124\n1, GPU-def, NVIDIA L40S, 46068, 570.124\n")}
	report, err := DiscoverLocal(t.Context(), runner)
	if err != nil {
		t.Fatal(err)
	}
	if runner.name != "nvidia-smi" || len(runner.args) != 2 || report.Contract != ContractVersion || report.State != "ready" || len(report.GPUs) != 2 || report.GPUs[0].MemoryTotalMiB != 46068 {
		t.Fatalf("unexpected discovery report=%+v runner=%+v", report, runner)
	}
	for _, limitation := range report.Limitations {
		if strings.Contains(strings.ToLower(limitation), "qualified") {
			t.Fatalf("discovery overclaimed qualification: %q", limitation)
		}
	}
}

func TestDiscoverLocalKeepsMissingToolExplicitAndNonFatal(t *testing.T) {
	report, err := DiscoverLocal(t.Context(), &fakeRunner{err: &exec.Error{Name: "nvidia-smi", Err: exec.ErrNotFound}})
	if err != nil || report.State != "unavailable" || len(report.GPUs) != 0 || len(report.Limitations) < 2 {
		t.Fatalf("missing tool report=%+v err=%v", report, err)
	}
}

func TestDiscoverLocalRejectsMalformedOrUnboundedInventory(t *testing.T) {
	for name, output := range map[string][]byte{
		"missing fields": []byte("0,GPU-abc,L40S\n"),
		"invalid memory": []byte("0,GPU-abc,L40S,unknown,570\n"),
		"oversized":      []byte(strings.Repeat("x", maxOutputBytes+1)),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := DiscoverLocal(t.Context(), &fakeRunner{output: output}); err == nil {
				t.Fatal("malformed inventory was accepted")
			}
		})
	}
	limit := &boundedBuffer{limit: 4}
	if _, err := limit.Write([]byte("12345")); err == nil || !strings.Contains(err.Error(), "bounded") {
		t.Fatalf("bounded writer accepted excessive output: %v", err)
	}
}
