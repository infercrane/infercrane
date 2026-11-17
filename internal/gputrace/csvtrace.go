package gputrace

import (
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
)

// CSV inputs name their columns; the parsers read columns by header name so
// added or reordered columns in newer tool versions do not break them.

type csvTable struct {
	reader  *csv.Reader
	columns map[string]int
}

func newCSVTable(reader io.Reader) (*csvTable, error) {
	table := csv.NewReader(&limitedReader{reader: reader, left: MaxTraceBytes})
	table.FieldsPerRecord = -1
	table.ReuseRecord = true
	header, err := table.Read()
	if err != nil {
		return nil, fmt.Errorf("read CSV header: %w", err)
	}
	columns := map[string]int{}
	for index, name := range header {
		columns[strings.TrimSpace(strings.TrimPrefix(name, "\ufeff"))] = index
	}
	return &csvTable{reader: table, columns: columns}, nil
}

func (t *csvTable) has(names ...string) bool {
	for _, name := range names {
		if _, ok := t.columns[name]; !ok {
			return false
		}
	}
	return true
}

func (t *csvTable) field(record []string, names ...string) (string, bool) {
	for _, name := range names {
		if index, ok := t.columns[name]; ok && index < len(record) {
			return strings.TrimSpace(record[index]), true
		}
	}
	return "", false
}

var digits = regexp.MustCompile(`-?\d+`)

// integer reads the first integer in a field, so "Agent 4" and "4" both read
// as 4.
func integer(value string) (int64, bool) {
	match := digits.FindString(value)
	if match == "" {
		return 0, false
	}
	parsed, err := strconv.ParseInt(match, 10, 64)
	return parsed, err == nil
}

// RocprofFiles are the rocprofv3 CSV outputs of one run. Kernels is required.
type RocprofFiles struct {
	Kernels, HIPAPI, MemoryCopies io.Reader
}

// ParseRocprofV3 reads rocprofv3 CSV output (--kernel-trace, optionally
// --hip-runtime-trace and --memory-copy-trace, or `rocpd convert -f csv`).
// Timestamps are nanoseconds; agents become devices in order of appearance.
func ParseRocprofV3(files RocprofFiles) (*Trace, error) {
	if files.Kernels == nil {
		return nil, errors.New("rocprofv3 analysis needs kernel_trace.csv")
	}
	trace := NewTrace(FormatRocprofV3)
	agents := map[int64]int32{}
	device := func(agent int64) int32 {
		if index, ok := agents[agent]; ok {
			return index
		}
		index := int32(len(agents))
		agents[agent] = index
		return index
	}
	table, err := newCSVTable(files.Kernels)
	if err != nil {
		return nil, err
	}
	if !table.has("Kernel_Name", "Start_Timestamp", "End_Timestamp") {
		return nil, errors.New("kernel_trace.csv needs Kernel_Name, Start_Timestamp, and End_Timestamp columns")
	}
	for {
		record, readErr := table.reader.Read()
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return nil, fmt.Errorf("read kernel_trace.csv: %w", readErr)
		}
		name, _ := table.field(record, "Kernel_Name")
		startText, _ := table.field(record, "Start_Timestamp")
		endText, _ := table.field(record, "End_Timestamp")
		start, okStart := integer(startText)
		end, okEnd := integer(endText)
		if name == "" || !okStart || !okEnd || end <= start {
			continue
		}
		agentText, _ := table.field(record, "Agent_Id")
		agent, _ := integer(agentText)
		streamText, hasStream := table.field(record, "Stream_Id", "Queue_Id")
		stream, _ := integer(streamText)
		if !hasStream {
			stream = 0
		}
		correlationText, _ := table.field(record, "Correlation_Id")
		correlation, _ := integer(correlationText)
		if !trace.addKernel(Kernel{Name: trace.Intern(name), Device: device(agent), Stream: stream, Start: start, End: end, Correlation: correlation}) {
			break
		}
	}
	if files.MemoryCopies != nil {
		if err = readRocprofCopies(trace, files.MemoryCopies, device); err != nil {
			return nil, err
		}
	}
	threads := map[int64]int32{}
	if files.HIPAPI != nil {
		if err = readRocprofAPI(trace, files.HIPAPI, threads); err != nil {
			return nil, err
		}
	}
	trace.Threads = len(threads)
	trace.finish()
	if len(trace.Kernels) == 0 {
		return nil, ErrNoKernels
	}
	return trace, nil
}

