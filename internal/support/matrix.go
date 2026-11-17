// Package support owns the public qualification matrix without owning provider
// or runtime implementations. Lifecycle code remains provider-neutral; release
// policy decides which registered combinations are exposed to users.
package support

import (
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/infercrane/infercrane/internal/provideridentity"
	"github.com/infercrane/infercrane/internal/runtimecontract"
)

const SGLangRuntimeVersion = "0.5.12"

// RuntimeImage is one pinned build of a built-in runtime for one accelerator
// platform. Every platform of a runtime runs the same engine version, so the
// engine's flags and defaults are the same on every GPU it serves. Digests
// were read from the registry's manifest for Tag; a tag can move, the digest
// cannot.
type RuntimeImage struct {
	Runtime       string `json:"runtime"`
	Platform      string `json:"platform"`
	EngineVersion string `json:"engine_version"`
	Image         string `json:"image"`
	Tag           string `json:"tag"`
	// Stack is the GPU software stack inside the image.
	Stack string `json:"stack"`
	// Targets are the GPU compiler targets the image is built for; empty for
	// the CUDA images, which cover every CUDA GPU the engine supports.
	Targets []string `json:"targets,omitempty"`
}

var runtimeImages = []RuntimeImage{
	{Runtime: DefaultRuntime, Platform: provideridentity.PlatformCUDA, EngineVersion: DefaultRuntimeVersion,
		Image: "vllm/vllm-openai@sha256:953d3a06d5e64ab582985cd7401289d3abf2a2c14ef2158e9a84313daeec77d7", Tag: "vllm/vllm-openai:v0.22.1", Stack: "CUDA 13.0.2"},
	// One vLLM ROCm image serves both CDNA generations: its PyTorch and AITER
	// builds list gfx942 and gfx950.
	{Runtime: DefaultRuntime, Platform: provideridentity.PlatformROCmGFX942, EngineVersion: DefaultRuntimeVersion,
		Image: "vllm/vllm-openai-rocm@sha256:368b2992c776ad653e40e9e00d38aa51cf1e9a16558b05d07c42818abd9ec031", Tag: "vllm/vllm-openai-rocm:v0.22.1", Stack: "ROCm 7.2.2, AITER v0.1.13", Targets: []string{"gfx942", "gfx950"}},
	{Runtime: DefaultRuntime, Platform: provideridentity.PlatformROCmGFX950, EngineVersion: DefaultRuntimeVersion,
		Image: "vllm/vllm-openai-rocm@sha256:368b2992c776ad653e40e9e00d38aa51cf1e9a16558b05d07c42818abd9ec031", Tag: "vllm/vllm-openai-rocm:v0.22.1", Stack: "ROCm 7.2.2, AITER v0.1.13", Targets: []string{"gfx942", "gfx950"}},
	{Runtime: "sglang", Platform: provideridentity.PlatformCUDA, EngineVersion: SGLangRuntimeVersion,
		Image: "lmsysorg/sglang:v0.5.12@sha256:42194170546745092e74cd5f81ad32a7c6e944c7111fe7bf13588152277ff356", Tag: "lmsysorg/sglang:v0.5.12", Stack: "CUDA 13"},
	// SGLang builds one ROCm image per CDNA generation (mi30x, mi35x).
	{Runtime: "sglang", Platform: provideridentity.PlatformROCmGFX942, EngineVersion: SGLangRuntimeVersion,
		Image: "lmsysorg/sglang:v0.5.12-rocm720-mi30x@sha256:1f5c080be74269393bdc07d2cb30127dd2931c45b0b53e448235fb14d854da63", Tag: "lmsysorg/sglang:v0.5.12-rocm720-mi30x", Stack: "ROCm 7.2, AITER a6bb4993", Targets: []string{"gfx942"}},
	{Runtime: "sglang", Platform: provideridentity.PlatformROCmGFX950, EngineVersion: SGLangRuntimeVersion,
		Image: "lmsysorg/sglang:v0.5.12-rocm720-mi35x@sha256:3be1556105cdfdede103dc053f002d4d0307bdb229d0e15bfef130d5c53af208", Tag: "lmsysorg/sglang:v0.5.12-rocm720-mi35x", Stack: "ROCm 7.2, AITER a6bb4993", Targets: []string{"gfx950"}},
}

// RuntimeImages returns every pinned built-in runtime image.
func RuntimeImages() []RuntimeImage {
	out := make([]RuntimeImage, len(runtimeImages))
	for index, image := range runtimeImages {
		image.Targets = slices.Clone(image.Targets)
		out[index] = image
	}
	return out
}

