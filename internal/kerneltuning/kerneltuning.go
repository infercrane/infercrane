// Package kerneltuning tunes an engine's own kernels for one GPU and the
// shapes of one workload, and delivers the result as data the engine reads at
// start, never as code.
//
// The first operator is vLLM's Triton fused-MoE kernel. vLLM picks its tile
// settings (block sizes, warps, pipeline stages) from a JSON file named after
// the MoE shape and the GPU, keyed by the number of tokens in a step. It looks
// in the folder VLLM_TUNED_CONFIG_FOLDER names first, then in the files it
// ships, and uses a built-in default when neither has the file; it then logs
// "Using default MoE config". A shape and GPU without a shipped file is the
// gap a tuned set fills. The engine falls back per file, and within a file a
// tuned set keeps the engine's own default for every step size where tuning
// did not measure better, so the set is a shape-keyed override over the
// library kernel, not a replacement for it.
//
// Every fact about an engine version here was read from that version's
// source; anything else is refused ("not reviewed").
package kerneltuning

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

const (
	// OperatorFusedMoE is vLLM's Triton fused-MoE kernel.
	OperatorFusedMoE = "fused-moe"
	// Tuner names the tuning method recorded on every set it makes
	// (tools/kernel-tune/tune_moe.py).
	Tuner = "infercrane-fused-moe-tuner-v1"
	// EngineConfigFolderVariable is the vLLM environment variable naming the
	// folder of tuned fused-MoE files. vLLM reads it before its own files
	// and falls back per file.
	EngineConfigFolderVariable = "VLLM_TUNED_CONFIG_FOLDER"
	// MountPath is where a serving container finds a set's files. It is
	// fixed, so the variable's value is never free text.
	MountPath = "/opt/infercrane/kernel-configs"

	// MaxFiles bounds the files of one set: one per MoE shape the model has.
	MaxFiles = 8
	// MaxFileBytes bounds one file's canonical bytes.
	MaxFileBytes = 32 << 10
	// MaxKeys bounds the step sizes one file tunes.
	MaxKeys = 64
	// MaxKey bounds a step size: tokens in one engine step.
	MaxKey = 1 << 20
)

const (
	sourceVLLM0221FusedMoE = "https://github.com/vllm-project/vllm/blob/v0.22.1/vllm/model_executor/layers/fused_moe/fused_moe.py"
	sourceVLLM034FusedMoE  = "https://github.com/vllm-project/vllm/blob/v0.34.0/vllm/model_executor/layers/fused_moe/fused_moe.py"
)

// reviewedEngine is one engine version whose tuned-config lookup was read
// from its source.
type reviewedEngine struct {
	runtime, version string
	// dtypes are the file-name dtypes whose default heuristic was read, with
	// the step sizes where that heuristic changes its tiles: the default is
	// the same for every size from one boundary up to the next.
	boundaries map[string][]int
	source     string
}

// vLLM 0.22.1 and 0.34.0: get_moe_configs reads VLLM_TUNED_CONFIG_FOLDER
// first and the shipped configs folder second, per file name, and picks the
// entry whose key is nearest the step's token count. On Hopper with block
// FP8 and tensor parallelism the FP8 MoE oracle puts the Triton kernel first.
// For block-quantized FP8 on CUDA, get_default_config changes its tiles above
// 16 and 64 tokens in 0.22.1, and above 4, 8, 16 and 64 tokens in 0.34.0.
var reviewedEngines = []reviewedEngine{
	{runtime: "vllm", version: "0.22.1", boundaries: map[string][]int{"fp8_w8a8+block": {16, 64}}, source: sourceVLLM0221FusedMoE},
	{runtime: "vllm", version: "0.34.0", boundaries: map[string][]int{"fp8_w8a8+block": {4, 8, 16, 64}}, source: sourceVLLM034FusedMoE},
}

func lookupEngine(runtime, version string) (reviewedEngine, bool) {
	for _, engine := range reviewedEngines {
		if engine.runtime == runtime && engine.version == version {
			return engine, true
		}
	}
	return reviewedEngine{}, false
}

// Reviewed reports whether a tuned set for this engine version, dtype and
// block shape can be built and checked.
func Reviewed(runtime, version string, shape FusedMoEShape) bool {
	engine, ok := lookupEngine(runtime, version)
	if !ok {
		return false
	}
	_, ok = engine.boundaries[shape.dtypeClass()]
	return ok
}

