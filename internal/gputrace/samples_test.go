package gputrace

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/infercrane/infercrane/internal/gpuprofile"
	"github.com/infercrane/infercrane/internal/testtools/tracefixture"
)

var update = flag.Bool("update", false, "rewrite the generated sample traces and golden analyses")

type sample struct {
	name, format string
	files        map[string][]byte
	options      Options
}

func samples() []sample {
	sglang := tracefixture.SGLangTP2(tracefixture.SampleBase)
	kernels, hip, copies := tracefixture.RocprofV3()
	gpu, api := tracefixture.NsysCSV()
	return []sample{
		{"vllm-h100", FormatKineto, map[string][]byte{"rank0.pt.trace.json.gz": tracefixture.VLLMSingleGPU(tracefixture.SampleBase)}, Options{Engine: "vllm", EngineVersion: "0.22.1"}},
		{"vllm-moe-h200", FormatKineto, map[string][]byte{"rank0.pt.trace.json.gz": tracefixture.VLLMMoE(tracefixture.SampleBase)}, Options{Engine: "vllm", EngineVersion: "0.22.1"}},
		{"sglang-tp2", FormatKineto, map[string][]byte{"rank0.trace.json.gz": sglang[0], "rank1.trace.json.gz": sglang[1]}, Options{Engine: "sglang"}},
		{"rocprofv3-mi300x", FormatRocprofV3, map[string][]byte{"kernel_trace.csv": kernels, "hip_api_trace.csv": hip, "memory_copy_trace.csv": copies}, Options{Accelerator: "AMD Instinct MI300X"}},
		{"nsys-l40s", FormatNsys, map[string][]byte{"report_cuda_gpu_trace.csv": gpu, "report_cuda_api_trace.csv": api}, Options{}},
	}
}

// TestSampleTracesMatchTheirGenerator keeps the committed sample traces in
// step with internal/testtools/tracefixture; -update rewrites them.
func TestSampleTracesMatchTheirGenerator(t *testing.T) {
	for _, current := range samples() {
		for name, data := range current.files {
			path := filepath.Join("testdata", current.name, name)
			if *update {
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, data, 0o644); err != nil {
					t.Fatal(err)
				}
				continue
			}
			stored, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("%s: %v (run go test ./internal/gputrace -run TestSampleTraces -update)", path, err)
			}
			if !bytes.Equal(stored, data) {
				t.Fatalf("%s differs from its generator; run go test ./internal/gputrace -run TestSampleTraces -update", path)
			}
		}
	}
}

// TestSampleAnalysesMatchGolden reads every committed sample from disk and
// compares its analysis with the reviewed golden file.
func TestSampleAnalysesMatchGolden(t *testing.T) {
	for _, current := range samples() {
		t.Run(current.name, func(t *testing.T) {
			trace, err := ParseFiles([]string{filepath.Join("testdata", current.name)}, "")
			if err != nil {
				t.Fatal(err)
			}
			if trace.Format != current.format {
				t.Fatalf("detected %s, want %s", trace.Format, current.format)
			}
			analysis, err := Analyze(trace, current.options)
			if err != nil {
				t.Fatal(err)
			}
			got, err := json.MarshalIndent(analysis, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			got = append(got, '\n')
			golden := filepath.Join("testdata", current.name+".analysis.golden.json")
			if *update {
				if err = os.WriteFile(golden, got, 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("analysis differs from %s; review the change and run with -update\n%s", golden, got)
			}
		})
	}
}

var _ = gpuprofile.AnalysisAlgorithm
