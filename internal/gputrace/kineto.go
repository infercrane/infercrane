package gputrace

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
)

// kinetoEvent keeps only the fields an analysis reads. Every other argument,
// including concrete inputs and Python source, is skipped by the decoder.
type kinetoEvent struct {
	Phase    string          `json:"ph"`
	Category string          `json:"cat"`
	Name     string          `json:"name"`
	PID      json.RawMessage `json:"pid"`
	TID      json.RawMessage `json:"tid"`
	TS       float64         `json:"ts"`
	Duration float64         `json:"dur"`
	Args     kinetoArgs      `json:"args"`
}

type kinetoArgs struct {
	Device      *json.Number    `json:"device"`
	Stream      *json.Number    `json:"stream"`
	Correlation *json.Number    `json:"correlation"`
	InputDims   json.RawMessage `json:"Input Dims"`
	InputType   json.RawMessage `json:"Input type"`
}

type kinetoDevice struct {
	ID   json.Number `json:"id"`
	Name string      `json:"name"`
}

type limitedReader struct {
	reader io.Reader
	left   int64
}

var errTraceTooLarge = fmt.Errorf("the trace is larger than %d GiB decompressed", MaxTraceBytes>>30)

func (l *limitedReader) Read(buffer []byte) (int, error) {
	if l.left <= 0 {
		return 0, errTraceTooLarge
	}
	if int64(len(buffer)) > l.left {
		buffer = buffer[:l.left]
	}
	count, err := l.reader.Read(buffer)
	l.left -= int64(count)
	return count, err
}

// openTrace returns a reader for a JSON trace, decompressing gzip by its
// magic bytes rather than its file name.
func openTrace(reader io.Reader) (io.Reader, func() error, error) {
	buffered := bufio.NewReaderSize(reader, 1<<20)
	magic, err := buffered.Peek(2)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, nil, err
	}
	if len(magic) == 2 && magic[0] == 0x1f && magic[1] == 0x8b {
		compressed, gzipErr := gzip.NewReader(buffered)
		if gzipErr != nil {
			return nil, nil, gzipErr
		}
		return &limitedReader{reader: bufio.NewReaderSize(compressed, 1<<20), left: MaxTraceBytes}, compressed.Close, nil
	}
	return &limitedReader{reader: buffered, left: MaxTraceBytes}, func() error { return nil }, nil
}

// ParseKineto reads one PyTorch profiler (Kineto) Chrome-trace JSON file, as
// vLLM and SGLang write it from /start_profile and /stop_profile, gzip or
// not. It streams the event list, so a trace larger than memory is read up to
// the event limits.
func ParseKineto(reader io.Reader) (*Trace, error) {
	source, closeSource, err := openTrace(reader)
	if err != nil {
		return nil, fmt.Errorf("open trace: %w", err)
	}
	defer closeSource()
	decoder := json.NewDecoder(source)
	decoder.UseNumber()
	if token, tokenErr := decoder.Token(); tokenErr != nil || token != json.Delim('{') {
		return nil, errors.New("a Kineto trace is a JSON object with traceEvents")
	}
	trace := NewTrace(FormatKineto)
	state := kinetoState{trace: trace, threads: map[string]int32{}}
	sawEvents := false
	for decoder.More() {
		token, tokenErr := decoder.Token()
		if tokenErr != nil {
			return nil, fmt.Errorf("read trace: %w", tokenErr)
		}
		key, _ := token.(string)
		switch key {
		case "traceEvents":
			sawEvents = true
			if err = state.readEvents(decoder); err != nil {
				return nil, err
			}
			if trace.Truncated {
				// Stop reading: the rest of the file is beyond the limit.
				state.origin()
				trace.finish()
				return finishKineto(trace, &state)
			}
		case "deviceProperties":
			var devices []kinetoDevice
			if err = decoder.Decode(&devices); err != nil {
				return nil, fmt.Errorf("read deviceProperties: %w", err)
			}
			for _, device := range devices {
				if id, idErr := device.ID.Int64(); idErr == nil && id >= 0 && id < 1<<20 {
					state.deviceNames = append(state.deviceNames, Device{ID: int32(id), Name: printable(device.Name, 128)})
				}
			}
		case "baseTimeNanoseconds":
			var base json.Number
			if err = decoder.Decode(&base); err != nil {
				return nil, fmt.Errorf("read baseTimeNanoseconds: %w", err)
			}
			if value, parseErr := base.Int64(); parseErr == nil && value > 0 {
				state.baseNS = value
			}
		default:
			var skip json.RawMessage
			if err = decoder.Decode(&skip); err != nil {
				return nil, fmt.Errorf("read trace: %w", err)
			}
		}
	}
	if !sawEvents {
		return nil, errors.New("the trace has no traceEvents")
	}
	state.origin()
	trace.finish()
	return finishKineto(trace, &state)
}

