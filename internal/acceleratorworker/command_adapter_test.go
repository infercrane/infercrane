package acceleratorworker

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/infercrane/infercrane/internal/acceleratorlab"
)

func TestCommandAdapterExecutesWithoutShellInterpolation(t *testing.T) {
	directory := t.TempDir()
	executable := filepath.Join(directory, "adapter")
	body := `#!/bin/sh
case "$1" in
profile) cat >/dev/null; printf '%s\n' '{"input_digest":"sha256:` + strings.Repeat("b", 64) + `","tool":"nsys","tool_version":"1","hardware":{"vendor":"nvidia","accelerator":"H200","compute_capability":"sm90"},"runtime_image_digest":"sha256:` + strings.Repeat("d", 64) + `","topology":{"mode":"aggregated","nodes":1,"accelerators":1,"tensor_parallel":1,"pipeline_parallel":1,"expert_parallel":1},"workload_digest":"sha256:` + strings.Repeat("e", 64) + `","captured_at":"2026-09-23T00:00:00Z","hotspots":[],"cost_usd":0.1,"artifact_digest":"sha256:` + strings.Repeat("a", 64) + `"}' ;;
*) exit 7 ;;
esac
`
	if err := os.WriteFile(executable, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	adapter, err := NewCommandAdapter(CommandAdapterConfig{Executable: executable})
	if err != nil {
		t.Fatal(err)
	}
	evidence, err := adapter.Capture(context.Background(), profileIntent())
	if err != nil || evidence.Tool != "nsys" || evidence.CostUSD != .1 {
		t.Fatalf("evidence=%+v err=%v", evidence, err)
	}
}

func TestCommandAdapterRejectsDigestMismatch(t *testing.T) {
	directory := t.TempDir()
	executable := filepath.Join(directory, "adapter")
	if err := os.WriteFile(executable, []byte("#!/bin/sh\ncat >/dev/null\nprintf '{\"input_digest\":\"wrong\"}'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	adapter, err := NewCommandAdapter(CommandAdapterConfig{Executable: executable})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = adapter.Capture(context.Background(), acceleratorlab.ProfileIntent{InputDigest: "wanted"}); err == nil {
		t.Fatal("digest mismatch accepted")
	}
}
