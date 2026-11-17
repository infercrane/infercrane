package gputrace

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/infercrane/infercrane/internal/gpuprofile"
)

const (
	excerptTargetNS = int64(time.Second)
	minExcerptNS    = int64(time.Millisecond)
	otherKernels    = "(other kernels)"
)

// Capture describes when and with what a trace was recorded.
type Capture struct {
	Tool, ToolVersion string
	StartedAt         time.Time
	EndedAt           time.Time
	// GPUs overrides the device count the trace shows.
	GPUs int
}

// Profile analyzes a trace and builds the v2 recording a collector posts:
// the analysis, flame-graph stacks from the host operators down to each
// kernel, sampled GPU time per bucket, and an excerpt of the busiest second.
func Profile(trace *Trace, options Options, capture Capture) (gpuprofile.Recording, error) {
	if trace == nil || len(trace.Kernels) == 0 {
		return gpuprofile.Recording{}, ErrNoKernels
	}
	p := prepare(trace)
	analysis := analyzePrepared(p, options)
	if err := gpuprofile.ValidateAnalysis(analysis); err != nil {
		return gpuprofile.Recording{}, fmt.Errorf("analysis: %w", err)
	}
	started, ended := capture.StartedAt.UTC(), capture.EndedAt.UTC()
	window := p.window.end - p.window.start
	if started.IsZero() || !ended.After(started) {
		return gpuprofile.Recording{}, fmt.Errorf("capture needs a start before its end")
	}
	if trace.OriginKnown {
		// The profiler's own clock is exact; widen the capture to cover it
		// when the host clock that timed the capture differs slightly.
		zero := time.Unix(0, trace.OriginNS).UTC()
		started = minTime(started, zero.Add(time.Duration(p.window.start)).Truncate(time.Microsecond))
		ended = maxTime(ended, zero.Add(time.Duration(p.window.end)).Add(time.Microsecond).Truncate(time.Microsecond))
	}
	gpus := max(capture.GPUs, len(p.devices))
	accelerator := options.Accelerator
	if accelerator == "" {
		accelerator = trace.Accelerator()
	}
	metadata := gpuprofile.CaptureMetadata{
		Tool: capture.Tool, ToolVersion: capture.ToolVersion, EvidenceClass: "measured", GPUs: &gpus,
		StartedAt: started, EndedAt: ended, ContentRecorded: false,
	}
	if accelerator != "" {
		metadata.Accelerator = pointer(printable(accelerator, 128))
	}
	frames, stacks, unavailable := p.flame()
	recording := gpuprofile.Recording{
		SchemaVersion: gpuprofile.SchemaVersionV2, Capture: metadata, Timeline: p.buckets(started, ended),
		Frames: frames, Stacks: stacks, UnavailableLayers: unavailable, Analysis: &analysis,
	}
	if excerpt := p.excerpt(window); excerpt != nil {
		recording.TimelineExcerpt = excerpt
	}
	return gpuprofile.Normalize(recording), nil
}

// CaptureFromTrace times a capture by the trace's own clock when it has one,
// and otherwise ends it at end with the trace's length.
func CaptureFromTrace(trace *Trace, tool, toolVersion string, end time.Time) Capture {
	window := time.Duration(0)
	if len(trace.Kernels) > 0 {
		p := prepare(trace)
		window = time.Duration(p.window.end)
		if trace.OriginKnown {
			started := time.Unix(0, trace.OriginNS).UTC()
			return Capture{Tool: tool, ToolVersion: toolVersion, StartedAt: started, EndedAt: started.Add(max(window, time.Millisecond))}
		}
	}
	window = max(window, time.Millisecond)
	return Capture{Tool: tool, ToolVersion: toolVersion, StartedAt: end.Add(-window), EndedAt: end}
}