// RuntimePlatforms lists the platforms a built-in runtime has an image for,
// in catalog order. A runtime without built-in images returns nil.
func RuntimePlatforms(runtime string) []string {
	var out []string
	for _, image := range runtimeImages {
		if image.Runtime == defaultRuntime(strings.TrimSpace(runtime)) {
			out = append(out, image.Platform)
		}
	}
	return out
}

// RuntimeWorkload is a built-in runtime's contract on one platform. It
// reports false when the runtime has no image for that platform.
func RuntimeWorkload(runtime, platform string) (runtimecontract.Workload, bool) {
	runtime = defaultRuntime(strings.TrimSpace(runtime))
	for _, image := range runtimeImages {
		if image.Runtime != runtime || image.Platform != platform {
			continue
		}
		var workload runtimecontract.Workload
		switch runtime {
		case DefaultRuntime:
			workload = vllmContract()
		case "sglang":
			workload = sglangContract()
		}
		workload.Image = image.Image
		return workload, true
	}
	return runtimecontract.Workload{}, false
}

// VLLMWorkload is vLLM's contract on the default CUDA platform.
func VLLMWorkload() runtimecontract.Workload {
	workload, _ := RuntimeWorkload(DefaultRuntime, provideridentity.PlatformCUDA)
	return workload
}

// SGLangWorkload is SGLang's contract on the default CUDA platform.
func SGLangWorkload() runtimecontract.Workload {
	workload, _ := RuntimeWorkload("sglang", provideridentity.PlatformCUDA)
	return workload
}

func vllmContract() runtimecontract.Workload {
	return runtimecontract.Workload{Command: []string{"vllm", "serve", "${MODEL}", "--revision", "${MODEL_REVISION}", "--host", "0.0.0.0", "--port", "${PORT}"}, Protocol: "openai", Port: 8000, ReadinessPath: "/health", ModelsPath: "/v1/models", MetricsPath: "/metrics", Cancellation: "http-disconnect", Drain: "connection", ShutdownGraceSeconds: 30}
}

func sglangContract() runtimecontract.Workload {
	return runtimecontract.Workload{Command: []string{"python3", "-m", "sglang.launch_server", "--model-path", "${MODEL}", "--revision", "${MODEL_REVISION}", "--host", "0.0.0.0", "--port", "${PORT}"}, Protocol: "openai", Port: 8000, ReadinessPath: "/health", ModelsPath: "/v1/models", MetricsPath: "/metrics", Cancellation: "http-disconnect", Drain: "connection", ShutdownGraceSeconds: 30}
}

// NormalizeWorkload fills the workload a revision stores. SGLang revisions
// store their image, chosen for the GPU's platform; vLLM revisions leave it
// empty and provisioning fills it the same way.
func NormalizeWorkload(runtime, gpu string, workload runtimecontract.Workload) runtimecontract.Workload {
	if workload.Empty() && runtime == "sglang" {
		if served, ok := RuntimeWorkload(runtime, provideridentity.AcceleratorPlatform(gpu)); ok {
			return served
		}
	}
	return workload
}

// ServedWorkload is the runtime contract a replica actually serves. A
// built-in runtime revision may leave its workload empty; provisioning then
// fills in that runtime's contract for the GPU's platform (see
// integration.RuntimeProfile.WorkloadFor). Readers that need the contract's
// paths, such as the metrics scrape during measurement, must use this rather
// than the stored, possibly empty draft.
func ServedWorkload(runtime, gpu string, workload runtimecontract.Workload) runtimecontract.Workload {
	if !workload.Empty() {
		return workload
	}
	if served, ok := RuntimeWorkload(runtime, provideridentity.AcceleratorPlatform(gpu)); ok {
		return served
	}
	return workload
}

const (
	DefaultRuntime = "vllm"
	// DefaultRuntimeVersion is the default vLLM runtime qualified by the public
	// support matrix. Persist it in every revision so benchmarks and
	// explanations never depend on an implicit image default.
	DefaultRuntimeVersion = "0.22.1"
	DefaultCloud          = "runpod"
	DefaultGPU            = "L40S"
	ElasticMode           = "elastic"
	ServerlessMode        = "serverless"
)

