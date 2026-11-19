package gputrace

import (
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/infercrane/infercrane/internal/gpuprofile"
)

// Thresholds of gpu-trace-analysis-v1. A change needs a new algorithm version.
const (
	// launchGapNS: an idle gap shorter than this between work the host had
	// already queued is launch latency, not a stall.
	launchGapNS = 30_000
	// stepGapNS: without step annotations, a GPU idle gap at least this long
	// separates two engine steps.
	stepGapNS     = 100_000
	seriesTarget  = 200
	minBucketNS   = 100_000
	maxTopBubbles = 10
)

// Options carries what a trace does not say about itself.
type Options struct {
	Engine        string
	EngineVersion string
	// Accelerator overrides the device name the trace reports.
	Accelerator string
	// Peak overrides the datasheet peak for the roofline.
	Peak *gpuprofile.Peak
	// Hardware is DCGM sampled during the capture, when it was.
	Hardware *gpuprofile.HardwareSeries
}

// Analyze reads a trace into a gpu-trace-analysis-v1 Analysis.
func Analyze(trace *Trace, options Options) (gpuprofile.Analysis, error) {
	if trace == nil || len(trace.Kernels) == 0 {
		return gpuprofile.Analysis{}, ErrNoKernels
	}
	p := prepare(trace)
	analysis := analyzePrepared(p, options)
	return analysis, gpuprofile.ValidateAnalysis(analysis)
}

func analyzePrepared(p *prepared, options Options) gpuprofile.Analysis {
	trace := p.trace
	window := p.window.end - p.window.start
	devices := int64(len(p.devices))
	streams := map[[2]int64]bool{}
	for _, kernel := range trace.Kernels {
		streams[[2]int64{int64(kernel.Device), kernel.Stream}] = true
	}
	analysis := gpuprofile.Analysis{
		AlgorithmVersion: gpuprofile.AnalysisAlgorithm,
		Trace: gpuprofile.TraceSource{
			Format: trace.Format, Devices: len(p.devices), Streams: len(streams), Kernels: int64(len(trace.Kernels)),
			CPUOpsRecorded: trace.CPUOpsRecorded, ShapesRecorded: trace.ShapesRecorded, LaunchesRecorded: len(p.launchKernels) > 0,
			WindowMS: math.Max(ms(window), 0.001), Truncated: trace.Truncated,
		},
		Hardware: options.Hardware,
	}
	if options.Engine == "vllm" || options.Engine == "sglang" {
		analysis.Trace.Engine = pointer(options.Engine)
	}
	if options.EngineVersion != "" {
		analysis.Trace.EngineVersion = pointer(options.EngineVersion)
	}
	capacity := window * devices
	analysis.Utilization = p.utilization(window, capacity)
	idle, hostWaitNS := p.idle(window, capacity)
	analysis.Idle = idle
	analysis.Launch = p.launches(window)
	analysis.Categories, analysis.TopKernels = p.kernels()
	analysis.Overlap = p.overlap()
	steps, stepOf := p.steps()
	analysis.Steps = steps
	analysis.Phases = p.phases(steps, stepOf)
	analysis.Roofline = p.roofline(options)
	analysis.Operators = p.operators(analysis.Roofline)
	analysis.Findings, analysis.Verdict = judge(analysis, hostWaitNS, capacity)
	return analysis
}

func (p *prepared) utilization(window, capacity int64) gpuprofile.Utilization {
	var busy, compute int64
	for _, device := range p.devices {
		busy += total(p.busy[device])
		var computeSpans []span
		for _, index := range p.byDevice[device] {
			if kernel := p.trace.Kernels[index]; isCompute(p.category(kernel)) {
				computeSpans = append(computeSpans, span{kernel.Start, kernel.End})
			}
		}
		merged, _ := union(computeSpans)
		compute += total(merged)
	}
	result := gpuprofile.Utilization{
		BusyShare: shareOf(busy, capacity), ComputeShare: shareOf(compute, capacity), NonComputeShare: shareOf(busy-compute, capacity),
	}
	result.IdleShare = round(1-result.BusyShare, 4)
	bucket := max(niceStep(int64(math.Ceil(float64(window)/seriesTarget))), minBucketNS)
	result.BucketMS = ms(bucket)
	for from := p.window.start; from < p.window.end && len(result.BusySeries) < gpuprofile.MaxSeries; from += bucket {
		to := min(from+bucket, p.window.end)
		var inside int64
		for _, device := range p.devices {
			inside += within(p.busy[device], from, to)
		}
		result.BusySeries = append(result.BusySeries, shareOf(inside, (to-from)*int64(len(p.devices))))
	}
	return result
}