func readRocprofCopies(trace *Trace, reader io.Reader, device func(int64) int32) error {
	table, err := newCSVTable(reader)
	if err != nil {
		return err
	}
	if !table.has("Start_Timestamp", "End_Timestamp") {
		return errors.New("memory_copy_trace.csv needs Start_Timestamp and End_Timestamp columns")
	}
	for {
		record, readErr := table.reader.Read()
		if errors.Is(readErr, io.EOF) {
			return nil
		}
		if readErr != nil {
			return fmt.Errorf("read memory_copy_trace.csv: %w", readErr)
		}
		startText, _ := table.field(record, "Start_Timestamp")
		endText, _ := table.field(record, "End_Timestamp")
		start, okStart := integer(startText)
		end, okEnd := integer(endText)
		agentText, _ := table.field(record, "Destination_Agent_Id", "Agent_Id")
		agent, okAgent := integer(agentText)
		if !okStart || !okEnd || !okAgent || end <= start {
			continue
		}
		direction, _ := table.field(record, "Direction", "Kind")
		correlationText, _ := table.field(record, "Correlation_Id")
		correlation, _ := integer(correlationText)
		name := "memcpy " + printable(direction, 64)
		if !trace.addKernel(Kernel{Name: trace.Intern(strings.TrimSpace(name)), Device: device(agent), Stream: -1, Start: start, End: end, Correlation: correlation, Memory: true}) {
			return nil
		}
	}
}

func readRocprofAPI(trace *Trace, reader io.Reader, threads map[int64]int32) error {
	table, err := newCSVTable(reader)
	if err != nil {
		return err
	}
	if !table.has("Function", "Start_Timestamp", "End_Timestamp", "Correlation_Id") {
		return errors.New("hip_api_trace.csv needs Function, Start_Timestamp, End_Timestamp, and Correlation_Id columns")
	}
	for {
		record, readErr := table.reader.Read()
		if errors.Is(readErr, io.EOF) {
			return nil
		}
		if readErr != nil {
			return fmt.Errorf("read hip_api_trace.csv: %w", readErr)
		}
		name, _ := table.field(record, "Function")
		startText, _ := table.field(record, "Start_Timestamp")
		endText, _ := table.field(record, "End_Timestamp")
		correlationText, _ := table.field(record, "Correlation_Id")
		threadText, _ := table.field(record, "Thread_Id")
		start, okStart := integer(startText)
		end, okEnd := integer(endText)
		correlation, okCorrelation := integer(correlationText)
		if name == "" || !okStart || !okEnd || !okCorrelation || correlation == 0 || end < start {
			continue
		}
		threadID, _ := integer(threadText)
		thread, ok := threads[threadID]
		if !ok {
			thread = int32(len(threads))
			threads[threadID] = thread
		}
		if !trace.addLaunch(Launch{Name: trace.Intern(name), Thread: thread, Start: start, End: end, Correlation: correlation}) {
			return nil
		}
	}
}

var (
	nsysDevice     = regexp.MustCompile(`^(.*?)\s*\((\d+)\)\s*$`)
	nsysAPIVersion = regexp.MustCompile(`_v\d+$`)
)

