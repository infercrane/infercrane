package kernelplanner

import (
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

const RegistryVersion = "infercrane-kernel-registry-v1"

// Implementation describes a reviewed place to look for an implementation.
// It is deliberately source-level metadata rather than a performance claim:
// the exact commit is pinned when a campaign is created and every candidate
// still has to pass compatibility, correctness, and target-GPU qualification.
type Implementation struct {
	ID                       string           `json:"id"`
	Name                     string           `json:"name"`
	Project                  string           `json:"project"`
	SourceURL                string           `json:"source_url"`
	License                  string           `json:"license"`
	Kind                     string           `json:"kind"`
	Backend                  string           `json:"backend"`
	OperatorFamilies         []OperatorFamily `json:"operator_families"`
	RuntimeAllowlist         []string         `json:"runtime_allowlist,omitempty"`
	HardwareVendors          []string         `json:"hardware_vendors"`
	MinimumComputeCapability string           `json:"minimum_compute_capability,omitempty"`
	DTypes                   []string         `json:"dtypes,omitempty"`
	Phases                   []Phase          `json:"phases,omitempty"`
	Priority                 int              `json:"priority"`
	Status                   string           `json:"status"`
	RevisionPolicy           string           `json:"revision_policy"`
	ExecutionBoundary        string           `json:"execution_boundary"`
	Notes                    []string         `json:"notes,omitempty"`
}

// KernelMatch is a deterministic, compatibility-filtered search result. A
// match is a candidate to inspect and benchmark, never proof that it is faster.
type KernelMatch struct {
	ImplementationID  string   `json:"implementation_id"`
	Name              string   `json:"name"`
	Project           string   `json:"project"`
	SourceURL         string   `json:"source_url"`
	License           string   `json:"license"`
	Kind              string   `json:"kind"`
	Backend           string   `json:"backend"`
	Status            string   `json:"status"`
	Compatibility     string   `json:"compatibility"`
	RevisionPolicy    string   `json:"revision_policy"`
	ExecutionBoundary string   `json:"execution_boundary"`
	Priority          int      `json:"priority"`
	Reasons           []string `json:"reasons"`
}

type Registry struct {
	Version         string           `json:"version"`
	Implementations []Implementation `json:"implementations"`
}

type SearchRequest struct {
	Family            OperatorFamily
	Runtime           string
	HardwareVendor    string
	ComputeCapability string
	DTypes            []string
	Phase             Phase
}

// DefaultRegistry returns reviewed source locations in the order InferCrane
// should investigate them: the pinned runtime/vendor path first, specialized
// libraries next, then adaptable DSL and handwritten templates.
func DefaultRegistry() Registry {
	allFamilies := []OperatorFamily{ResidualRMSNorm, QuantizedLinear, SwiGLU, AttentionPrefill, AttentionDecode, KVCache, MoERouting, Sampling}
	return Registry{Version: RegistryVersion, Implementations: []Implementation{
		{
			ID: "runtime-vllm", Name: "Pinned vLLM implementation", Project: "vLLM", SourceURL: "https://github.com/vllm-project/vllm", License: "Apache-2.0", Kind: "runtime-bundled", Backend: "vllm", OperatorFamilies: allFamilies, RuntimeAllowlist: []string{"vllm"}, HardwareVendors: []string{"nvidia"}, Priority: 10, Status: "baseline", RevisionPolicy: "use the implementation bundled in the pinned runtime image", ExecutionBoundary: "trusted-runtime-image",
			Notes: []string{"Measure this implementation as the control before adapting or generating a kernel."},
		},
		{
			ID: "runtime-sglang", Name: "Pinned SGLang implementation", Project: "SGLang", SourceURL: "https://github.com/sgl-project/sglang", License: "Apache-2.0", Kind: "runtime-bundled", Backend: "sglang", OperatorFamilies: allFamilies, RuntimeAllowlist: []string{"sglang"}, HardwareVendors: []string{"nvidia"}, Priority: 10, Status: "baseline", RevisionPolicy: "use the implementation bundled in the pinned runtime image", ExecutionBoundary: "trusted-runtime-image",
			Notes: []string{"Measure this implementation as the control before adapting or generating a kernel."},
		},
		{
			ID: "flashinfer", Name: "FlashInfer kernel and primitive", Project: "FlashInfer", SourceURL: "https://github.com/flashinfer-ai/flashinfer", License: "Apache-2.0", Kind: "specialized-library", Backend: "flashinfer", OperatorFamilies: []OperatorFamily{AttentionPrefill, AttentionDecode, KVCache, MoERouting, Sampling}, HardwareVendors: []string{"nvidia"}, MinimumComputeCapability: "sm75", DTypes: []string{"fp16", "bf16", "fp8", "int8"}, Phases: []Phase{PhasePrefill, PhaseDecode, PhaseMixed}, Priority: 20, Status: "review-required", RevisionPolicy: "pin an immutable upstream commit and package digest per campaign", ExecutionBoundary: "brezel-build-and-target-gpu-worker",
			Notes: []string{"Runner and tactic selection must be profiled for the exact shapes and accelerator."},
		},
		{
			ID: "cutlass-cute", Name: "CUTLASS/CuTe template", Project: "NVIDIA CUTLASS", SourceURL: "https://github.com/NVIDIA/cutlass", License: "BSD-3-Clause", Kind: "vendor-library", Backend: "cutlass-cute", OperatorFamilies: []OperatorFamily{QuantizedLinear, MoERouting}, HardwareVendors: []string{"nvidia"}, MinimumComputeCapability: "sm75", DTypes: []string{"fp16", "bf16", "tf32", "fp8", "int8", "int4"}, Phases: []Phase{PhasePrefill, PhaseDecode, PhaseMixed}, Priority: 30, Status: "review-required", RevisionPolicy: "pin an immutable upstream commit and generated-kernel digest per campaign", ExecutionBoundary: "brezel-build-and-target-gpu-worker",
			Notes: []string{"Instantiate only templates compatible with the exact layout, quantization format, and SM target."},
		},
		{
			ID: "triton-language", Name: "Triton implementation search", Project: "Triton", SourceURL: "https://github.com/triton-lang/triton", License: "MIT", Kind: "kernel-dsl", Backend: "triton", OperatorFamilies: allFamilies, HardwareVendors: []string{"nvidia"}, MinimumComputeCapability: "sm70", DTypes: []string{"fp32", "tf32", "fp16", "bf16", "fp8", "int8", "int4"}, Phases: []Phase{PhasePrefill, PhaseDecode, PhaseMixed}, Priority: 40, Status: "adapt-or-generate", RevisionPolicy: "pin compiler, source, generated artifact, and target architecture", ExecutionBoundary: "brezel-build-and-target-gpu-worker",
			Notes: []string{"Prefer adapting a reviewed runtime or library kernel before generating a new implementation."},
		},
		{
			ID: "infercrane-residual-rmsnorm-triton", Name: "InferCrane fused residual RMSNorm", Project: "InferCrane", SourceURL: "https://github.com/infercrane/infercrane/tree/main/tools/kernel-lab", License: "Apache-2.0", Kind: "internal-experiment", Backend: "triton", OperatorFamilies: []OperatorFamily{ResidualRMSNorm}, HardwareVendors: []string{"nvidia"}, MinimumComputeCapability: "sm70", DTypes: []string{"fp16", "bf16", "fp32"}, Phases: []Phase{PhasePrefill, PhaseDecode, PhaseMixed}, Priority: 45, Status: "experimental", RevisionPolicy: "use the repository commit and artifact digest", ExecutionBoundary: "brezel-build-and-target-gpu-worker",
			Notes: []string{"The Qwen3-0.6B H100 experiment improved the isolated operator but did not establish a material end-to-end win."},
		},
		{
			ID: "cuda-cute-template", Name: "Handwritten CUDA/CuTe template", Project: "InferCrane", SourceURL: "https://github.com/infercrane/infercrane/tree/main/tools/kernel-lab", License: "Apache-2.0", Kind: "custom-template", Backend: "cuda-cute", OperatorFamilies: allFamilies, HardwareVendors: []string{"nvidia"}, MinimumComputeCapability: "sm70", Phases: []Phase{PhasePrefill, PhaseDecode, PhaseMixed}, Priority: 60, Status: "generate-last", RevisionPolicy: "pin source, compiler, flags, generated artifact, driver, and target architecture", ExecutionBoundary: "brezel-build-and-target-gpu-worker",
			Notes: []string{"Enter only after an Amdahl gate and after existing runtime, vendor, and open-source candidates have been measured."},
		},
	}}
}

func (r Registry) Validate() error {
	if strings.TrimSpace(r.Version) == "" || len(r.Implementations) == 0 {
		return errors.New("kernel registry requires a version and implementations")
	}
	seen := map[string]struct{}{}
	for _, implementation := range r.Implementations {
		if implementation.ID == "" || implementation.Name == "" || implementation.Project == "" || implementation.License == "" || implementation.Backend == "" || implementation.Kind == "" || implementation.Priority < 1 || len(implementation.OperatorFamilies) == 0 || len(implementation.HardwareVendors) == 0 || implementation.RevisionPolicy == "" || implementation.ExecutionBoundary == "" {
			return fmt.Errorf("kernel implementation %q is incomplete", implementation.ID)
		}
		if _, duplicate := seen[implementation.ID]; duplicate {
			return fmt.Errorf("duplicate kernel implementation %q", implementation.ID)
		}
		seen[implementation.ID] = struct{}{}
		parsed, err := url.Parse(implementation.SourceURL)
		if err != nil || parsed.Scheme != "https" || parsed.Host == "" {
			return fmt.Errorf("kernel implementation %q requires an HTTPS source URL", implementation.ID)
		}
	}
	return nil
}

func (r Registry) Search(request SearchRequest) ([]KernelMatch, error) {
	if err := r.Validate(); err != nil {
		return nil, err
	}
	request.Runtime = strings.ToLower(strings.TrimSpace(request.Runtime))
	request.HardwareVendor = strings.ToLower(strings.TrimSpace(request.HardwareVendor))
	request.ComputeCapability = strings.ToLower(strings.TrimSpace(request.ComputeCapability))
	if request.Family == "" || request.HardwareVendor == "" {
		return nil, errors.New("kernel search requires operator family and hardware vendor")
	}
	dtypes := normalizedSet(request.DTypes)
	var matches []KernelMatch
	for _, implementation := range r.Implementations {
		if !containsOperator(implementation.OperatorFamilies, request.Family) || !containsFold(implementation.HardwareVendors, request.HardwareVendor) {
			continue
		}
		if len(implementation.RuntimeAllowlist) > 0 && !containsFold(implementation.RuntimeAllowlist, request.Runtime) {
			continue
		}
		if len(implementation.Phases) > 0 && !containsPhase(implementation.Phases, request.Phase) {
			continue
		}
		if !computeCapabilityAtLeast(request.ComputeCapability, implementation.MinimumComputeCapability) {
			continue
		}
		if len(dtypes) > 0 && len(implementation.DTypes) > 0 && !setsIntersect(dtypes, normalizedSet(implementation.DTypes)) {
			continue
		}
		reasons := []string{"operator family matched", "hardware vendor matched"}
		compatibility := "review-required"
		if implementation.Kind == "runtime-bundled" {
			compatibility = "exact-runtime-baseline"
			reasons = append(reasons, "implementation is bundled in the pinned runtime")
		} else {
			reasons = append(reasons, "exact layout, shape, runtime ABI, and source revision still require validation")
		}
		if implementation.MinimumComputeCapability != "" {
			reasons = append(reasons, "minimum compute capability "+implementation.MinimumComputeCapability+" satisfied")
		}
		matches = append(matches, KernelMatch{ImplementationID: implementation.ID, Name: implementation.Name, Project: implementation.Project, SourceURL: implementation.SourceURL, License: implementation.License, Kind: implementation.Kind, Backend: implementation.Backend, Status: implementation.Status, Compatibility: compatibility, RevisionPolicy: implementation.RevisionPolicy, ExecutionBoundary: implementation.ExecutionBoundary, Priority: implementation.Priority, Reasons: reasons})
	}
	sort.Slice(matches, func(i, j int) bool {
		if matches[i].Priority != matches[j].Priority {
			return matches[i].Priority < matches[j].Priority
		}
		return matches[i].ImplementationID < matches[j].ImplementationID
	})
	return matches, nil
}

func normalizedSet(values []string) map[string]struct{} {
	result := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = strings.ToLower(strings.TrimSpace(value))
		value = strings.NewReplacer("float16", "fp16", "bfloat16", "bf16", "float32", "fp32").Replace(value)
		if value != "" {
			result[value] = struct{}{}
		}
	}
	return result
}

func setsIntersect(left, right map[string]struct{}) bool {
	for value := range left {
		if _, found := right[value]; found {
			return true
		}
	}
	return false
}

func containsOperator(values []OperatorFamily, wanted OperatorFamily) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func containsPhase(values []Phase, wanted Phase) bool {
	for _, value := range values {
		if value == wanted || value == PhaseMixed || wanted == PhaseMixed {
			return true
		}
	}
	return false
}

func containsFold(values []string, wanted string) bool {
	for _, value := range values {
		if strings.EqualFold(strings.TrimSpace(value), wanted) {
			return true
		}
	}
	return false
}

func computeCapabilityAtLeast(actual, minimum string) bool {
	if minimum == "" {
		return true
	}
	if actual == "" {
		return false
	}
	parse := func(value string) (int, bool) {
		value = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(value)), "sm_")
		value = strings.TrimPrefix(value, "sm")
		parsed, err := strconv.Atoi(value)
		return parsed, err == nil
	}
	a, aok := parse(actual)
	m, mok := parse(minimum)
	return aok && mok && a >= m
}