// idle splits idle time by cause. A gap is host_wait when the launch of the
// work that ended it was still running, or had not started, when the GPU
// went idle; launch_gap when the work was queued and the gap is shorter than
// launchGapNS; other otherwise. Idle time before a device's first or after
// its last activity is other.
func (p *prepared) idle(window, capacity int64) (gpuprofile.IdleBreakdown, int64) {
	launches := len(p.launchKernels) > 0
	var idleNS, hostWait, launchGap int64
	var gaps []int64
	type bubble struct {
		start, length int64
		device        int32
		cause         string
	}
	var top []bubble
	for _, device := range p.devices {
		busy := p.busy[device]
		idleNS += window - total(busy)
		for index := 1; index < len(busy); index++ {
			start, end := busy[index-1].end, busy[index].start
			if end <= start {
				continue
			}
			length := end - start
			gaps = append(gaps, length)
			cause := "other"
			if launch, _, ok := p.launchOfKernel(p.trace.Kernels[p.busyFirst[device][index]]); ok && launches {
				switch {
				case launch.End > start:
					cause, hostWait = "host_wait", hostWait+length
				case length < launchGapNS:
					cause, launchGap = "launch_gap", launchGap+length
				}
			}
			top = append(top, bubble{start, length, device, cause})
			if len(top) > 4*maxTopBubbles {
				sort.Slice(top, func(i, j int) bool { return top[i].length > top[j].length })
				top = top[:maxTopBubbles]
			}
		}
	}
	result := gpuprofile.IdleBreakdown{IdleMS: ms(idleNS), Bubbles: gpuprofile.BubbleStats{Count: len(gaps), Top: []gpuprofile.Bubble{}}}
	if len(gaps) > 0 {
		sorted := sortedCopy(gaps)
		result.Bubbles.P50US = pointer(us(percentile(sorted, 0.5)))
		result.Bubbles.P95US = pointer(us(percentile(sorted, 0.95)))
		result.Bubbles.MaxUS = pointer(us(sorted[len(sorted)-1]))
	}
	sort.Slice(top, func(i, j int) bool {
		if top[i].length != top[j].length {
			return top[i].length > top[j].length
		}
		return top[i].start < top[j].start
	})
	for index := 0; index < len(top) && index < maxTopBubbles; index++ {
		result.Bubbles.Top = append(result.Bubbles.Top, gpuprofile.Bubble{
			StartMS: ms(top[index].start - p.window.start), DurationUS: us(top[index].length), Device: int(top[index].device), Cause: top[index].cause,
		})
	}
	switch {
	case !launches:
		result.Reason = "The trace has no host launch calls linked to GPU work, so idle time cannot be attributed to a cause."
	case idleNS == 0:
		result.HostWaitShare, result.LaunchGapShare, result.OtherShare = pointer(0.0), pointer(0.0), pointer(0.0)
	default:
		host, gap := shareOf(hostWait, idleNS), shareOf(launchGap, idleNS)
		result.HostWaitShare, result.LaunchGapShare = pointer(host), pointer(gap)
		result.OtherShare = pointer(round(math.Max(0, 1-host-gap), 4))
	}
	if !launches {
		return result, 0
	}
	return result, hostWait
}

