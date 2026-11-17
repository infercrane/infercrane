package gputrace

import (
	"sort"

	"github.com/infercrane/infercrane/internal/gpuprofile"
)

// maxOpChain bounds the host operator frames kept above one launch.
const maxOpChain = 8

// prepared is a trace with the links an analysis needs: each GPU activity's
// category and launch, and each launch's enclosing host operators.
type prepared struct {
	trace      *Trace
	categories []string // by name index
	window     span
	devices    []int32
	byDevice   map[int32][]int // kernel indexes, sorted by start
	busy       map[int32][]span
	busyFirst  map[int32][]int // kernel index that opened each busy span
	// launchOf maps a correlation to its launch call.
	launchOf map[int64]int
	// launchKernels maps a launch index to the kernels it enqueued.
	launchKernels map[int][]int
	// chains holds each launch's enclosing operators, outermost first.
	chains      map[int][]int
	isLaunchAPI []bool
	// stepList holds the engine steps once steps has run.
	stepList []step
}

func prepare(trace *Trace) *prepared {
	p := &prepared{
		trace: trace, categories: make([]string, len(trace.Names)), byDevice: map[int32][]int{}, busy: map[int32][]span{},
		busyFirst: map[int32][]int{}, launchOf: map[int64]int{}, launchKernels: map[int][]int{}, chains: map[int][]int{},
	}
	for index, name := range trace.Names {
		p.categories[index] = Category(name)
	}
	p.window = span{trace.Kernels[0].Start, trace.Kernels[0].End}
	for index, kernel := range trace.Kernels {
		p.window.start, p.window.end = min(p.window.start, kernel.Start), max(p.window.end, kernel.End)
		p.byDevice[kernel.Device] = append(p.byDevice[kernel.Device], index)
	}
	for device, indexes := range p.byDevice {
		p.devices = append(p.devices, device)
		spans := make([]span, len(indexes))
		for position, index := range indexes {
			spans[position] = span{trace.Kernels[index].Start, trace.Kernels[index].End}
		}
		merged, first := union(spans)
		p.busy[device] = merged
		opened := make([]int, len(first))
		for position, input := range first {
			opened[position] = indexes[input]
		}
		p.busyFirst[device] = opened
	}
	sort.Slice(p.devices, func(i, j int) bool { return p.devices[i] < p.devices[j] })
	p.isLaunchAPI = make([]bool, len(trace.Launches))
	for index, launch := range trace.Launches {
		p.isLaunchAPI[index] = IsLaunchAPI(trace.Names[launch.Name])
		if existing, ok := p.launchOf[launch.Correlation]; !ok || p.isLaunchAPI[index] && !p.isLaunchAPI[existing] {
			p.launchOf[launch.Correlation] = index
		}
	}
	for index, kernel := range trace.Kernels {
		if kernel.Correlation == 0 {
			continue
		}
		if launch, ok := p.launchOf[kernel.Correlation]; ok {
			p.launchKernels[launch] = append(p.launchKernels[launch], index)
		}
	}
	p.linkOperators()
	return p
}

func (p *prepared) category(kernel Kernel) string {
	if kernel.Memory {
		return gpuprofile.CategoryMemory
	}
	return p.categories[kernel.Name]
}

func (p *prepared) launchOfKernel(kernel Kernel) (Launch, int, bool) {
	if kernel.Correlation == 0 {
		return Launch{}, -1, false
	}
	index, ok := p.launchOf[kernel.Correlation]
	if !ok {
		return Launch{}, -1, false
	}
	return p.trace.Launches[index], index, true
}

// linkOperators finds, for every launch, the host operators open on its
// thread when it started, outermost first, by a sweep over each thread.
func (p *prepared) linkOperators() {
	if len(p.trace.Ops) == 0 || len(p.trace.Launches) == 0 {
		return
	}
	opsByThread := map[int32][]int{}
	for index, op := range p.trace.Ops {
		opsByThread[op.Thread] = append(opsByThread[op.Thread], index)
	}
	launchesByThread := map[int32][]int{}
	for index, launch := range p.trace.Launches {
		if len(p.launchKernels[index]) > 0 {
			launchesByThread[launch.Thread] = append(launchesByThread[launch.Thread], index)
		}
	}
	for thread, launches := range launchesByThread {
		ops := opsByThread[thread]
		var stack []int
		next := 0
		for _, launchIndex := range launches {
			at := p.trace.Launches[launchIndex].Start
			for next < len(ops) && p.trace.Ops[ops[next]].Start <= at {
				candidate := p.trace.Ops[ops[next]]
				for len(stack) > 0 && p.trace.Ops[stack[len(stack)-1]].End <= candidate.Start {
					stack = stack[:len(stack)-1]
				}
				stack = append(stack, ops[next])
				next++
			}
			for len(stack) > 0 && p.trace.Ops[stack[len(stack)-1]].End < at {
				stack = stack[:len(stack)-1]
			}
			// Ops that closed before this launch but under a still-open parent
			// were popped above only when on top; keep those that contain it.
			chain := make([]int, 0, min(len(stack), maxOpChain))
			for _, op := range stack {
				if p.trace.Ops[op].End >= at {
					chain = append(chain, op)
				}
			}
			if len(chain) > maxOpChain {
				// Keep the outermost three and the innermost frames.
				chain = append(chain[:3:3], chain[len(chain)-(maxOpChain-3):]...)
			}
			if len(chain) > 0 {
				p.chains[launchIndex] = chain
			}
		}
	}
}

// innermostOp is the operator that issued a launch, or -1.
func (p *prepared) innermostOp(launch int) int {
	chain := p.chains[launch]
	if len(chain) == 0 {
		return -1
	}
	return chain[len(chain)-1]
}
