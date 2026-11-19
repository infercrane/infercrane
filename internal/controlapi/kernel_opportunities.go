package controlapi

import (
	"errors"
	"net/http"
	"sort"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/gpuprofile"
	"github.com/infercrane/infercrane/internal/kernelplanner"
	"github.com/infercrane/infercrane/internal/provideridentity"
)

const (
	kernelScreenBoundary = "Operators a lab run may try, screened from a recorded GPU profile. Nothing was built or measured: a ceiling is the end-to-end speedup if the operator took no time, never a gain, and only a qualified lab experiment's measured change is a result."

	kernelScreenTraceKernels  = "trace_top_kernels"
	kernelScreenSampledStacks = "sampled_stacks"

	kernelScreenReady                  = "screened"
	kernelScreenAcceleratorNotRecorded = "accelerator_not_recorded"
	kernelScreenNoReviewedOperator     = "no_reviewed_operator"
	kernelScreenNoKernelTime           = "no_kernel_time"
)

type endpointKernelScreen struct {
	Endpoint        string                   `json:"endpoint"`
	State           string                   `json:"state"`
	Profile         kernelScreenProfile      `json:"profile"`
	Hardware        *kernelScreenHardware    `json:"hardware"`
	Runtime         *string                  `json:"runtime"`
	Source          string                   `json:"source"`
	ClassifiedShare float64                  `json:"classified_share"`
	Policy          kernelScreenPolicy       `json:"policy"`
	Opportunities   []kernelplanner.Screened `json:"opportunities"`
	Limitations     []string                 `json:"limitations"`
	Boundary        string                   `json:"boundary"`
}

type kernelScreenProfile struct {
	ID            string    `json:"id"`
	Deployment    string    `json:"deployment"`
	Revision      int       `json:"revision"`
	SchemaVersion string    `json:"schema_version"`
	Tool          string    `json:"tool"`
	ToolVersion   string    `json:"tool_version"`
	Accelerator   *string   `json:"accelerator"`
	StartedAt     time.Time `json:"started_at"`
	EndedAt       time.Time `json:"ended_at"`
	RecordedAt    time.Time `json:"recorded_at"`
}

type kernelScreenHardware struct {
	Vendor      string `json:"vendor"`
	Accelerator string `json:"accelerator"`
}

type kernelScreenPolicy struct {
	MinHotspotFraction    float64 `json:"min_hotspot_fraction"`
	MinAmdahlCeiling      float64 `json:"min_amdahl_ceiling"`
	TargetEndToEndSpeedup float64 `json:"target_end_to_end_speedup"`
}

// endpointKernelOpportunities screens the newest GPU profile of the active
// revision behind an endpoint with the kernel planner's own gates and kernel
// vocabulary. It reads stored evidence only: nothing runs, nothing is spent,
// and no request identity is pinned. A lab run profiles again on the target
// GPU before it plans an experiment.
func (a API) endpointKernelOpportunities(w http.ResponseWriter, r *http.Request) {
	store, ok := a.Store.(gpuProfileStore)
	if !ok {
		writeError(w, http.StatusNotImplemented, "capability_unavailable", "GPU profile storage is not configured")
		return
	}
	if len(r.URL.Query()) > 0 {
		writeError(w, http.StatusBadRequest, "invalid_request", "kernel-opportunities accepts no query parameters")
		return
	}
	actor := r.Context().Value(identityKey{}).(domain.Principal)
	profile, err := store.EndpointGPUProfile(r.Context(), actor.TenantID, r.PathValue("name"))
	switch {
	case errors.Is(err, domain.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", "endpoint was not found")
		return
	case errors.Is(err, gpuprofile.ErrNotRecorded):
		writeError(w, http.StatusNotFound, "gpu_profile_not_found", "no GPU profile is recorded for the active revision of this endpoint")
		return
	case err != nil:
		writeError(w, http.StatusInternalServerError, "internal", "GPU profile could not be read")
		return
	}
	screen, err := screenKernelProfile(r.PathValue("name"), profile)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "the GPU profile could not be screened for kernel opportunities")
		return
	}
	writeJSON(w, http.StatusOK, screen)
}