func (p *prepared) launches(window int64) gpuprofile.LaunchStats {
	result := gpuprofile.LaunchStats{}
	perThread := map[int32]int64{}
	var delays []int64
	var withWork, short int64
	for index, launch := range p.trace.Launches {
		if !p.isLaunchAPI[index] {
			continue
		}
		result.Launches++
		if IsGraphLaunch(p.trace.Names[launch.Name]) {
			result.GraphLaunches++
		}
		perThread[launch.Thread] += launch.End - launch.Start
		kernels := p.launchKernels[index]
		if len(kernels) == 0 {
			continue
		}
		withWork++
		first := p.trace.Kernels[kernels[0]].Start
		var work int64
		for _, kernel := range kernels {
			first = min(first, p.trace.Kernels[kernel].Start)
			work += p.trace.Kernels[kernel].End - p.trace.Kernels[kernel].Start
		}
		delays = append(delays, max(0, first-launch.Start))
		if work < launch.End-launch.Start {
			short++
		}
	}
	result.PerSecond = round(float64(result.Launches)/(float64(window)/1e9), 1)
	if withWork == 0 {
		result.Reason = "The trace has no host launch calls linked to GPU work."
		return result
	}
	var busiest int64
	for _, spent := range perThread {
		busiest = max(busiest, spent)
	}
	sorted := sortedCopy(delays)
	result.HostLaunchShare = pointer(shareOf(busiest, window))
	result.LaunchDelayP50US = pointer(us(percentile(sorted, 0.5)))
	result.LaunchDelayP95US = pointer(us(percentile(sorted, 0.95)))
	result.ShortKernelShare = pointer(shareOf(short, withWork))
	return result
}

func (p *prepared) kernels() ([]gpuprofile.CategoryTime, []gpuprofile.KernelStat) {
	type totals struct {
		count, total, longest int64
	}
	byCategory := map[string]*totals{}
	byName := map[int32]*totals{}
	var all int64
	for _, kernel := range p.trace.Kernels {
		length := kernel.End - kernel.Start
		category := p.category(kernel)
		if byCategory[category] == nil {
			byCategory[category] = &totals{}
		}
		byCategory[category].count++
		byCategory[category].total += length
		if byName[kernel.Name] == nil {
			byName[kernel.Name] = &totals{}
		}
		entry := byName[kernel.Name]
		entry.count++
		entry.total += length
		entry.longest = max(entry.longest, length)
		all += length
	}
	categories := []gpuprofile.CategoryTime{}
	for _, category := range gpuprofile.Categories {
		if entry := byCategory[category]; entry != nil {
			categories = append(categories, gpuprofile.CategoryTime{Category: category, KernelMS: ms(entry.total), Share: shareOf(entry.total, all), Count: entry.count})
		}
	}
	sort.SliceStable(categories, func(i, j int) bool { return categories[i].KernelMS > categories[j].KernelMS })
	names := make([]int32, 0, len(byName))
	for name := range byName {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool {
		left, right := byName[names[i]].total, byName[names[j]].total
		if left != right {
			return left > right
		}
		return p.trace.Names[names[i]] < p.trace.Names[names[j]]
	})
	top := []gpuprofile.KernelStat{}
	for _, name := range names {
		if len(top) == gpuprofile.MaxTopKernels {
			break
		}
		entry := byName[name]
		stat := gpuprofile.KernelStat{
			Name: p.trace.Names[name], Category: p.categories[name], Count: entry.count, TotalMS: ms(entry.total),
			MeanUS: us(entry.total / entry.count), MaxUS: us(entry.longest), Share: shareOf(entry.total, all),
		}
		if phase := KernelPhase(stat.Name); phase != "" {
			stat.Phase = pointer(phase)
		}
		top = append(top, stat)
	}
	return categories, top
}

func (p *prepared) overlap() gpuprofile.Overlap {
	var communication, overlapped int64
	for _, device := range p.devices {
		var commSpans, computeSpans []span
		for _, index := range p.byDevice[device] {
			kernel := p.trace.Kernels[index]
			switch category := p.category(kernel); {
			case category == gpuprofile.CategoryCommunication:
				commSpans = append(commSpans, span{kernel.Start, kernel.End})
			case isCompute(category):
				computeSpans = append(computeSpans, span{kernel.Start, kernel.End})
			}
		}
		comm, _ := union(commSpans)
		comp, _ := union(computeSpans)
		communication += total(comm)
		overlapped += intersection(comm, comp)
	}
	result := gpuprofile.Overlap{CommunicationMS: ms(communication)}
	if communication == 0 {
		result.Reason = "No collective communication kernels ran, so there is nothing to overlap."
		return result
	}
	result.OverlappedShare = pointer(shareOf(overlapped, communication))
	return result
}

