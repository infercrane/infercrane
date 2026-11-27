package kerneltuning

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/support"
)

// qwen36Shape is Qwen3.6-35B-A3B-FP8 on one GPU: 256 experts, an expert
// intermediate size of 512, FP8 weights in 128x128 blocks.
var qwen36Shape = FusedMoEShape{Experts: 256, Intermediate: 512, DType: "fp8_w8a8", BlockN: 128, BlockK: 128}

const h100 = "NVIDIA_H100_80GB_HBM3"

func one() *int { value := 1; return &value }

func tunedEntries() map[int]TileConfig {
	return map[int]TileConfig{
		1:   {BlockSizeM: 16, BlockSizeN: 64, BlockSizeK: 128, GroupSizeM: 1, NumWarps: 4, NumStages: 4},
		128: {BlockSizeM: 16, BlockSizeN: 128, BlockSizeK: 128, GroupSizeM: 1, NumWarps: 4, NumStages: 3},
		65:  {BlockSizeM: 64, BlockSizeN: 128, BlockSizeK: 128, GroupSizeM: 32, SplitK: one(), NumWarps: 4, NumStages: 3},
	}
}

func testSet(t *testing.T) domain.KernelConfigSet {
	t.Helper()
	file, err := NewFile(qwen36Shape, h100, tunedEntries())
	if err != nil {
		t.Fatal(err)
	}
	set := NewSet("vllm", "0.34.0", h100, []domain.KernelConfigFile{file})
	if err = Validate(set); err != nil {
		t.Fatal(err)
	}
	return set
}

func TestFileNameIsTheEnginesName(t *testing.T) {
	// The file vLLM 0.34.0 looks for on an H100 for this model, and the one
	// it ships for the H200 (E=256,N=512 ... NVIDIA_H200 ... block_shape).
	if got := FileName(qwen36Shape, h100); got != "E=256,N=512,device_name=NVIDIA_H100_80GB_HBM3,dtype=fp8_w8a8,block_shape=[128,128].json" {
		t.Fatalf("FileName = %s", got)
	}
	if got := FileName(FusedMoEShape{Experts: 256, Intermediate: 512}, h100); got != "E=256,N=512,device_name=NVIDIA_H100_80GB_HBM3.json" {
		t.Fatalf("bf16 FileName = %s", got)
	}
	shape, device, err := ParseFileName("E=256,N=512,device_name=NVIDIA_H200,dtype=fp8_w8a8,block_shape=[128,128].json")
	if err != nil || shape != qwen36Shape || device != "NVIDIA_H200" {
		t.Fatalf("ParseFileName = %+v %q %v", shape, device, err)
	}
	for _, name := range []string{
		"E=256,N=512,device_name=NVIDIA_H200,dtype=fp8_w8a8,block_shape=[128,128]_down.json",
		"../E=256,N=512,device_name=NVIDIA_H200.json",
		"E=256,N=512,device_name=NVIDIA H200.json",
		"E=0256,N=512,device_name=NVIDIA_H200.json",
		"E=256,N=512,device_name=NVIDIA_H200,block_shape=[128,128].json\n",
	} {
		if _, _, err := ParseFileName(name); err == nil {
			t.Errorf("ParseFileName(%q) accepted", name)
		}
	}
}

func TestEngineDeviceName(t *testing.T) {
	for reported, want := range map[string]string{
		"NVIDIA H100 80GB HBM3": "NVIDIA_H100_80GB_HBM3",
		"NVIDIA H200":           "NVIDIA_H200",
		"NVIDIA H200 NVL":       "NVIDIA_H200",
		"NVIDIA A100-SXM4-80GB": "NVIDIA_A100-SXM4-80GB",
		"NVIDIA RTX A6000/x":    "NVIDIA_RTX_A6000_x",
	} {
		if got := EngineDeviceName(reported); got != want {
			t.Errorf("EngineDeviceName(%q) = %q, want %q", reported, got, want)
		}
	}
	if got := DeviceForGPU("H100"); got != h100 {
		t.Fatalf("DeviceForGPU(H100) = %q", got)
	}
}