// Matrix is an immutable-by-convention set of qualified runtime and
// provider/compute-mode combinations. Adding an adapter does not silently make
// it public: qualification must be added here explicitly.
type Matrix struct {
	runtimes     map[string]struct{}
	clouds       map[string]map[string]struct{}
	combinations map[string]map[string]map[string]struct{}
	// platforms lists the accelerator platforms each cloud and mode can run.
	// A matrix without it does not check accelerators.
	platforms map[string]map[string][]string
}

// Qualified builds an exact runtime/provider/mode matrix. It avoids implying
// that every registered runtime works on every provider.
func Qualified(combinations map[string]map[string][]string) Matrix {
	m := Matrix{runtimes: map[string]struct{}{}, clouds: map[string]map[string]struct{}{}, combinations: map[string]map[string]map[string]struct{}{}}
	for cloud, modes := range combinations {
		m.clouds[cloud] = map[string]struct{}{}
		m.combinations[cloud] = map[string]map[string]struct{}{}
		for mode, runtimes := range modes {
			m.clouds[cloud][mode] = struct{}{}
			m.combinations[cloud][mode] = map[string]struct{}{}
			for _, runtime := range runtimes {
				m.runtimes[runtime] = struct{}{}
				m.combinations[cloud][mode][runtime] = struct{}{}
			}
		}
	}
	return m
}

func New(runtimes []string, clouds map[string][]string) Matrix {
	matrix := Matrix{runtimes: make(map[string]struct{}, len(runtimes)), clouds: make(map[string]map[string]struct{}, len(clouds))}
	for _, runtime := range runtimes {
		if runtime = strings.TrimSpace(runtime); runtime != "" {
			matrix.runtimes[runtime] = struct{}{}
		}
	}
	for cloud, modes := range clouds {
		cloud = strings.TrimSpace(cloud)
		if cloud == "" {
			continue
		}
		qualified := make(map[string]struct{}, len(modes))
		for _, mode := range modes {
			if mode = strings.TrimSpace(mode); mode != "" {
				qualified[mode] = struct{}{}
			}
		}
		matrix.clouds[cloud] = qualified
	}
	return matrix
}

// V1 is the public runtime/provider/compute qualification policy. Registered
// adapters remain private until their exact combination appears here.
func V1() Matrix {
	matrix := Qualified(map[string]map[string][]string{
		DefaultCloud: {ElasticMode: {DefaultRuntime, "custom-oci"}, ServerlessMode: {DefaultRuntime}},
		"aws":        {ElasticMode: {DefaultRuntime, "sglang", "custom-oci"}},
		"gcp":        {ElasticMode: {DefaultRuntime, "sglang", "custom-oci"}},
		"kubernetes": {ElasticMode: {DefaultRuntime, "sglang", "custom-oci"}},
	})
	cuda := []string{provideridentity.PlatformCUDA}
	withROCm := []string{provideridentity.PlatformCUDA, provideridentity.PlatformROCmGFX942, provideridentity.PlatformROCmGFX950}
	// RunPod rents AMD Instinct pods (MI300X, MI350X); its serverless worker
	// image is CUDA only. AWS and GCP offer no AMD Instinct instances. A
	// Kubernetes cluster runs whatever its device plugin exposes.
	matrix.platforms = map[string]map[string][]string{
		DefaultCloud: {ElasticMode: withROCm, ServerlessMode: cuda},
		"aws":        {ElasticMode: cuda},
		"gcp":        {ElasticMode: cuda},
		"kubernetes": {ElasticMode: withROCm},
	}
	return matrix
}

// PlatformQualification is one cloud and compute mode and the accelerator
// platforms qualified there.
type PlatformQualification struct {
	Cloud     string   `json:"cloud"`
	Mode      string   `json:"mode"`
	Platforms []string `json:"platforms"`
}

// AcceleratorPlatforms lists where each accelerator platform may run, sorted
// by cloud and mode. A matrix without platform data returns nil.
func (m Matrix) AcceleratorPlatforms() []PlatformQualification {
	var out []PlatformQualification
	for cloud, modes := range m.platforms {
		for mode, platforms := range modes {
			out = append(out, PlatformQualification{Cloud: cloud, Mode: mode, Platforms: slices.Clone(platforms)})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Cloud != out[j].Cloud {
			return out[i].Cloud < out[j].Cloud
		}
		return out[i].Mode < out[j].Mode
	})
	return out
}