type step struct {
	start, end int64
	kernels    []int
	phase      string
	named      bool
}

// steps finds engine steps from step annotations, or else from GPU idle gaps
// of at least stepGapNS on the busiest device, and maps every kernel to its
// step (-1 for none).
func (p *prepared) steps() (gpuprofile.StepStats, []int) {
	stepOf := make([]int, len(p.trace.Kernels))
	for index := range stepOf {
		stepOf[index] = -1
	}
	result := gpuprofile.StepStats{Source: "none", Items: []gpuprofile.StepItem{}}
	var found []step
	counts := map[string]int{}
	for _, op := range p.trace.Ops {
		if marker := stepMarker(p.trace.Names[op.Name]); marker != "" {
			counts[marker]++
		}
	}
	chosen, best := "", 1
	for marker, count := range counts {
		if count > best || count == best && marker < chosen {
			chosen, best = marker, count
		}
	}
	if chosen != "" {
		var lastEnd int64 = math.MinInt64
		for _, op := range p.trace.Ops {
			if stepMarker(p.trace.Names[op.Name]) != chosen || op.Start < lastEnd {
				continue
			}
			found = append(found, step{start: op.Start, end: op.End, phase: annotationPhase(p.trace.Names[op.Name])})
			lastEnd = op.End
		}
		p.assignByLaunch(found, stepOf)
		result.Source, result.Marker = "annotations", pointer(p.trace.Names[firstOpNamed(p.trace, chosen)])
	} else {
		found = p.gapSteps()
		p.assignByStart(found, stepOf)
		if len(found) >= 2 {
			result.Source = "idle_gaps"
		}
	}
	if len(found) < 2 {
		result.Source, result.Marker = "none", nil
		result.Reason = "The trace has no step annotations and too few GPU idle gaps to separate engine steps."
		for index := range stepOf {
			stepOf[index] = -1
		}
		return result, stepOf
	}
	for index := range stepOf {
		if stepOf[index] >= 0 {
			found[stepOf[index]].kernels = append(found[stepOf[index]].kernels, index)
		}
	}
	durations := make([]int64, len(found))
	items := make([]gpuprofile.StepItem, len(found))
	for index := range found {
		current := &found[index]
		phases := map[string]bool{}
		perDevice := map[int32][]span{}
		end := current.end
		for _, kernel := range current.kernels {
			k := p.trace.Kernels[kernel]
			if phase := KernelPhase(p.trace.Names[k.Name]); phase != "" {
				phases[phase] = true
			}
			perDevice[k.Device] = append(perDevice[k.Device], span{k.Start, k.End})
			if result.Source == "annotations" {
				end = max(end, k.End)
			}
		}
		if current.phase != "" {
			current.named = true
		} else {
			switch {
			case phases[gpuprofile.PhasePrefill] && phases[gpuprofile.PhaseDecode]:
				current.phase = gpuprofile.PhaseMixed
			case phases[gpuprofile.PhasePrefill]:
				current.phase = gpuprofile.PhasePrefill
			case phases[gpuprofile.PhaseDecode]:
				current.phase = gpuprofile.PhaseDecode
			default:
				current.phase = gpuprofile.PhaseUnknown
			}
		}
		var busy int64
		for _, spans := range perDevice {
			sort.Slice(spans, func(i, j int) bool { return spans[i].start < spans[j].start })
			merged, _ := union(spans)
			busy += total(merged)
		}
		if len(perDevice) > 0 {
			busy /= int64(len(perDevice))
		}
		durations[index] = end - current.start
		items[index] = gpuprofile.StepItem{
			StartMS: ms(max(0, current.start-p.window.start)), DurationMS: ms(end - current.start), GPUBusyMS: ms(busy),
			Kernels: len(current.kernels), Phase: current.phase,
		}
	}
	sorted := sortedCopy(durations)
	result.Count = len(found)
	result.P50MS, result.P95MS = pointer(ms(percentile(sorted, 0.5))), pointer(ms(percentile(sorted, 0.95)))
	stride := max(1, int(math.Ceil(float64(len(items))/gpuprofile.MaxStepItems)))
	for index := 0; index < len(items); index += stride {
		result.Items = append(result.Items, items[index])
	}
	p.stepList = found
	return result, stepOf
}