// flame attributes GPU time to root-first stacks. Where kernels overlap on
// one device, each gets an equal part of the overlapped time, so the stacks
// never hold more time than the GPUs had.
func (p *prepared) flame() ([]gpuprofile.Frame, []gpuprofile.Stack, []string) {
	exclusive := p.exclusiveTime()
	frameIndex := map[string]int{}
	var frames []gpuprofile.Frame
	frame := func(kind, name string) int {
		name = printable(name, gpuprofile.MaxFrameName)
		if name == "" {
			name = "(unnamed)"
		}
		key := kind + "\x00" + name
		if index, ok := frameIndex[key]; ok {
			return index
		}
		frames = append(frames, gpuprofile.Frame{Kind: kind, Name: name})
		frameIndex[key] = len(frames) - 1
		return len(frames) - 1
	}
	byStack := map[string]int64{}
	stackFrames := map[string][]int{}
	for index, kernel := range p.trace.Kernels {
		var path []int
		if _, launch, ok := p.launchOfKernel(kernel); ok {
			for _, op := range p.chains[launch] {
				path = append(path, frame(gpuprofile.KindTorch, p.trace.Names[p.trace.Ops[op].Name]))
			}
			path = append(path, frame(gpuprofile.KindCUDAAPI, p.trace.Names[p.trace.Launches[launch].Name]))
		}
		path = append(path, frame(gpuprofile.KindKernel, p.trace.Names[kernel.Name]))
		key := pathKey(path)
		byStack[key] += exclusive[index]
		stackFrames[key] = path
	}
	keys := make([]string, 0, len(byStack))
	for key, ns := range byStack {
		if ns >= 1 {
			keys = append(keys, key)
		}
	}
	sort.Slice(keys, func(i, j int) bool {
		if byStack[keys[i]] != byStack[keys[j]] {
			return byStack[keys[i]] > byStack[keys[j]]
		}
		return keys[i] < keys[j]
	})
	stacks := make([]gpuprofile.Stack, 0, min(len(keys), gpuprofile.MaxStacks))
	var rest int64
	for position, key := range keys {
		if position >= gpuprofile.MaxStacks-1 && len(keys) > gpuprofile.MaxStacks {
			rest += byStack[key]
			continue
		}
		stacks = append(stacks, gpuprofile.Stack{Frames: stackFrames[key], GPUNS: byStack[key]})
	}
	if rest > 0 {
		stacks = append(stacks, gpuprofile.Stack{Frames: []int{frame(gpuprofile.KindKernel, otherKernels)}, GPUNS: rest})
	}
	if len(frames) > gpuprofile.MaxFrames {
		frames, stacks = collapseToKernels(frames, stacks)
	}
	unavailable := []string{gpuprofile.KindPython, gpuprofile.KindNative, gpuprofile.KindSASS, gpuprofile.KindStall}
	if len(p.chains) == 0 {
		unavailable = append(unavailable, gpuprofile.KindTorch)
	}
	if len(p.launchKernels) == 0 {
		unavailable = append(unavailable, gpuprofile.KindCUDAAPI)
	}
	return frames, stacks, unavailable
}

// collapseToKernels keeps only kernel frames when a trace has more distinct
// host frames than one profile holds.
func collapseToKernels(frames []gpuprofile.Frame, stacks []gpuprofile.Stack) ([]gpuprofile.Frame, []gpuprofile.Stack) {
	index := map[string]int{}
	var kept []gpuprofile.Frame
	byKernel := map[int]int64{}
	for _, stack := range stacks {
		last := frames[stack.Frames[len(stack.Frames)-1]]
		position, ok := index[last.Name]
		if !ok {
			if len(kept) >= gpuprofile.MaxFrames-1 {
				last = gpuprofile.Frame{Kind: gpuprofile.KindKernel, Name: otherKernels}
			}
			if position, ok = index[last.Name]; !ok {
				kept = append(kept, last)
				position = len(kept) - 1
				index[last.Name] = position
			}
		}
		byKernel[position] += stack.GPUNS
	}
	collapsed := make([]gpuprofile.Stack, 0, len(byKernel))
	for position, ns := range byKernel {
		collapsed = append(collapsed, gpuprofile.Stack{Frames: []int{position}, GPUNS: ns})
	}
	sort.Slice(collapsed, func(i, j int) bool { return collapsed[i].Frames[0] < collapsed[j].Frames[0] })
	return kept, collapsed
}

func pathKey(path []int) string {
	var builder strings.Builder
	for _, index := range path {
		fmt.Fprintf(&builder, "%d,", index)
	}
	return builder.String()
}