// ValidateAccelerator checks that a provisioned GPU's platform can run: the
// built-in runtime has a pinned image for it, and the cloud and mode are
// qualified for it. An operator-declared SkyPilot cloud runs only the default
// CUDA platform. Attached compute (no GPU) is not checked.
func (m Matrix) ValidateAccelerator(runtime, cloud, mode, adapter, gpu string) error {
	if strings.TrimSpace(gpu) == "" {
		return nil
	}
	platform := provideridentity.AcceleratorPlatform(gpu)
	if platforms := RuntimePlatforms(runtime); platforms != nil && !slices.Contains(platforms, platform) {
		return fmt.Errorf("runtime %q has no pinned image for %s (platform %s)", defaultRuntime(runtime), gpu, platform)
	}
	// SkyPilot reaches its node over SSH; only the CUDA images carry it.
	if (adapter == "skypilot" || strings.HasPrefix(adapter, "skypilot-")) && platform != provideridentity.PlatformCUDA {
		return fmt.Errorf("%s needs the %s platform, which the SkyPilot adapter does not run; use provider adapter runpod-pods or kubernetes", gpu, platform)
	}
	if m.platforms == nil {
		return nil
	}
	if qualified, ok := m.platforms[cloud][mode]; ok {
		if slices.Contains(qualified, platform) {
			return nil
		}
	} else if mode == ElasticMode && adapter == "skypilot-"+cloud && platform == provideridentity.PlatformCUDA {
		return nil
	}
	var where []string
	for qualifiedCloud, modes := range m.platforms {
		for qualifiedMode, platforms := range modes {
			if slices.Contains(platforms, platform) {
				where = append(where, qualifiedCloud+" "+qualifiedMode)
			}
		}
	}
	sort.Strings(where)
	return fmt.Errorf("%s needs the %s platform, which is not qualified for cloud %q in %s mode; qualified: %s", gpu, platform, cloud, mode, strings.Join(where, ", "))
}

func (m Matrix) Validate(runtime, cloud, mode string) error {
	if err := m.ValidateRuntime(runtime); err != nil {
		return err
	}
	modes, ok := m.clouds[cloud]
	if !ok {
		return fmt.Errorf("provider cloud %q is not qualified; supported clouds: %s", cloud, strings.Join(sortedNestedKeys(m.clouds), ", "))
	}
	if _, ok = modes[mode]; !ok {
		return fmt.Errorf("compute mode %q is not qualified for cloud %q", mode, cloud)
	}
	if len(m.combinations) > 0 {
		if _, ok = m.combinations[cloud][mode][defaultRuntime(runtime)]; !ok {
			return fmt.Errorf("runtime %q is not qualified for cloud %q in %s mode", defaultRuntime(runtime), cloud, mode)
		}
	}
	return nil
}

// ValidateProvider applies the public qualification matrix, then admits one
// operator opt-in: a cloud the operator declared in a SkyPilot provider
// manifest, named by its exact adapter identity ("skypilot-<cloud>") in elastic
// mode with a known runtime. A cloud name alone is never enough, and the
// control plane still refuses to launch unless that adapter was registered
// from a manifest whose credentials are present.
func (m Matrix) ValidateProvider(runtime, cloud, mode, adapter string) error {
	err := m.Validate(runtime, cloud, mode)
	if err == nil {
		return nil
	}
	if mode != ElasticMode || adapter != "skypilot-"+cloud || !PortableCloud(cloud) {
		return err
	}
	return m.ValidateRuntime(runtime)
}

// PortableCloud reports whether a cloud name is a valid SkyPilot cloud
// identifier: lower-case letters, digits and inner dashes, at most 64 bytes.
func PortableCloud(cloud string) bool {
	if cloud == "" || len(cloud) > 64 {
		return false
	}
	isAlphaNumeric := func(char byte) bool { return char >= 'a' && char <= 'z' || char >= '0' && char <= '9' }
	if !isAlphaNumeric(cloud[0]) || !isAlphaNumeric(cloud[len(cloud)-1]) {
		return false
	}
	for _, char := range cloud {
		if char >= 'a' && char <= 'z' || char >= '0' && char <= '9' || char == '-' {
			continue
		}
		return false
	}
	return true
}

func defaultRuntime(runtime string) string {
	if runtime == "" {
		return DefaultRuntime
	}
	return runtime
}

func (m Matrix) ValidateRuntime(runtime string) error {
	if runtime == "" {
		runtime = DefaultRuntime
	}
	if _, ok := m.runtimes[runtime]; !ok {
		return fmt.Errorf("runtime %q is not qualified; supported runtimes: %s", runtime, strings.Join(sortedKeys(m.runtimes), ", "))
	}
	return nil
}

func sortedKeys(values map[string]struct{}) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func sortedNestedKeys(values map[string]map[string]struct{}) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