// FusedMoEShape is the shape vLLM names a fused-MoE file after: experts on
// one GPU, the expert's intermediate size on one GPU, the weight dtype and
// the quantization block.
type FusedMoEShape struct {
	Experts      int    `json:"experts"`
	Intermediate int    `json:"intermediate"`
	DType        string `json:"dtype,omitempty"`
	BlockN       int    `json:"block_n,omitempty"`
	BlockK       int    `json:"block_k,omitempty"`
}

func (shape FusedMoEShape) dtypeClass() string {
	if shape.BlockN > 0 && shape.BlockK > 0 {
		return shape.DType + "+block"
	}
	return shape.DType
}

// Valid reports whether the shape can name a file.
func (shape FusedMoEShape) Valid() error {
	if shape.Experts <= 0 || shape.Experts > 4096 || shape.Intermediate <= 0 || shape.Intermediate > 1<<17 {
		return fmt.Errorf("fused-MoE shape needs 1 to 4096 experts and an intermediate size from 1 to %d", 1<<17)
	}
	if shape.DType != "" && !validDTypes[shape.DType] {
		return fmt.Errorf("fused-MoE dtype %q is not one the engine names files with", shape.DType)
	}
	if (shape.BlockN > 0) != (shape.BlockK > 0) || shape.BlockN < 0 || shape.BlockK < 0 || shape.BlockN > 1024 || shape.BlockK > 1024 {
		return fmt.Errorf("fused-MoE block shape must give both sizes, each from 1 to 1024")
	}
	return nil
}

var validDTypes = map[string]bool{"fp8_w8a8": true, "int8_w8a8": true, "int8_w8a16": true, "int4_w4a16": true, "bf16": true}

// FileName is the file vLLM looks for: get_config_file_name in fused_moe.py.
// device is the GPU's name as the engine writes it (EngineDeviceName).
func FileName(shape FusedMoEShape, device string) string {
	name := fmt.Sprintf("E=%d,N=%d,device_name=%s", shape.Experts, shape.Intermediate, device)
	if shape.DType != "" {
		name += ",dtype=" + shape.DType
	}
	if shape.BlockN > 0 && shape.BlockK > 0 {
		name += fmt.Sprintf(",block_shape=[%d,%d]", shape.BlockN, shape.BlockK)
	}
	return name + ".json"
}

var fileNamePattern = regexp.MustCompile(`^E=([0-9]{1,4}),N=([0-9]{1,6}),device_name=([A-Za-z0-9_.-]{1,64})(?:,dtype=([a-z0-9_]{1,16}))?(?:,block_shape=\[([0-9]{1,4}),([0-9]{1,4})\])?\.json$`)

// ParseFileName reads a file name vLLM would look for. Down-projection files
// (`_down.json`) and backend-specific files are refused: no tuned set writes
// them.
func ParseFileName(name string) (FusedMoEShape, string, error) {
	match := fileNamePattern.FindStringSubmatch(name)
	if match == nil {
		return FusedMoEShape{}, "", fmt.Errorf("%q is not a fused-MoE file name the engine reads", boundedText(name))
	}
	number := func(text string) int {
		value, _ := strconv.Atoi(text)
		return value
	}
	shape := FusedMoEShape{Experts: number(match[1]), Intermediate: number(match[2]), DType: match[4], BlockN: number(match[5]), BlockK: number(match[6])}
	if err := shape.Valid(); err != nil {
		return FusedMoEShape{}, "", err
	}
	if FileName(shape, match[3]) != name {
		return FusedMoEShape{}, "", fmt.Errorf("%q is not in the engine's own file-name form", boundedText(name))
	}
	return shape, match[3], nil
}

var deviceSpace = regexp.MustCompile(`[\s/]+`)

// EngineDeviceName is the GPU name as vLLM writes it in a file name: the
// name the driver reports, runs of spaces and slashes replaced by one
// underscore, and any H200 named NVIDIA_H200 (get_device_name_as_file_name
// and get_config_file_name).
func EngineDeviceName(reported string) string {
	name := deviceSpace.ReplaceAllString(strings.TrimSpace(reported), "_")
	for _, part := range strings.Split(name, "_") {
		if part == "H200" {
			return "NVIDIA_H200"
		}
	}
	return name
}

// boundedText echoes at most a short, printable prefix of untrusted text.
func boundedText(text string) string {
	if len(text) > 96 {
		text = text[:96] + "…"
	}
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return '?'
		}
		return r
	}, text)
}