func firstOpNamed(trace *Trace, marker string) int32 {
	for _, op := range trace.Ops {
		if stepMarker(trace.Names[op.Name]) == marker {
			return op.Name
		}
	}
	return 0
}

// assignByLaunch maps a kernel to the step whose host range contains its
// launch, or, without a launch, the step whose range contains its start.
func (p *prepared) assignByLaunch(steps []step, stepOf []int) {
	if len(steps) == 0 {
		return
	}
	locate := func(at int64) int {
		index := sort.Search(len(steps), func(i int) bool { return steps[i].start > at }) - 1
		if index >= 0 && at <= steps[index].end {
			return index
		}
		return -1
	}
	for index, kernel := range p.trace.Kernels {
		if launch, _, ok := p.launchOfKernel(kernel); ok {
			stepOf[index] = locate(launch.Start)
			continue
		}
		stepOf[index] = locate(kernel.Start)
	}
}

func (p *prepared) gapSteps() []step {
	var reference int32
	var most int64 = -1
	for _, device := range p.devices {
		if busy := total(p.busy[device]); busy > most {
			reference, most = device, busy
		}
	}
	busy := p.busy[reference]
	if len(busy) == 0 {
		return nil
	}
	var found []step
	current := step{start: busy[0].start, end: busy[0].end}
	for _, segment := range busy[1:] {
		if segment.start-current.end >= stepGapNS {
			found = append(found, current)
			current = step{start: segment.start, end: segment.end}
			continue
		}
		current.end = segment.end
	}
	return append(found, current)
}

func (p *prepared) assignByStart(steps []step, stepOf []int) {
	for index, kernel := range p.trace.Kernels {
		position := sort.Search(len(steps), func(i int) bool { return steps[i].start > kernel.Start }) - 1
		if position >= 0 {
			stepOf[index] = position
		}
	}
}

func (p *prepared) phases(steps gpuprofile.StepStats, stepOf []int) gpuprofile.PhaseSplit {
	result := gpuprofile.PhaseSplit{Source: "unknown"}
	byPhase := map[string]int64{}
	stepsByPhase := map[string]int{}
	var all int64
	named, hinted := false, false
	for _, current := range p.stepList {
		stepsByPhase[current.phase]++
		if current.named {
			named = true
		} else if current.phase != gpuprofile.PhaseUnknown {
			hinted = true
		}
	}
	for index, kernel := range p.trace.Kernels {
		length := kernel.End - kernel.Start
		all += length
		phase := gpuprofile.PhaseUnknown
		if stepOf[index] >= 0 && steps.Source != "none" {
			phase = p.stepList[stepOf[index]].phase
		}
		byPhase[phase] += length
	}
	switch {
	case named:
		result.Source = "annotations"
	case hinted:
		result.Source = "kernel_names"
	}
	read := func(phase string) gpuprofile.PhaseTime {
		return gpuprofile.PhaseTime{GPUMS: ms(byPhase[phase]), Share: shareOf(byPhase[phase], all), Steps: stepsByPhase[phase]}
	}
	result.Prefill, result.Decode, result.Mixed, result.Unknown = read(gpuprofile.PhasePrefill), read(gpuprofile.PhaseDecode), read(gpuprofile.PhaseMixed), read(gpuprofile.PhaseUnknown)
	return result
}

// percent writes a share as a percentage with one decimal below 10%.
func percent(share float64) string {
	value := share * 100
	if value < 10 {
		return strings.TrimSuffix(fmt.Sprintf("%.1f", value), ".0") + "%"
	}
	return fmt.Sprintf("%.0f%%", value)
}

func seconds(windowMS float64) string {
	if windowMS < 1000 {
		return fmt.Sprintf("%.0f ms", windowMS)
	}
	return fmt.Sprintf("%.1f s", windowMS/1000)
}
