package controlapi

import (
	"encoding/json"
	"math"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/gpuprofile"
)

func kernelScreenStore(profile gpuprofile.Profile) *fakeGPUProfileStore {
	return &fakeGPUProfileStore{
		fakeStore: &fakeStore{principal: domain.Principal{ID: "viewer-1", TenantID: "tenant-1", Name: "viewer", Role: "viewer", Scopes: []string{"read"}}},
		profile:   profile,
	}
}

func traceProfile(accelerator *string) gpuprofile.Profile {
	now := time.Now().UTC().Truncate(time.Second)
	engine, decode, prefill := "vllm", "decode", "prefill"
	return gpuprofile.Profile{
		SchemaVersion: gpuprofile.SchemaVersionV2, Endpoint: "chat", Deployment: "chat-prod", Revision: 3,
		Capture: gpuprofile.Capture{ID: "gpc-" + strings.Repeat("c", 32), CaptureMetadata: gpuprofile.CaptureMetadata{
			Tool: "torch-profiler", ToolVersion: "2.9.0", EvidenceClass: "measured", Accelerator: accelerator, StartedAt: now.Add(-time.Minute), EndedAt: now,
		}},
		Timeline: gpuprofile.Timeline{BucketSeconds: 60, Buckets: []gpuprofile.Bucket{}}, Frames: []gpuprofile.Frame{}, Stacks: []gpuprofile.Stack{}, UnavailableLayers: []string{}, RecordedAt: now,
		Analysis: &gpuprofile.Analysis{
			Trace: gpuprofile.TraceSource{Engine: &engine},
			// 1,000 ms of kernel time in every category; only named kernels are attributed.
			Categories: []gpuprofile.CategoryTime{{Category: "gemm", KernelMS: 600}, {Category: "norm", KernelMS: 120}, {Category: "attention", KernelMS: 200}, {Category: "other", KernelMS: 80}},
			TopKernels: []gpuprofile.KernelStat{
				{Name: "sm90_xmma_gemm_bf16bf16_bf16f32", Category: "gemm", TotalMS: 550},
				{Name: "fused_add_rms_norm_kernel", Category: "norm", TotalMS: 90},
				{Name: "rms_norm_kernel", Category: "norm", TotalMS: 30},
				{Name: "flashinfer_batch_decode_kernel", Category: "attention", Phase: &decode, TotalMS: 150},
				{Name: "flash_attn_varlen_fwd_kernel", Category: "attention", Phase: &prefill, TotalMS: 40},
				{Name: "silu_and_mul_kernel", Category: "activation", TotalMS: 20},
				{Name: "flash_attn_unknown_phase", Category: "attention", TotalMS: 10},
				{Name: "ncclDevKernel_AllReduce", Category: "communication", TotalMS: 50},
			},
		},
	}
}

func readKernelScreen(t *testing.T, store *fakeGPUProfileStore) endpointKernelScreen {
	t.Helper()
	handler := (API{Store: store, Authenticator: store}).Handler()
	response := serve(handler, http.MethodGet, "/api/v1/endpoints/chat/kernel-opportunities", "viewer-session", "")
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var screen endpointKernelScreen
	if err := json.Unmarshal(response.Body.Bytes(), &screen); err != nil {
		t.Fatal(err)
	}
	return screen
}

func TestKernelScreenReadsTheTraceWithThePlannerGates(t *testing.T) {
	accelerator := "NVIDIA H100 80GB HBM3"
	screen := readKernelScreen(t, kernelScreenStore(traceProfile(&accelerator)))
	if screen.State != kernelScreenReady || screen.Source != kernelScreenTraceKernels || screen.Hardware == nil || screen.Hardware.Vendor != "nvidia" || screen.Runtime == nil || *screen.Runtime != "vllm" {
		t.Fatalf("screen = %+v", screen)
	}
	if screen.Profile.ID != "gpc-"+strings.Repeat("c", 32) || screen.Profile.Deployment != "chat-prod" || screen.Profile.Revision != 3 {
		t.Fatalf("profile = %+v", screen.Profile)
	}
	shares := map[string]float64{}
	for _, item := range screen.Opportunities {
		shares[item.HotspotID] = item.DeviceTimeFraction
	}
	// Both norm kernels add up; attention splits by the phase each kernel ran in;
	// a kernel without a phase and the communication kernel are never attributed.
	want := map[string]float64{"quantized-linear": .55, "residual-rmsnorm": .12, "attention-decode": .15, "attention-prefill": .04, "swiglu": .02}
	if len(shares) != len(want) {
		t.Fatalf("shares = %v", shares)
	}
	for id, share := range want {
		if math.Abs(shares[id]-share) > 1e-9 {
			t.Fatalf("%s share = %v, want %v", id, shares[id], share)
		}
	}
	if math.Abs(screen.ClassifiedShare-.88) > 1e-9 {
		t.Fatalf("classified share = %v", screen.ClassifiedShare)
	}
	if screen.Opportunities[0].HotspotID != "quantized-linear" {
		t.Fatalf("largest first: %+v", screen.Opportunities)
	}
	for _, item := range screen.Opportunities {
		switch item.HotspotID {
		case "residual-rmsnorm":
			if !item.Eligible || item.RequiredKernelSpeedup == nil || item.Kernel != "fused_add_rms_norm_kernel" || len(item.ExistingImplementations) == 0 || item.ExistingImplementations[0].ImplementationID != "runtime-vllm" {
				t.Fatalf("norm = %+v", item)
			}
		case "attention-prefill", "swiglu":
			if item.Eligible || item.RequiredKernelSpeedup != nil || !strings.Contains(item.Reason, "below policy minimum") {
				t.Fatalf("%s = %+v", item.HotspotID, item)
			}
		}
	}
	if screen.Policy.MinHotspotFraction != .05 || screen.Policy.TargetEndToEndSpeedup != 1.02 || !strings.Contains(screen.Boundary, "never a gain") {
		t.Fatalf("policy=%+v boundary=%q", screen.Policy, screen.Boundary)
	}
}