// ParseNsysCSV reads Nsight Systems CSV exports from
// `nsys stats --report cuda_gpu_trace,cuda_api_trace --format csv`. The API
// trace is optional; without it, host launch statistics stay unknown.
func ParseNsysCSV(gpuTrace, apiTrace io.Reader) (*Trace, error) {
	if gpuTrace == nil {
		return nil, errors.New("Nsight Systems analysis needs the cuda_gpu_trace CSV")
	}
	trace := NewTrace(FormatNsys)
	table, err := newCSVTable(gpuTrace)
	if err != nil {
		return nil, err
	}
	if !table.has("Start (ns)", "Duration (ns)", "Name") {
		return nil, errors.New("the cuda_gpu_trace CSV needs Start (ns), Duration (ns), and Name columns")
	}
	for {
		record, readErr := table.reader.Read()
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return nil, fmt.Errorf("read cuda_gpu_trace CSV: %w", readErr)
		}
		name, _ := table.field(record, "Name")
		startText, _ := table.field(record, "Start (ns)")
		durationText, _ := table.field(record, "Duration (ns)")
		start, okStart := integer(startText)
		duration, okDuration := integer(durationText)
		if name == "" || !okStart || !okDuration || duration <= 0 {
			continue
		}
		deviceText, _ := table.field(record, "Device")
		var device int64
		if match := nsysDevice.FindStringSubmatch(deviceText); match != nil {
			device, _ = strconv.ParseInt(match[2], 10, 64)
			if device >= 0 && device < 1<<20 {
				trace.addDevice(int32(device), printable(match[1], 128))
			}
		} else if parsed, ok := integer(deviceText); ok {
			device = parsed
		}
		streamText, _ := table.field(record, "Strm")
		stream, _ := integer(streamText)
		correlationText, _ := table.field(record, "CorrId", "CorrID")
		correlation, _ := integer(correlationText)
		memory := strings.HasPrefix(name, "[CUDA memcpy") || strings.HasPrefix(name, "[CUDA memset")
		if device < 0 || device >= 1<<20 {
			continue
		}
		if !trace.addKernel(Kernel{Name: trace.Intern(name), Device: int32(device), Stream: stream, Start: start, End: start + duration, Correlation: correlation, Memory: memory}) {
			break
		}
	}
	threads := map[string]int32{}
	if apiTrace != nil {
		api, apiErr := newCSVTable(apiTrace)
		if apiErr != nil {
			return nil, apiErr
		}
		if !api.has("Start (ns)", "Duration (ns)", "Name") || !api.has("CorrID") && !api.has("CorrId") {
			return nil, errors.New("the cuda_api_trace CSV needs Start (ns), Duration (ns), Name, and CorrID columns")
		}
		for {
			record, readErr := api.reader.Read()
			if errors.Is(readErr, io.EOF) {
				break
			}
			if readErr != nil {
				return nil, fmt.Errorf("read cuda_api_trace CSV: %w", readErr)
			}
			name, _ := api.field(record, "Name")
			startText, _ := api.field(record, "Start (ns)")
			durationText, _ := api.field(record, "Duration (ns)")
			correlationText, _ := api.field(record, "CorrID", "CorrId")
			start, okStart := integer(startText)
			duration, okDuration := integer(durationText)
			correlation, okCorrelation := integer(correlationText)
			if name == "" || !okStart || !okDuration || !okCorrelation || correlation == 0 || duration < 0 {
				continue
			}
			pid, _ := api.field(record, "Pid")
			tid, _ := api.field(record, "Tid")
			key := pid + "/" + tid
			thread, ok := threads[key]
			if !ok {
				thread = int32(len(threads))
				threads[key] = thread
			}
			// Nsight Systems appends a version suffix such as _v7000 to API names.
			name = nsysAPIVersion.ReplaceAllString(name, "")
			if !trace.addLaunch(Launch{Name: trace.Intern(name), Thread: thread, Start: start, End: start + duration, Correlation: correlation}) {
				break
			}
		}
	}
	trace.Threads = len(threads)
	trace.finish()
	if len(trace.Kernels) == 0 {
		return nil, ErrNoKernels
	}
	return trace, nil
}
