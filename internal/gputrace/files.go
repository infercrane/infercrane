package gputrace

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// MaxTraceFiles bounds how many files (one per rank) one capture merges.
const MaxTraceFiles = 64

// ParseFiles reads one capture from files or directories. Kineto JSON files
// (one per rank) are merged; a rocprofv3 or Nsight Systems CSV export is
// read with its sibling files. format is kineto, rocprofv3, nsys, or "" to
// detect it from the files.
func ParseFiles(paths []string, format string) (*Trace, error) {
	files, err := expand(paths)
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, errors.New("no trace files found")
	}
	if format == "" {
		if format, err = detect(files[0]); err != nil {
			return nil, err
		}
	}
	switch format {
	case FormatKineto:
		var merged *Trace
		for _, path := range files {
			if !strings.HasSuffix(path, ".json") && !strings.HasSuffix(path, ".json.gz") {
				continue
			}
			trace, parseErr := parseKinetoFile(path)
			if parseErr != nil {
				return nil, fmt.Errorf("%s: %w", filepath.Base(path), parseErr)
			}
			if merged == nil {
				merged = trace
				continue
			}
			merged.Merge(trace)
		}
		if merged == nil {
			return nil, errors.New("no Kineto trace (.json or .json.gz) among the files")
		}
		return merged, nil
	case FormatRocprofV3:
		return parseRocprofFiles(files)
	case FormatNsys:
		return parseNsysFiles(files)
	default:
		return nil, fmt.Errorf("unknown trace format %q: use kineto, rocprofv3, or nsys", format)
	}
}

func parseKinetoFile(path string) (*Trace, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return ParseKineto(file)
}

func expand(paths []string) ([]string, error) {
	var files []string
	for _, path := range paths {
		info, err := os.Stat(path)
		if err != nil {
			return nil, err
		}
		if !info.IsDir() {
			files = append(files, path)
			continue
		}
		entries, err := os.ReadDir(path)
		if err != nil {
			return nil, err
		}
		for _, entry := range entries {
			name := entry.Name()
			if !entry.IsDir() && (strings.HasSuffix(name, ".json") || strings.HasSuffix(name, ".json.gz") || strings.HasSuffix(name, ".csv")) {
				files = append(files, filepath.Join(path, name))
			}
		}
	}
	sort.Strings(files)
	if len(files) > MaxTraceFiles {
		return nil, fmt.Errorf("a capture merges at most %d files", MaxTraceFiles)
	}
	return files, nil
}

// detect names the format of a file from its name and first line.
func detect(path string) (string, error) {
	if strings.HasSuffix(path, ".json") || strings.HasSuffix(path, ".json.gz") {
		return FormatKineto, nil
	}
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	header, err := bufio.NewReader(io.LimitReader(file, 1<<16)).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	switch {
	case strings.Contains(header, "Kernel_Name") || strings.Contains(header, "Correlation_Id"):
		return FormatRocprofV3, nil
	case strings.Contains(header, "Start (ns)"):
		return FormatNsys, nil
	default:
		return "", fmt.Errorf("%s is not a Kineto JSON, rocprofv3 CSV, or Nsight Systems CSV trace", filepath.Base(path))
	}
}

func headerOf(path string) string {
	file, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer file.Close()
	header, _ := bufio.NewReader(io.LimitReader(file, 1<<16)).ReadString('\n')
	return header
}

func parseRocprofFiles(files []string) (*Trace, error) {
	var kernels, api, copies string
	for _, path := range files {
		header := headerOf(path)
		switch {
		case strings.Contains(header, "Kernel_Name"):
			kernels = path
		case strings.Contains(header, "Function") && strings.Contains(header, "Correlation_Id"):
			api = path
		case strings.Contains(header, "Destination_Agent_Id") || strings.Contains(header, "Direction"):
			copies = path
		}
	}
	if kernels == "" {
		return nil, errors.New("rocprofv3 analysis needs kernel_trace.csv (run rocprofv3 --kernel-trace --output-format csv)")
	}
	open := func(path string) (io.Reader, func(), error) {
		if path == "" {
			return nil, func() {}, nil
		}
		file, err := os.Open(path)
		if err != nil {
			return nil, func() {}, err
		}
		return file, func() { file.Close() }, nil
	}
	kernelFile, closeKernels, err := open(kernels)
	if err != nil {
		return nil, err
	}
	defer closeKernels()
	apiFile, closeAPI, err := open(api)
	if err != nil {
		return nil, err
	}
	defer closeAPI()
	copyFile, closeCopies, err := open(copies)
	if err != nil {
		return nil, err
	}
	defer closeCopies()
	return ParseRocprofV3(RocprofFiles{Kernels: kernelFile, HIPAPI: apiFile, MemoryCopies: copyFile})
}

func parseNsysFiles(files []string) (*Trace, error) {
	var gpu, api string
	for _, path := range files {
		header := headerOf(path)
		switch {
		case strings.Contains(header, "Strm") || strings.Contains(header, "GrdX"):
			gpu = path
		case strings.Contains(header, "CorrID") || strings.Contains(header, "Tid"):
			api = path
		}
	}
	if gpu == "" {
		return nil, errors.New("Nsight Systems analysis needs the cuda_gpu_trace CSV (nsys stats --report cuda_gpu_trace,cuda_api_trace --format csv)")
	}
	gpuFile, err := os.Open(gpu)
	if err != nil {
		return nil, err
	}
	defer gpuFile.Close()
	var apiReader io.Reader
	if api != "" {
		apiFile, openErr := os.Open(api)
		if openErr != nil {
			return nil, openErr
		}
		defer apiFile.Close()
		apiReader = apiFile
	}
	return ParseNsysCSV(gpuFile, apiReader)
}
