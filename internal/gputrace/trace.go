// Package gputrace reads GPU profiler traces of a serving engine (PyTorch
// profiler / Kineto JSON from vLLM and SGLang on NVIDIA and AMD, rocprofv3
// CSV, and Nsight Systems CSV exports) and turns one into a bounded,
// content-free analysis: busy and idle time, idle causes, launch overhead,
// kernel categories, communication overlap, prefill and decode, engine
// steps, and a roofline position. The definitions follow Holistic Trace
// Analysis (MIT); the logic is re-implemented here so the CLI needs no
// Python. Only kernel, operator, and API names and their times are read;
// arguments other than device, stream, correlation, and operator shapes are
// never kept, so no request content can enter an analysis.
package gputrace

import (
	"errors"
	"sort"
)

// Bounds on one trace. A trace with more events is analyzed up to the limit
// and marked truncated.
const (
	MaxKernels  = 4_000_000
	MaxLaunches = 4_000_000
	MaxOps      = 4_000_000
	MaxNames    = 200_000
	// MaxTraceBytes bounds the decompressed size of one trace file.
	MaxTraceBytes = int64(8) << 30
	maxNameBytes  = 512
)

// Trace formats.
const (
	FormatKineto    = "kineto"
	FormatRocprofV3 = "rocprofv3"
	FormatNsys      = "nsys"
)

// Limits the parsers enforce; tests lower them.
var (
	kernelLimit = MaxKernels
	launchLimit = MaxLaunches
	opLimit     = MaxOps
)

// ErrNoKernels reports a trace without any GPU kernel to analyze.
var ErrNoKernels = errors.New("the trace has no GPU kernels")

// Kernel is one GPU activity: a kernel, or a memory copy or set when Memory
// is true. Times are nanoseconds from the trace origin.
type Kernel struct {
	Name        int32
	Device      int32
	Stream      int64
	Start, End  int64
	Correlation int64
	Memory      bool
}

// Launch is one host runtime or driver call with a correlation to GPU work.
type Launch struct {
	Name        int32
	Thread      int32
	Start, End  int64
	Correlation int64
}

// Op is one host range: a PyTorch operator or a record_function annotation.
type Op struct {
	Name       int32
	Thread     int32
	Start, End int64
	// FLOPs and Bytes are set for matrix multiplies, and for operators of a
	// measured class, with recorded shapes. ShapeLabel is set for matrix
	// multiplies only, so the roofline reads no other operator.
	FLOPs, Bytes float64
	ShapeLabel   int32
	// Operator is the 1-based index of the operator's class in
	// gpuprofile.Operators, or 0.
	Operator int8
}

// Device is one GPU the trace names.
type Device struct {
	ID   int32
	Name string
}

// Trace is one capture, possibly merged from one file per rank.
type Trace struct {
	Format   string
	Names    []string
	Kernels  []Kernel
	Launches []Launch
	Ops      []Op
	Devices  []Device
	// OriginNS is the Unix time in nanoseconds of time zero when OriginKnown.
	OriginNS       int64
	OriginKnown    bool
	Threads        int
	CPUOpsRecorded bool
	ShapesRecorded bool
	Truncated      bool

	names  map[string]int32
	merges int64
}

// NewTrace starts an empty trace of one format.
func NewTrace(format string) *Trace {
	return &Trace{Format: format, names: map[string]int32{}}
}

// Intern returns the index of name, adding it when new. Names longer than a
// frame name allows are cut, and names past MaxNames share one entry.
func (t *Trace) Intern(name string) int32 {
	if len(name) > maxNameBytes {
		name = name[:maxNameBytes]
	}
	if index, ok := t.names[name]; ok {
		return index
	}
	if len(t.Names) >= MaxNames {
		t.Truncated = true
		return t.Intern("(more names than one analysis reads)")
	}
	index := int32(len(t.Names))
	t.Names = append(t.Names, name)
	t.names[name] = index
	return index
}

func (t *Trace) addKernel(kernel Kernel) bool {
	if len(t.Kernels) >= kernelLimit {
		t.Truncated = true
		return false
	}
	t.Kernels = append(t.Kernels, kernel)
	return true
}

func (t *Trace) addLaunch(launch Launch) bool {
	if len(t.Launches) >= launchLimit {
		t.Truncated = true
		return false
	}
	t.Launches = append(t.Launches, launch)
	return true
}

