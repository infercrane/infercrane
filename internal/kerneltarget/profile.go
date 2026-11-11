package kerneltarget

import (
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/infercrane/infercrane/internal/acceleratorlab"
	"github.com/infercrane/infercrane/internal/benchmark"
	"github.com/infercrane/infercrane/internal/kernelplanner"
)

const (
	maxTraceBytes        = 8 << 30
	profilerTool         = "torch-profiler"
	maxProfileConcurrent = 16
)

var defaultProfileRequest = json.RawMessage(`{"output_dir":"/profiles","activities":["CPU","GPU"],"with_stack":false,"record_shapes":true}`)

// operatorMarkers is reviewed vocabulary from kernel names observed in real
// traces. Unmatched kernels stay in the denominator but never become hotspots.
var operatorMarkers = []struct {
	family  kernelplanner.OperatorFamily
	markers []string
}{
	{kernelplanner.LinearRecurrence, []string{"deltanet", "delta_rule", "gdn", "linear_attn", "mamba"}},
	{kernelplanner.StateCommit, []string{"state_commit", "state_scatter", "scatter_state"}},
	{kernelplanner.ResidualRMSNorm, []string{"rmsnorm", "rms_norm", "layernorm", "norm_kernel"}},
	{kernelplanner.MoERouting, []string{"fused_moe", "moe_", "expert"}},
	{kernelplanner.QuantizedLinear, []string{"gemm", "matmul", "cutlass", "cublas", "wgmma", "nvjet"}},
	{kernelplanner.SwiGLU, []string{"swiglu", "silu_and_mul"}},
	{kernelplanner.KVCache, []string{"kv_cache", "kvcache"}},
	{kernelplanner.Sampling, []string{"sampling", "topk", "topp", "softmax"}},
}

var attentionMarkers = []string{"attention", "flash_attn", "flashinfer", "fmha", "mha_"}

func classify(name string, phase kernelplanner.Phase) (kernelplanner.OperatorFamily, bool) {
	lowered := strings.ToLower(name)
	for _, entry := range operatorMarkers {
		for _, marker := range entry.markers {
			if strings.Contains(lowered, marker) {
				return entry.family, true
			}
		}
	}
	for _, marker := range attentionMarkers {
		if strings.Contains(lowered, marker) {
			switch phase {
			case kernelplanner.PhasePrefill:
				return kernelplanner.AttentionPrefill, true
			case kernelplanner.PhaseDecode:
				return kernelplanner.AttentionDecode, true
			}
			return "", false // A mixed trace cannot say which attention path this was.
		}
	}
	return "", false
}

type traceEvent struct {
	Phase    string         `json:"ph"`
	Category string         `json:"cat"`
	Name     string         `json:"name"`
	Duration float64        `json:"dur"`
	Args     map[string]any `json:"args"`
}

type kernelTotals struct {
	durationUS float64
	count      int
}

// deviceKernels applies the same event filter that produced the measured H200
// profiles, so only GPU kernel time is counted.
func deviceKernels(events []traceEvent) (map[string]kernelTotals, float64) {
	kernels := map[string]kernelTotals{}
	var total float64
	for _, event := range events {
		category := strings.ToLower(event.Category)
		_, hasStream := event.Args["stream"]
		device, hasDevice := event.Args["device"]
		deviceLike := strings.Contains(category, "kernel") || category == "gpu" || hasStream || hasDevice && strings.Contains(strings.ToLower(fmt.Sprint(device)), "cuda")
		runtimeAPI := strings.Contains(category, "runtime") || strings.Contains(category, "driver")
		if event.Phase != "X" || !deviceLike || runtimeAPI || event.Duration <= 0 || event.Name == "" {
			continue
		}
		current := kernels[event.Name]
		current.durationUS += event.Duration
		current.count++
		kernels[event.Name] = current
		total += event.Duration
	}
	return kernels, total
}