func finishKineto(trace *Trace, state *kinetoState) (*Trace, error) {
	for _, device := range state.deviceNames {
		for _, kernel := range trace.Kernels {
			if kernel.Device == device.ID {
				trace.addDevice(device.ID, device.Name)
				break
			}
		}
	}
	trace.Threads = len(state.threads)
	if len(trace.Kernels) == 0 {
		return nil, ErrNoKernels
	}
	return trace, nil
}

type kinetoState struct {
	trace       *Trace
	threads     map[string]int32
	deviceNames []Device
	baseNS      int64
	minTS       float64
	haveTS      bool
}

func (s *kinetoState) readEvents(decoder *json.Decoder) error {
	if token, err := decoder.Token(); err != nil || token != json.Delim('[') {
		return errors.New("traceEvents must be a list")
	}
	for decoder.More() {
		var event kinetoEvent
		if err := decoder.Decode(&event); err != nil {
			return fmt.Errorf("read trace event: %w", err)
		}
		if !s.add(event) {
			return nil
		}
	}
	if _, err := decoder.Token(); err != nil {
		return fmt.Errorf("read trace: %w", err)
	}
	return nil
}

// origin sets the Unix time of the trace's clock: baseTimeNanoseconds plus
// relative timestamps, or timestamps that are Unix microseconds already.
func (s *kinetoState) origin() {
	switch {
	case s.baseNS > 0 && s.haveTS:
		s.trace.OriginNS, s.trace.OriginKnown = s.baseNS, true
	case s.haveTS && s.minTS > 1e15:
		s.trace.OriginNS, s.trace.OriginKnown = 0, true
	}
}

// add records one event and reports whether reading should continue.
func (s *kinetoState) add(event kinetoEvent) bool {
	if event.Phase != "X" || event.Name == "" || math.IsNaN(event.TS) || math.IsInf(event.TS, 0) || event.Duration < 0 || math.IsNaN(event.Duration) {
		return true
	}
	start := int64(math.Round(event.TS * 1000))
	end := start + int64(math.Round(event.Duration*1000))
	if !s.haveTS || event.TS < s.minTS {
		s.minTS, s.haveTS = event.TS, true
	}
	trace := s.trace
	category := strings.ToLower(event.Category)
	switch category {
	case "kernel", "gpu_memcpy", "gpu_memset":
		device, ok := number(event.Args.Device)
		if !ok {
			device = rawNumber(event.PID)
		}
		stream, ok := number(event.Args.Stream)
		if !ok {
			stream = rawNumber(event.TID)
		}
		correlation, _ := number(event.Args.Correlation)
		if device < 0 || device >= 1<<20 || end <= start {
			return true
		}
		return trace.addKernel(Kernel{Name: trace.Intern(event.Name), Device: int32(device), Stream: stream, Start: start, End: end, Correlation: correlation, Memory: category != "kernel"})
	case "cuda_runtime", "cuda_driver", "hip_runtime", "hip_driver":
		correlation, ok := number(event.Args.Correlation)
		if !ok || correlation == 0 {
			return true
		}
		return trace.addLaunch(Launch{Name: trace.Intern(event.Name), Thread: s.thread(event), Start: start, End: end, Correlation: correlation})
	case "cpu_op", "user_annotation":
		// A record_function annotation is free text chosen by engine code, so
		// only the step markers this package knows are read. Operator names
		// (cpu_op) are registered identifiers.
		if category == "user_annotation" && stepMarker(event.Name) == "" {
			return true
		}
		trace.CPUOpsRecorded = true
		op := Op{Name: trace.Intern(event.Name), Thread: s.thread(event), Start: start, End: end, ShapeLabel: -1, Operator: operatorIndex(hostOperators[event.Name])}
		if len(event.Args.InputDims) > 0 {
			trace.ShapesRecorded = true
			if flops, bytes, label, ok := matmulShape(event.Name, event.Args.InputDims, event.Args.InputType); ok {
				op.FLOPs, op.Bytes, op.ShapeLabel = flops, bytes, trace.Intern(label)
			} else if op.Operator > 0 {
				var dims [][]int64
				var types []string
				if json.Unmarshal(event.Args.InputDims, &dims) == nil {
					_ = json.Unmarshal(event.Args.InputType, &types)
					if flops, bytes, ok := operatorShape(event.Name, dims, types); ok {
						op.FLOPs, op.Bytes = flops, bytes
					}
				}
			}
		}
		return trace.addOp(op)
	}
	return true
}