func (t *Trace) addOp(op Op) bool {
	if len(t.Ops) >= opLimit {
		t.Truncated = true
		return false
	}
	t.Ops = append(t.Ops, op)
	return true
}

func (t *Trace) addDevice(id int32, name string) {
	for index, device := range t.Devices {
		if device.ID == id {
			if device.Name == "" {
				t.Devices[index].Name = name
			}
			return
		}
	}
	t.Devices = append(t.Devices, Device{ID: id, Name: name})
}

// finish sorts events by time, records every device a kernel ran on, and
// shifts times so the first event starts at zero.
func (t *Trace) finish() {
	sort.SliceStable(t.Kernels, func(i, j int) bool { return t.Kernels[i].Start < t.Kernels[j].Start })
	sort.SliceStable(t.Launches, func(i, j int) bool { return t.Launches[i].Start < t.Launches[j].Start })
	sort.SliceStable(t.Ops, func(i, j int) bool {
		if t.Ops[i].Start != t.Ops[j].Start {
			return t.Ops[i].Start < t.Ops[j].Start
		}
		return t.Ops[i].End > t.Ops[j].End
	})
	for _, kernel := range t.Kernels {
		t.addDevice(kernel.Device, "")
	}
	sort.Slice(t.Devices, func(i, j int) bool { return t.Devices[i].ID < t.Devices[j].ID })
	if len(t.Kernels) == 0 {
		return
	}
	origin := t.Kernels[0].Start
	for _, launch := range t.Launches {
		origin = min(origin, launch.Start)
	}
	for index := range t.Kernels {
		t.Kernels[index].Start -= origin
		t.Kernels[index].End -= origin
	}
	for index := range t.Launches {
		t.Launches[index].Start -= origin
		t.Launches[index].End -= origin
	}
	for index := range t.Ops {
		t.Ops[index].Start -= origin
		t.Ops[index].End -= origin
	}
	if t.OriginKnown {
		t.OriginNS += origin
	}
}

// Accelerator is the name of the first device the trace describes, or "".
func (t *Trace) Accelerator() string {
	for _, device := range t.Devices {
		if device.Name != "" {
			return device.Name
		}
	}
	return ""
}

// Merge adds other's events to t, keeping devices, threads, names, and
// correlations apart. Files with absolute origins are aligned on them;
// otherwise both are assumed to share one clock.
func (t *Trace) Merge(other *Trace) {
	shift := int64(0)
	if t.OriginKnown && other.OriginKnown {
		shift = other.OriginNS - t.OriginNS
	} else {
		t.OriginKnown, t.OriginNS = false, 0
	}
	remap := make([]int32, len(other.Names))
	for index, name := range other.Names {
		remap[index] = t.Intern(name)
	}
	threadBase := int32(t.Threads)
	// Correlation IDs are unique within one process only, so each merged file
	// gets its own range. Kineto and CUPTI IDs stay far below 2^40.
	t.merges++
	correlationBase := t.merges << 40
	for _, kernel := range other.Kernels {
		kernel.Name = remap[kernel.Name]
		kernel.Start += shift
		kernel.End += shift
		if kernel.Correlation != 0 {
			kernel.Correlation += correlationBase
		}
		if !t.addKernel(kernel) {
			break
		}
	}
	for _, launch := range other.Launches {
		launch.Name = remap[launch.Name]
		launch.Thread += threadBase
		launch.Start += shift
		launch.End += shift
		if launch.Correlation != 0 {
			launch.Correlation += correlationBase
		}
		if !t.addLaunch(launch) {
			break
		}
	}
	for _, op := range other.Ops {
		op.Name = remap[op.Name]
		if op.ShapeLabel >= 0 {
			op.ShapeLabel = remap[op.ShapeLabel]
		}
		op.Thread += threadBase
		op.Start += shift
		op.End += shift
		if !t.addOp(op) {
			break
		}
	}
	for _, device := range other.Devices {
		t.addDevice(device.ID, device.Name)
	}
	t.Threads += other.Threads
	t.CPUOpsRecorded = t.CPUOpsRecorded || other.CPUOpsRecorded
	t.ShapesRecorded = t.ShapesRecorded || other.ShapesRecorded
	t.Truncated = t.Truncated || other.Truncated
	t.finish()
}