// exclusiveTime splits each device's busy time among the activities running
// at each moment.
func (p *prepared) exclusiveTime() []int64 {
	result := make([]float64, len(p.trace.Kernels))
	for _, device := range p.devices {
		type edge struct {
			at     int64
			kernel int
			open   bool
		}
		indexes := p.byDevice[device]
		edges := make([]edge, 0, 2*len(indexes))
		for _, index := range indexes {
			kernel := p.trace.Kernels[index]
			edges = append(edges, edge{kernel.Start, index, true}, edge{kernel.End, index, false})
		}
		sort.Slice(edges, func(i, j int) bool {
			if edges[i].at != edges[j].at {
				return edges[i].at < edges[j].at
			}
			return !edges[i].open && edges[j].open
		})
		active := map[int]bool{}
		last := int64(0)
		for _, current := range edges {
			if len(active) > 0 && current.at > last {
				part := float64(current.at-last) / float64(len(active))
				for kernel := range active {
					result[kernel] += part
				}
			}
			last = current.at
			if current.open {
				active[current.kernel] = true
			} else {
				delete(active, current.kernel)
			}
		}
	}
	out := make([]int64, len(result))
	for index, ns := range result {
		out[index] = int64(math.Floor(ns))
	}
	return out
}

// buckets lays busy GPU time over the capture window. A bucket outside the
// traced window has no samples (null); one inside it is measured, so an idle
// bucket inside it is zero.
func (p *prepared) buckets(started, ended time.Time) gpuprofile.Timeline {
	length := ended.Sub(started)
	bucket := max(time.Second, time.Duration(math.Ceil(float64(length)/float64(gpuprofile.MaxBuckets)/float64(time.Second)))*time.Second)
	timeline := gpuprofile.Timeline{BucketSeconds: int(bucket / time.Second), Buckets: []gpuprofile.Bucket{}}
	// Trace time zero sits at the trace's own Unix time when known, and at
	// the capture start otherwise.
	zero := started
	if p.trace.OriginKnown {
		zero = time.Unix(0, p.trace.OriginNS).UTC()
	}
	for at := started; at.Before(ended) && len(timeline.Buckets) < gpuprofile.MaxBuckets; at = at.Add(bucket) {
		from, to := at.Sub(zero).Nanoseconds(), minTime(at.Add(bucket), ended).Sub(zero).Nanoseconds()
		entry := gpuprofile.Bucket{Start: at}
		if to > p.window.start && from < p.window.end {
			var busy int64
			for _, device := range p.devices {
				busy += within(p.busy[device], from, to)
			}
			seconds := round(float64(busy)/1e9, 6)
			entry.GPUSeconds = &seconds
		}
		timeline.Buckets = append(timeline.Buckets, entry)
	}
	return timeline
}