func readTrace(path string) ([]traceEvent, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	var reader io.Reader = file
	if strings.HasSuffix(path, ".gz") {
		compressed, gzipErr := gzip.NewReader(file)
		if gzipErr != nil {
			return nil, gzipErr
		}
		defer compressed.Close()
		reader = compressed
	}
	var trace struct {
		TraceEvents []traceEvent `json:"traceEvents"`
	}
	if err = json.NewDecoder(io.LimitReader(reader, maxTraceBytes)).Decode(&trace); err != nil {
		return nil, err
	}
	return trace.TraceEvents, nil
}

func hashFile(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err = io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func traceFiles(directory string) ([]string, error) {
	var paths []string
	err := filepath.WalkDir(directory, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !entry.IsDir() && (strings.HasSuffix(path, ".trace.json") || strings.HasSuffix(path, ".trace.json.gz")) {
			paths = append(paths, path)
		}
		return nil
	})
	sort.Strings(paths)
	return paths, err
}

func digestTraces(directory string) (string, error) {
	paths, err := traceFiles(directory)
	if err != nil {
		return "", fmt.Errorf("find profiler traces: %w", err)
	}
	if len(paths) == 0 {
		return "", errors.New("runtime produced no profiler trace")
	}
	combined := sha256.New()
	for _, path := range paths {
		digest, hashErr := hashFile(path)
		if hashErr != nil {
			return "", fmt.Errorf("hash profiler trace: %w", hashErr)
		}
		_, _ = io.WriteString(combined, digest)
	}
	return "sha256:" + hex.EncodeToString(combined.Sum(nil)), nil
}

// summarizeTraces returns hotspots from the busiest trace and one digest
// covering every trace file captured.
func summarizeTraces(directory string, phase kernelplanner.Phase) ([]kernelplanner.Hotspot, string, error) {
	paths, err := traceFiles(directory)
	if err != nil {
		return nil, "", fmt.Errorf("find profiler traces: %w", err)
	}
	if len(paths) == 0 {
		return nil, "", errors.New("runtime produced no profiler trace")
	}
	digest, err := digestTraces(directory)
	if err != nil {
		return nil, "", err
	}
	var primary map[string]kernelTotals
	var primaryTotal float64
	for _, path := range paths {
		events, readErr := readTrace(path)
		if readErr != nil {
			return nil, "", fmt.Errorf("read profiler trace: %w", readErr)
		}
		if kernels, total := deviceKernels(events); total > primaryTotal {
			primary, primaryTotal = kernels, total
		}
	}
	if primaryTotal <= 0 {
		return nil, "", errors.New("profiler trace contains no measurable GPU kernel events")
	}
	type familyTotals struct {
		kernelTotals
		topName     string
		topDuration float64
	}
	families := map[kernelplanner.OperatorFamily]*familyTotals{}
	for name, totals := range primary {
		family, known := classify(name, phase)
		if !known {
			continue
		}
		entry := families[family]
		if entry == nil {
			entry = &familyTotals{}
			families[family] = entry
		}
		entry.durationUS += totals.durationUS
		entry.count += totals.count
		if totals.durationUS > entry.topDuration || totals.durationUS == entry.topDuration && name < entry.topName {
			entry.topName, entry.topDuration = name, totals.durationUS
		}
	}
	if len(families) == 0 {
		return nil, "", errors.New("profiler trace contains no reviewed operator family")
	}
	hotspots := make([]kernelplanner.Hotspot, 0, len(families))
	for family, totals := range families {
		hotspots = append(hotspots, kernelplanner.Hotspot{
			ID: string(family), Name: totals.topName, Family: family, DeviceTimeFraction: totals.durationUS / primaryTotal,
			MeanDurationUS: totals.durationUS / float64(totals.count), Invocations: totals.count,
		})
	}
	sort.Slice(hotspots, func(i, j int) bool {
		if hotspots[i].DeviceTimeFraction != hotspots[j].DeviceTimeFraction {
			return hotspots[i].DeviceTimeFraction > hotspots[j].DeviceTimeFraction
		}
		return hotspots[i].ID < hotspots[j].ID
	})
	return hotspots, digest, nil
}

func containerName(prefix, digest string) string {
	return "infercrane-" + prefix + "-" + strings.TrimPrefix(digest, "sha256:")[:16]
}