func TestValidateAcceptsOnlyCanonicalDataFiles(t *testing.T) {
	set := testSet(t)
	if !strings.HasPrefix(set.Digest, "sha256:") || len(set.Files) != 1 {
		t.Fatalf("set = %+v", set)
	}
	wantContent := `{"1":{"BLOCK_SIZE_M":16,"BLOCK_SIZE_N":64,"BLOCK_SIZE_K":128,"GROUP_SIZE_M":1,"num_warps":4,"num_stages":4},` +
		`"65":{"BLOCK_SIZE_M":64,"BLOCK_SIZE_N":128,"BLOCK_SIZE_K":128,"GROUP_SIZE_M":32,"SPLIT_K":1,"num_warps":4,"num_stages":3},` +
		`"128":{"BLOCK_SIZE_M":16,"BLOCK_SIZE_N":128,"BLOCK_SIZE_K":128,"GROUP_SIZE_M":1,"num_warps":4,"num_stages":3}}`
	if set.Files[0].Content != wantContent {
		t.Fatalf("canonical content = %s", set.Files[0].Content)
	}

	// A set survives storage that rewrites JSON objects: the content is a
	// string.
	body, err := json.Marshal(set)
	if err != nil {
		t.Fatal(err)
	}
	var back domain.KernelConfigSet
	if err = json.Unmarshal(body, &back); err != nil || Validate(back) != nil {
		t.Fatalf("round trip: %v %v", err, Validate(back))
	}

	mutate := map[string]func(*domain.KernelConfigSet){
		"code in a file": func(s *domain.KernelConfigSet) { s.Files[0].Content = `{"1":{"BLOCK_SIZE_M":"import os"}}` },
		"extra setting": func(s *domain.KernelConfigSet) {
			s.Files[0].Content = strings.Replace(s.Files[0].Content, `"num_stages":4}`, `"num_stages":4,"USE_TMA":1}`, 1)
		},
		"not canonical": func(s *domain.KernelConfigSet) {
			s.Files[0].Content = strings.Replace(s.Files[0].Content, `{"1":`, `{ "1":`, 1)
		},
		"wrong sha256":        func(s *domain.KernelConfigSet) { s.Files[0].SHA256 = strings.Repeat("0", 64) },
		"wrong digest":        func(s *domain.KernelConfigSet) { s.Digest = "sha256:" + strings.Repeat("0", 64) },
		"other device's file": func(s *domain.KernelConfigSet) { s.Device = "NVIDIA_H200" },
		"unreviewed engine":   func(s *domain.KernelConfigSet) { s.RuntimeVersion = "0.22.1" },
		"other operator":      func(s *domain.KernelConfigSet) { s.Operator = "attention" },
		"no files":            func(s *domain.KernelConfigSet) { s.Files = nil },
		"duplicate file":      func(s *domain.KernelConfigSet) { s.Files = append(s.Files, s.Files[0]) },
		"unreviewed dtype": func(s *domain.KernelConfigSet) {
			s.Files[0].Name = "E=256,N=512,device_name=NVIDIA_H100_80GB_HBM3.json"
		},
		"split K above one": func(s *domain.KernelConfigSet) {
			s.Files[0].Content = strings.Replace(s.Files[0].Content, `"SPLIT_K":1`, `"SPLIT_K":2`, 1)
		},
		"misaligned tile": func(s *domain.KernelConfigSet) {
			s.Files[0].Content = strings.Replace(s.Files[0].Content, `"BLOCK_SIZE_K":128,"GROUP_SIZE_M":1,"num_warps":4,"num_stages":4`, `"BLOCK_SIZE_K":48,"GROUP_SIZE_M":1,"num_warps":4,"num_stages":4`, 1)
		},
		"step size zero": func(s *domain.KernelConfigSet) {
			s.Files[0].Content = strings.Replace(s.Files[0].Content, `{"1":`, `{"0":`, 1)
		},
		"two JSON objects": func(s *domain.KernelConfigSet) { s.Files[0].Content += "{}" },
		"fractional block": func(s *domain.KernelConfigSet) {
			s.Files[0].Content = strings.Replace(s.Files[0].Content, `"BLOCK_SIZE_M":16,"BLOCK_SIZE_N":64`, `"BLOCK_SIZE_M":16.5,"BLOCK_SIZE_N":64`, 1)
		},
		"other tuner recorded": func(s *domain.KernelConfigSet) { s.Tuner = "hand-written" },
	}
	for name, change := range mutate {
		broken := testSet(t)
		broken.Files = append([]domain.KernelConfigFile(nil), broken.Files...)
		change(&broken)
		if Validate(broken) == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestValidateForRevisionPinsEngineAndGPU(t *testing.T) {
	set := testSet(t)
	if err := ValidateForRevision(set, "vllm", "0.34.0", "H100"); err != nil {
		t.Fatal(err)
	}
	for _, revision := range [][3]string{{"vllm", "0.22.1", "H100"}, {"sglang", "0.34.0", "H100"}, {"vllm", "0.34.0", "H200"}, {"vllm", "0.34.0", "H100 PCIe"}} {
		if ValidateForRevision(set, revision[0], revision[1], revision[2]) == nil {
			t.Errorf("revision %v accepted a set tuned for vllm 0.34.0 on an H100 SXM", revision)
		}
	}
}

func TestKeysKeepTheEnginesDefaultExactOutsideTunedSizes(t *testing.T) {
	loads := []Load{{Running: 32, BatchedTokens: 8192}, {Running: 128, BatchedTokens: 8192}}
	plan, err := Keys("vllm", "0.34.0", qwen36Shape, loads)
	if err != nil {
		t.Fatal(err)
	}
	wantTune := []int{1, 2, 4, 8, 16, 24, 32, 48, 64, 96, 128, 2048, 3072, 4096, 6144, 8192}
	if len(plan.Tune) != len(wantTune) {
		t.Fatalf("tune = %v, want %v", plan.Tune, wantTune)
	}
	for index := range wantTune {
		if plan.Tune[index] != wantTune[index] {
			t.Fatalf("tune = %v, want %v", plan.Tune, wantTune)
		}
	}
	if want := []int{5, 9, 17, 65}; len(plan.Default) != len(want) || plan.Default[0] != 5 || plan.Default[3] != 65 {
		t.Fatalf("default = %v, want %v", plan.Default, want)
	}
	// For every step size, the nearest key gets the same default tiles as
	// the step itself, so a key left at the default changes nothing.
	keys := append(append([]int{}, plan.Tune...), plan.Default...)
	for tokens := 1; tokens <= 9000; tokens++ {
		if key := NearestKey(keys, tokens); !SameDefaultRegion("vllm", "0.34.0", qwen36Shape, key, tokens) {
			t.Fatalf("step of %d tokens resolves to key %d across a default boundary", tokens, key)
		}
	}
	// Speculation multiplies a decode step.
	plan, err = Keys("vllm", "0.34.0", qwen36Shape, []Load{{Running: 128, DraftTokens: 3}})
	if err != nil || plan.Tune[len(plan.Tune)-1] != 512 {
		t.Fatalf("with 3 drafts tune = %v, %v", plan.Tune, err)
	}
	if _, err = Keys("vllm", "0.20.2", qwen36Shape, loads); err == nil {
		t.Fatal("an unreviewed engine version planned keys")
	}
	// vLLM 0.22.1's default changes only above 16 and 64 tokens.
	if plan, err = Keys("vllm", "0.22.1", qwen36Shape, loads); err != nil || len(plan.Default) != 2 || plan.Default[0] != 17 || plan.Default[1] != 65 {
		t.Fatalf("0.22.1 default = %v, %v", plan.Default, err)
	}
	if _, err = Keys("vllm", "0.34.0", FusedMoEShape{Experts: 256, Intermediate: 512}, loads); err == nil {
		t.Fatal("an unreviewed dtype planned keys")
	}
}

func TestNearestKeyBreaksTiesLow(t *testing.T) {
	if got := NearestKey([]int{64, 96}, 80); got != 64 {
		t.Fatalf("tie went to %d", got)
	}
	if got := NearestKey([]int{96, 64, 128}, 100); got != 96 {
		t.Fatalf("nearest = %d", got)
	}
}

func TestReadEngineLog(t *testing.T) {
	log := strings.Join([]string{
		"\x1b[0;36m(EngineCore pid=91)\x1b[0;0m INFO 11-26 10:00:01 [fp8.py:502] Using TRITON Fp8 MoE backend out of potential backends: ['TRITON', 'DEEPGEMM'].",
		"WARNING 11-26 10:00:02 [fused_moe.py:1254] Using default MoE config. Performance might be sub-optimal! Config file not found at /opt/infercrane/kernel-configs/E=256,N=512,device_name=NVIDIA_H100_80GB_HBM3,dtype=fp8_w8a8,block_shape=[128,128].json, /usr/local/lib/python3.12/dist-packages/vllm/model_executor/layers/fused_moe/configs/E=256,N=512,device_name=NVIDIA_H100_80GB_HBM3,dtype=fp8_w8a8,block_shape=[128,128].json",
	}, "\n")
	reading := ReadEngineLog(log)
	if reading.Backend != "TRITON" || len(reading.DefaultFor) != 1 || len(reading.ConfigFiles) != 0 {
		t.Fatalf("reading = %+v", reading)
	}
	if gaps := reading.Gaps(); len(gaps) != 1 || gaps[0] != qwen36Shape {
		t.Fatalf("gaps = %+v", gaps)
	}
	tuned := ReadEngineLog("INFO 11-26 10:05:00 [fused_moe.py:1240] Using configuration from /opt/infercrane/kernel-configs/E=256,N=512,device_name=NVIDIA_H100_80GB_HBM3,dtype=fp8_w8a8,block_shape=[128,128].json for MoE layer.\r")
	if names := tuned.ReadFrom(MountPath); len(names) != 1 || names[0] != FileName(qwen36Shape, h100) || len(tuned.DefaultFor) != 0 {
		t.Fatalf("tuned reading = %+v", tuned)
	}
	shipped := ReadEngineLog("Using configuration from /usr/lib/vllm/fused_moe/configs/E=256,N=512,device_name=NVIDIA_H200,dtype=fp8_w8a8,block_shape=[128,128].json for MoE layer.")
	if len(shipped.ReadFrom(MountPath)) != 0 {
		t.Fatal("a shipped file counted as the tuned set's")
	}
}

func TestShapeFromModelConfig(t *testing.T) {
	// Qwen3.6-35B-A3B-FP8's config.json fields that decide the shape.
	qwen := `{"architectures":["Qwen3_5MoeForConditionalGeneration"],"text_config":{"num_experts":256,"num_experts_per_tok":8,"moe_intermediate_size":512,"hidden_size":2048},
		"quantization_config":{"quant_method":"fp8","fmt":"e4m3","activation_scheme":"dynamic","weight_block_size":[128,128]}}`
	shape, ok, err := ShapeFromModelConfig([]byte(qwen), 1)
	if err != nil || !ok || shape != qwen36Shape {
		t.Fatalf("shape = %+v %v %v", shape, ok, err)
	}
	shape, ok, err = ShapeFromModelConfig([]byte(qwen), 2)
	if err != nil || !ok || shape.Intermediate != 256 {
		t.Fatalf("TP2 shape = %+v %v %v", shape, ok, err)
	}
	coder := `{"num_experts":128,"moe_intermediate_size":768,"quantization_config":{"quant_method":"fp8","weight_block_size":[128,128]}}`
	if shape, ok, err = ShapeFromModelConfig([]byte(coder), 1); err != nil || !ok || FileName(shape, h100) != "E=128,N=768,device_name=NVIDIA_H100_80GB_HBM3,dtype=fp8_w8a8,block_shape=[128,128].json" {
		t.Fatalf("coder = %+v %v %v", shape, ok, err)
	}
	if _, ok, err = ShapeFromModelConfig([]byte(`{"hidden_size":4096,"intermediate_size":11008}`), 1); ok || err != nil {
		t.Fatalf("dense model: ok=%v err=%v", ok, err)
	}
	if _, _, err = ShapeFromModelConfig([]byte(`{"num_experts":8,"intermediate_size":14336,"quantization_config":{"quant_method":"awq"}}`), 1); err == nil {
		t.Fatal("an unreviewed quantization method was accepted")
	}
	if _, _, err = ShapeFromModelConfig([]byte(`{"num_experts":8,"intermediate_size":14335}`), 2); err == nil {
		t.Fatal("an intermediate size that does not split was accepted")
	}
}

func TestReviewed(t *testing.T) {
	if !Reviewed("vllm", "0.34.0", qwen36Shape) || !Reviewed("vllm", "0.22.1", qwen36Shape) || Reviewed("vllm", "0.34.0", FusedMoEShape{Experts: 8, Intermediate: 14336}) || Reviewed("sglang", "0.5.20", qwen36Shape) {
		t.Fatal("reviewed engines are not as read from the source")
	}
}

// TestDigestIsStable pins one set's digest: tools/kernel-tune/tune_moe.py
// computes the same value for the same set (test_kernel_tune.py), so a set
// the tuner writes is the set a revision validates.
func TestDigestIsStable(t *testing.T) {
	if got := testSet(t).Digest; got != pinnedDigest {
		t.Fatalf("digest = %s, want %s", got, pinnedDigest)
	}
}

const pinnedDigest = "sha256:7b137f2132b0b2cd25d9b94007e00e57bd6e56e5cf27ad9b929a8266bf947def"

func TestValidateForDeployment(t *testing.T) {
	if err := ValidateForDeployment(nil, "", "", "H100", true); err != nil {
		t.Fatalf("no set: %v", err)
	}
	file, err := NewFile(qwen36Shape, h100, tunedEntries())
	if err != nil {
		t.Fatal(err)
	}
	current := NewSet("vllm", support.CurrentRuntimeVersion("vllm"), h100, []domain.KernelConfigFile{file})
	// A revision without an engine or version runs the current vLLM release.
	if err = ValidateForDeployment(&current, "", "", "H100", true); err != nil {
		t.Fatal(err)
	}
	if ValidateForDeployment(&current, "vllm", "", "H100", false) == nil {
		t.Fatal("a custom workload accepted a set reviewed for the pinned image")
	}
	older := NewSet("vllm", support.LegacyVLLMRuntimeVersion, h100, []domain.KernelConfigFile{file})
	if ValidateForDeployment(&older, "vllm", "", "H100", true) == nil {
		t.Fatal("a set for the older release was accepted by a revision on the current release")
	}
	// A revision that records the older release takes its set, however the version is written.
	if err = ValidateForDeployment(&older, "vllm", "v"+support.LegacyVLLMRuntimeVersion, "H100", true); err != nil {
		t.Fatal(err)
	}
}