// excerpt cuts the busiest second of the trace (shorter when that second
// holds more events than an excerpt allows) into lanes for a timeline viewer.
func (p *prepared) excerpt(window int64) *gpuprofile.TimelineExcerpt {
	length := min(excerptTargetNS, window)
	if length <= 0 {
		return nil
	}
	start := p.busiestStart(length)
	for {
		if count := p.eventsIn(start, start+length); count <= gpuprofile.MaxExcerptEvents || length <= minExcerptNS {
			break
		}
		length /= 2
		start = p.busiestStart(length)
	}
	end := start + length
	excerpt := &gpuprofile.TimelineExcerpt{
		StartMS: ms(start - p.window.start), DurationMS: math.Max(ms(length), 0.001), Names: []string{}, Categories: []string{},
		Lanes: []gpuprofile.Lane{}, Events: [][4]float64{}, Steps: []gpuprofile.ExcerptStep{},
	}
	names := map[int32]int{}
	name := func(index int32, category string) int {
		if position, ok := names[index]; ok {
			return position
		}
		excerpt.Names = append(excerpt.Names, printable(p.trace.Names[index], gpuprofile.MaxFrameName))
		excerpt.Categories = append(excerpt.Categories, category)
		names[index] = len(excerpt.Names) - 1
		return names[index]
	}
	type laneKey struct {
		host           bool
		device, thread int32
		stream         int64
	}
	lanes := map[laneKey]int{}
	var keys []laneKey
	type pending struct {
		lane laneKey
		name int
		from int64
		to   int64
	}
	var events []pending
	add := func(lane laneKey, nameIndex int, from, to int64) {
		if len(events) >= gpuprofile.MaxExcerptEvents {
			excerpt.Truncated = true
			return
		}
		if _, ok := lanes[lane]; !ok {
			lanes[lane] = -1
			keys = append(keys, lane)
		}
		events = append(events, pending{lane, nameIndex, max(from, start), min(to, end)})
	}
	first := sort.Search(len(p.trace.Kernels), func(i int) bool { return p.trace.Kernels[i].Start >= start })
	for index := first; index < len(p.trace.Kernels) && p.trace.Kernels[index].Start < end; index++ {
		kernel := p.trace.Kernels[index]
		add(laneKey{device: kernel.Device, stream: kernel.Stream}, name(kernel.Name, p.category(kernel)), kernel.Start, kernel.End)
	}
	firstLaunch := sort.Search(len(p.trace.Launches), func(i int) bool { return p.trace.Launches[i].Start >= start })
	for index := firstLaunch; index < len(p.trace.Launches) && p.trace.Launches[index].Start < end; index++ {
		if launch := p.trace.Launches[index]; p.isLaunchAPI[index] {
			add(laneKey{host: true, thread: launch.Thread}, name(launch.Name, gpuprofile.CategoryLaunch), launch.Start, launch.End)
		}
	}
	if len(events) == 0 {
		return nil
	}
	sort.Slice(keys, func(i, j int) bool {
		left, right := keys[i], keys[j]
		if left.host != right.host {
			return right.host
		}
		if left.device != right.device {
			return left.device < right.device
		}
		if left.stream != right.stream {
			return left.stream < right.stream
		}
		return left.thread < right.thread
	})
	if len(keys) > gpuprofile.MaxExcerptLanes {
		keys = keys[:gpuprofile.MaxExcerptLanes]
		excerpt.Truncated = true
	}
	for position, key := range keys {
		lanes[key] = position
		if key.host {
			excerpt.Lanes = append(excerpt.Lanes, gpuprofile.Lane{Kind: "host", Label: fmt.Sprintf("Host thread %d", key.thread+1)})
			continue
		}
		device, stream := int(key.device), key.stream
		excerpt.Lanes = append(excerpt.Lanes, gpuprofile.Lane{Kind: "gpu", Device: &device, Stream: &stream, Label: fmt.Sprintf("GPU %d · stream %d", device, stream)})
	}
	for _, event := range events {
		lane := lanes[event.lane]
		if lane < 0 {
			continue
		}
		excerpt.Events = append(excerpt.Events, [4]float64{float64(lane), float64(event.name), us(event.from - start), us(max(0, event.to-event.from))})
	}
	for _, current := range p.stepList {
		if current.end <= start || current.start >= end || len(excerpt.Steps) >= gpuprofile.MaxExcerptSteps {
			continue
		}
		from, to := max(current.start, start), min(current.end, end)
		excerpt.Steps = append(excerpt.Steps, gpuprofile.ExcerptStep{StartUS: us(from - start), DurationUS: us(to - from), Phase: current.phase})
	}
	return excerpt
}

// busiestStart is the start of the window of the given length with the most
// GPU busy time, stepping by a twentieth of the length.
func (p *prepared) busiestStart(length int64) int64 {
	best, bestBusy := p.window.start, int64(-1)
	stride := max(length/20, 1)
	for from := p.window.start; from+length <= p.window.end || from == p.window.start; from += stride {
		var busy int64
		for _, device := range p.devices {
			busy += within(p.busy[device], from, from+length)
		}
		if busy > bestBusy {
			best, bestBusy = from, busy
		}
		if from+length > p.window.end {
			break
		}
	}
	return best
}

func (p *prepared) eventsIn(from, to int64) int {
	kernels := sort.Search(len(p.trace.Kernels), func(i int) bool { return p.trace.Kernels[i].Start >= to }) -
		sort.Search(len(p.trace.Kernels), func(i int) bool { return p.trace.Kernels[i].Start >= from })
	launches := sort.Search(len(p.trace.Launches), func(i int) bool { return p.trace.Launches[i].Start >= to }) -
		sort.Search(len(p.trace.Launches), func(i int) bool { return p.trace.Launches[i].Start >= from })
	return kernels + launches
}

func minTime(left, right time.Time) time.Time {
	if right.Before(left) {
		return right
	}
	return left
}

func maxTime(left, right time.Time) time.Time {
	if right.After(left) {
		return right
	}
	return left
}