func TestKernelScreenWithoutAnAcceleratorKeepsSharesAndChoosesNoTemplate(t *testing.T) {
	screen := readKernelScreen(t, kernelScreenStore(traceProfile(nil)))
	if screen.State != kernelScreenAcceleratorNotRecorded || screen.Hardware != nil || len(screen.Opportunities) != 5 {
		t.Fatalf("screen = %+v", screen)
	}
	for _, item := range screen.Opportunities {
		if item.Eligible || item.Template != "" || item.RequiredKernelSpeedup != nil || !strings.Contains(item.Reason, "did not record its accelerator") {
			t.Fatalf("item = %+v", item)
		}
	}
}

func TestKernelScreenOfSampledStacksLeavesAttentionOut(t *testing.T) {
	accelerator := "MI300X"
	profile := traceProfile(&accelerator)
	profile.SchemaVersion, profile.Analysis = gpuprofile.SchemaVersion, nil
	profile.Frames = []gpuprofile.Frame{
		{Kind: "python", Name: "forward"}, {Kind: "kernel", Name: "Cijk_Ailk_Bljk_gemm"}, {Kind: "kernel", Name: "rmsnorm_kernel"},
		{Kind: "kernel", Name: "paged_attention_kernel"}, {Kind: "stall", Name: "long_scoreboard"},
	}
	profile.Stacks = []gpuprofile.Stack{
		{Frames: []int{0, 1, 4}, GPUNS: 700}, {Frames: []int{0, 2}, GPUNS: 200}, {Frames: []int{0, 3}, GPUNS: 50}, {Frames: []int{0}, GPUNS: 50},
	}
	screen := readKernelScreen(t, kernelScreenStore(profile))
	if screen.State != kernelScreenReady || screen.Source != kernelScreenSampledStacks || screen.Hardware == nil || screen.Hardware.Vendor != "amd" || screen.Runtime != nil {
		t.Fatalf("screen = %+v", screen)
	}
	if len(screen.Opportunities) != 2 || screen.Opportunities[0].HotspotID != "quantized-linear" || screen.Opportunities[0].DeviceTimeFraction != .7 || screen.Opportunities[1].DeviceTimeFraction != .2 {
		t.Fatalf("opportunities = %+v", screen.Opportunities)
	}
	if !strings.Contains(strings.Join(screen.Limitations, " "), "attention kernels are left out") {
		t.Fatalf("limitations = %v", screen.Limitations)
	}
}

func TestKernelScreenNamesWhatItCannotScreen(t *testing.T) {
	accelerator := "H100"
	profile := traceProfile(&accelerator)
	profile.Analysis.TopKernels = []gpuprofile.KernelStat{{Name: "ncclDevKernel_AllReduce", Category: "communication", TotalMS: 50}}
	if screen := readKernelScreen(t, kernelScreenStore(profile)); screen.State != kernelScreenNoReviewedOperator || len(screen.Opportunities) != 0 {
		t.Fatalf("screen = %+v", screen)
	}
	profile.Analysis.Categories = []gpuprofile.CategoryTime{}
	if screen := readKernelScreen(t, kernelScreenStore(profile)); screen.State != kernelScreenNoKernelTime {
		t.Fatalf("screen = %+v", screen)
	}
}

func TestKernelScreenErrors(t *testing.T) {
	store := kernelScreenStore(gpuprofile.Profile{})
	handler := (API{Store: store, Authenticator: store}).Handler()
	store.readErr = gpuprofile.ErrNotRecorded
	if response := serve(handler, http.MethodGet, "/api/v1/endpoints/chat/kernel-opportunities", "viewer-session", ""); response.Code != http.StatusNotFound || !strings.Contains(response.Body.String(), `"gpu_profile_not_found"`) {
		t.Fatalf("not recorded: %d %s", response.Code, response.Body.String())
	}
	store.readErr = domain.ErrNotFound
	if response := serve(handler, http.MethodGet, "/api/v1/endpoints/missing/kernel-opportunities", "viewer-session", ""); response.Code != http.StatusNotFound || !strings.Contains(response.Body.String(), `"not_found"`) {
		t.Fatalf("missing endpoint: %d %s", response.Code, response.Body.String())
	}
	if response := serve(handler, http.MethodGet, "/api/v1/endpoints/chat/kernel-opportunities?phase=decode", "viewer-session", ""); response.Code != http.StatusBadRequest {
		t.Fatalf("query: %d", response.Code)
	}
	bare := (API{Store: &fakeStore{}, APIKey: "secret"}).Handler()
	if response := serve(bare, http.MethodGet, "/api/v1/endpoints/chat/kernel-opportunities", "secret", ""); response.Code != http.StatusNotImplemented {
		t.Fatalf("store without profiles: %d", response.Code)
	}
}