func (s *kinetoState) thread(event kinetoEvent) int32 {
	key := string(bytes.TrimSpace(event.PID)) + "/" + string(bytes.TrimSpace(event.TID))
	if index, ok := s.threads[key]; ok {
		return index
	}
	index := int32(len(s.threads))
	s.threads[key] = index
	return index
}

func number(value *json.Number) (int64, bool) {
	if value == nil {
		return 0, false
	}
	if parsed, err := value.Int64(); err == nil {
		return parsed, true
	}
	if parsed, err := value.Float64(); err == nil && parsed == math.Trunc(parsed) && math.Abs(parsed) < 1<<53 {
		return int64(parsed), true
	}
	return 0, false
}

func rawNumber(raw json.RawMessage) int64 {
	text := strings.Trim(string(bytes.TrimSpace(raw)), `"`)
	if parsed, err := strconv.ParseInt(text, 10, 64); err == nil {
		return parsed
	}
	return -1
}

// printable keeps a device or kernel name to bounded, printable text.
func printable(value string, limit int) string {
	var builder strings.Builder
	for _, character := range value {
		if character < 0x20 || character == 0x7f {
			continue
		}
		if builder.Len()+len(string(character)) > limit {
			break
		}
		builder.WriteRune(character)
	}
	return strings.TrimSpace(builder.String())
}

// matmulShape computes FLOPs and the minimum bytes moved by a matrix-multiply
// operator from its recorded input shapes. Bytes assume each operand is read
// once and the result written once, so intensity is an upper bound.
func matmulShape(name string, dimsJSON, typesJSON json.RawMessage) (float64, float64, string, bool) {
	var dims [][]int64
	if json.Unmarshal(dimsJSON, &dims) != nil {
		return 0, 0, "", false
	}
	var types []string
	_ = json.Unmarshal(typesJSON, &types)
	elementBytes := func(index int) float64 {
		if index < len(types) {
			return dtypeBytes(types[index])
		}
		return 0
	}
	var batch, m, k, n int64
	var left, right int
	switch name {
	case "aten::mm":
		left, right = 0, 1
	case "aten::addmm":
		left, right = 1, 2
	case "aten::bmm":
		left, right = 0, 1
	case "aten::baddbmm":
		left, right = 1, 2
	default:
		return 0, 0, "", false
	}
	if right >= len(dims) {
		return 0, 0, "", false
	}
	a, b := dims[left], dims[right]
	switch {
	case len(a) == 2 && len(b) == 2 && a[1] == b[0]:
		batch, m, k, n = 1, a[0], a[1], b[1]
	case len(a) == 3 && len(b) == 3 && a[0] == b[0] && a[2] == b[1]:
		batch, m, k, n = a[0], a[1], a[2], b[2]
	default:
		return 0, 0, "", false
	}
	size := elementBytes(left)
	if batch < 1 || m < 1 || k < 1 || n < 1 || size == 0 {
		return 0, 0, "", false
	}
	flops := 2 * float64(batch) * float64(m) * float64(n) * float64(k)
	bytes := float64(batch) * (float64(m*k)*size + float64(k*n)*math.Max(size, elementBytes(right)) + float64(m*n)*size)
	shape := fmt.Sprintf("%d×%d×%d", m, k, n)
	if batch > 1 {
		shape = fmt.Sprintf("%d×", batch) + shape
	}
	return flops, bytes, fmt.Sprintf("%s %s %s", strings.TrimPrefix(name, "aten::"), shape, dtypeLabel(types, left)), true
}

func dtypeBytes(dtype string) float64 {
	switch strings.TrimPrefix(dtype, "c10::") {
	case "BFloat16", "Half", "short":
		return 2
	case "float", "int", "Float":
		return 4
	case "double", "long int", "long":
		return 8
	case "Float8_e4m3fn", "Float8_e5m2", "Float8_e4m3fnuz", "Float8_e5m2fnuz", "signed char", "unsigned char", "char":
		return 1
	default:
		return 0
	}
}

func dtypeLabel(types []string, index int) string {
	if index >= len(types) {
		return "unknown"
	}
	switch strings.TrimPrefix(types[index], "c10::") {
	case "BFloat16":
		return "bf16"
	case "Half":
		return "fp16"
	case "float":
		return "fp32"
	case "Float8_e4m3fn", "Float8_e4m3fnuz":
		return "fp8e4m3"
	case "Float8_e5m2", "Float8_e5m2fnuz":
		return "fp8e5m2"
	default:
		return printable(types[index], 24)
	}
}