func screenKernelProfile(endpoint string, profile gpuprofile.Profile) (endpointKernelScreen, error) {
	policy := kernelplanner.DefaultPolicy(kernelplanner.Policy{})
	screen := endpointKernelScreen{
		Endpoint: endpoint,
		Profile: kernelScreenProfile{
			ID: profile.Capture.ID, Deployment: profile.Deployment, Revision: profile.Revision, SchemaVersion: profile.SchemaVersion,
			Tool: profile.Capture.Tool, ToolVersion: profile.Capture.ToolVersion, Accelerator: profile.Capture.Accelerator,
			StartedAt: profile.Capture.StartedAt, EndedAt: profile.Capture.EndedAt, RecordedAt: profile.RecordedAt,
		},
		Policy: kernelScreenPolicy{
			MinHotspotFraction: policy.MinHotspotFraction, MinAmdahlCeiling: policy.MinMaxEndToEndSpeedup, TargetEndToEndSpeedup: policy.TargetEndToEndSpeedup,
		},
		Opportunities: []kernelplanner.Screened{},
		Limitations: []string{
			"A lab run profiles again on the target GPU, with the exact runtime image and workload, before it plans an experiment.",
			"Kernels outside the reviewed operator vocabulary stay in the denominator and are never opportunities.",
		},
		Boundary: kernelScreenBoundary,
	}
	hotspots, total, source := profileKernelTime(profile)
	screen.Source = source
	switch source {
	case kernelScreenTraceKernels:
		screen.Limitations = append(screen.Limitations, "Only the kernels the trace analysis lists by name are attributed; time in other kernels stays in the denominator.")
		if engine := profile.Analysis.Trace.Engine; engine != nil && *engine != "" {
			screen.Runtime = engine
		}
	default:
		screen.Limitations = append(screen.Limitations, "A sampled capture records no serving phase, so attention kernels are left out.")
	}
	if total <= 0 {
		screen.State = kernelScreenNoKernelTime
		return screen, nil
	}
	classified := 0.0
	for index := range hotspots {
		hotspots[index].DeviceTimeFraction /= total
		classified += hotspots[index].DeviceTimeFraction
	}
	screen.ClassifiedShare = min(classified, 1)
	// A family holding every nanosecond has no finite ceiling; the planner
	// refuses it, so the screen leaves it out rather than inventing one.
	finite := hotspots[:0]
	for _, hotspot := range hotspots {
		if hotspot.DeviceTimeFraction < 1 {
			finite = append(finite, hotspot)
		}
	}
	if len(finite) == 0 {
		screen.State = kernelScreenNoReviewedOperator
		return screen, nil
	}
	accelerator := ""
	if profile.Capture.Accelerator != nil {
		accelerator = *profile.Capture.Accelerator
	}
	vendor := plannerVendor(accelerator)
	if vendor == "" {
		screen.State = kernelScreenAcceleratorNotRecorded
		for _, hotspot := range finite {
			screen.Opportunities = append(screen.Opportunities, kernelplanner.Screened{
				HotspotID: hotspot.ID, Kernel: hotspot.Name, OperatorFamily: hotspot.Family, DeviceTimeFraction: hotspot.DeviceTimeFraction,
				MaxEndToEndSpeedup: 1 / (1 - hotspot.DeviceTimeFraction), Reason: "the capture did not record its accelerator, so no reviewed kernel template applies",
				ExistingImplementations: []kernelplanner.KernelMatch{},
			})
		}
		sort.SliceStable(screen.Opportunities, func(i, j int) bool {
			return screen.Opportunities[i].DeviceTimeFraction > screen.Opportunities[j].DeviceTimeFraction
		})
		return screen, nil
	}
	screen.Hardware = &kernelScreenHardware{Vendor: vendor, Accelerator: accelerator}
	runtime := ""
	if screen.Runtime != nil {
		runtime = *screen.Runtime
	}
	// Existing implementations are searched for any phase: the capture's
	// kernels may span both, and a lab run pins the phase it measures.
	screened, err := kernelplanner.Screen(kernelplanner.ScreenRequest{
		Runtime: runtime, Hardware: kernelplanner.HardwareIdentity{Vendor: vendor, Accelerator: accelerator},
		Phase: kernelplanner.PhaseMixed, Hotspots: finite, Policy: policy,
	})
	if err != nil {
		return screen, err
	}
	screen.State, screen.Opportunities = kernelScreenReady, screened
	return screen, nil
}

// profileKernelTime sums a capture's kernel time by reviewed operator family.
// A v2 capture's trace analysis names its busiest kernels with exact
// durations and phases, over the total kernel time of every category. A v1
// capture has only sampled stacks, attributed to the innermost kernel frame,
// over all sampled GPU time. Fractions are left as raw times here.
func profileKernelTime(profile gpuprofile.Profile) ([]kernelplanner.Hotspot, float64, string) {
	type family struct {
		time, top float64
		name      string
	}
	families := map[kernelplanner.OperatorFamily]*family{}
	add := func(name string, phase kernelplanner.Phase, time float64) {
		kind, known := kernelplanner.ClassifyKernel(name, phase)
		if !known || time <= 0 {
			return
		}
		entry := families[kind]
		if entry == nil {
			entry = &family{}
			families[kind] = entry
		}
		entry.time += time
		if time > entry.top || time == entry.top && name < entry.name {
			entry.top, entry.name = time, name
		}
	}
	var total float64
	source := kernelScreenSampledStacks
	if profile.Analysis != nil {
		source = kernelScreenTraceKernels
		for _, category := range profile.Analysis.Categories {
			total += category.KernelMS
		}
		for _, kernel := range profile.Analysis.TopKernels {
			phase := kernelplanner.Phase("")
			if kernel.Phase != nil {
				phase = kernelplanner.Phase(*kernel.Phase)
			}
			add(kernel.Name, phase, kernel.TotalMS)
		}
	} else {
		for _, stack := range profile.Stacks {
			total += float64(stack.GPUNS)
			for index := len(stack.Frames) - 1; index >= 0; index-- {
				frame := stack.Frames[index]
				if frame >= 0 && frame < len(profile.Frames) && profile.Frames[frame].Kind == gpuprofile.KindKernel {
					add(profile.Frames[frame].Name, "", float64(stack.GPUNS))
					break
				}
			}
		}
	}
	hotspots := make([]kernelplanner.Hotspot, 0, len(families))
	for kind, entry := range families {
		hotspots = append(hotspots, kernelplanner.Hotspot{ID: string(kind), Name: entry.name, Family: kind, DeviceTimeFraction: entry.time})
	}
	sort.Slice(hotspots, func(i, j int) bool { return hotspots[i].ID < hotspots[j].ID })
	return hotspots, total, source
}

// plannerVendor names the kernel planner's vendor for a recorded GPU: a
// reviewed accelerator's own vendor, else NVIDIA for the default CUDA
// platform, which is how every other GPU name runs. An unrecorded GPU has none.
func plannerVendor(accelerator string) string {
	if accelerator == "" {
		return ""
	}
	if reviewed, ok := provideridentity.LookupAccelerator(accelerator); ok {
		return reviewed.Vendor
	}
	if provideridentity.AcceleratorPlatform(accelerator) == provideridentity.PlatformCUDA {
		return "nvidia"
	}
	return ""
}