func (t Target) profileRequestBody(spec RuntimeSpec) []byte {
	if len(spec.ProfileRequest) > 0 {
		return spec.ProfileRequest
	}
	return defaultProfileRequest
}

// shapeForPhase mirrors the workload-separated capture used on real hardware:
// a prefill trace needs almost no output and a decode trace needs short inputs.
func shapeForPhase(workload acceleratorlab.Workload) (int, int) {
	switch workload.Phase {
	case kernelplanner.PhasePrefill:
		return workload.InputTokens, 1
	case kernelplanner.PhaseDecode:
		return min(256, workload.InputTokens), max(128, min(512, workload.OutputTokens))
	default:
		return workload.InputTokens, workload.OutputTokens
	}
}

// Capture records which GPU operators dominate the requested serving phase.
func (t Target) Capture(ctx context.Context, intent acceleratorlab.ProfileIntent) (acceleratorlab.ProfileEvidence, error) {
	started := t.now()
	if err := validateBoundary(intent.Model, intent.Topology); err != nil {
		return acceleratorlab.ProfileEvidence{}, err
	}
	if intent.Workload.Modality != acceleratorlab.ModalityText {
		return acceleratorlab.ProfileEvidence{}, errors.New("this target adapter profiles text workloads only")
	}
	spec, err := t.runtime(intent.Runtime)
	if err != nil {
		return acceleratorlab.ProfileEvidence{}, err
	}
	if err = t.verifyAccelerator(ctx, intent.Hardware); err != nil {
		return acceleratorlab.ProfileEvidence{}, err
	}
	image := spec.Repository + "@" + intent.Runtime.ImageDigest
	if err = t.materialize(ctx, spec, image, intent.Model); err != nil {
		return acceleratorlab.ProfileEvidence{}, err
	}
	if err = t.ensureNetwork(ctx); err != nil {
		return acceleratorlab.ProfileEvidence{}, err
	}
	profileDir, err := t.privateDirectory("profile-")
	if err != nil {
		return acceleratorlab.ProfileEvidence{}, err
	}
	instance, err := t.start(ctx, serverRequest{Spec: spec, Image: image, Model: intent.Model, Name: containerName("profile", intent.InputDigest), ProfileDir: profileDir})
	if err != nil {
		return acceleratorlab.ProfileEvidence{}, err
	}
	defer instance.stop()

	inputTokens, outputTokens := shapeForPhase(intent.Workload)
	concurrency := min(maxProfileConcurrent, max(1, intent.Workload.Concurrency))
	load := benchmark.Config{
		Binary: t.AIPerf, Endpoint: instance.baseURL, Model: intent.Model.Repository, Tokenizer: t.snapshotPath(intent.Model),
		Requests: max(16, concurrency*4), Concurrency: concurrency, InputTokens: inputTokens, OutputTokens: outputTokens, WarmupRequests: max(2, concurrency),
	}
	if _, err = instance.post(ctx, "/start_profile", t.profileRequestBody(spec)); err != nil {
		return acceleratorlab.ProfileEvidence{}, err
	}
	if _, err = t.Load.Run(ctx, load); err != nil {
		return acceleratorlab.ProfileEvidence{}, fmt.Errorf("drive profile workload: %w", err)
	}
	if _, err = instance.post(ctx, "/stop_profile", nil); err != nil {
		return acceleratorlab.ProfileEvidence{}, err
	}
	hotspots, digest, err := summarizeTraces(profileDir, intent.Workload.Phase)
	if err != nil {
		return acceleratorlab.ProfileEvidence{}, err
	}
	return acceleratorlab.ProfileEvidence{
		InputDigest: intent.InputDigest, Tool: profilerTool, ToolVersion: intent.Runtime.Name + "-" + intent.Runtime.Version,
		Hardware: intent.Hardware, RuntimeImageDigest: intent.Runtime.ImageDigest, Topology: intent.Topology,
		WorkloadDigest: intent.Workload.Digest, CapturedAt: t.now(), Hotspots: hotspots, CostUSD: t.elapsedCost(started), ArtifactDigest: digest,
	}, nil
}
