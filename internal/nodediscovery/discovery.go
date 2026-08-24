// Package nodediscovery performs bounded, read-only inspection of a host.
// It never installs software, opens a listener, or mutates accelerator state.
package nodediscovery

import (
	"bytes"
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

const (
	ContractVersion = "infercrane.node-discovery/v1"
	maxOutputBytes  = 1 << 20
	maxGPUs         = 256
)

type GPU struct {
	Index          int    `json:"index"`
	UUID           string `json:"uuid"`
	Name           string `json:"name"`
	MemoryTotalMiB int    `json:"memory_total_mib"`
	DriverVersion  string `json:"driver_version"`
}

type Report struct {
	Contract    string   `json:"contract"`
	State       string   `json:"state"`
	Source      string   `json:"source"`
	GPUs        []GPU    `json:"gpus"`
	Limitations []string `json:"limitations"`
}

type Runner interface {
	Run(context.Context, string, ...string) ([]byte, error)
}

type CommandRunner struct{}

func (CommandRunner) Run(ctx context.Context, name string, arguments ...string) ([]byte, error) {
	path, err := exec.LookPath(name)
	if err != nil {
		return nil, err
	}
	var stdout, stderr boundedBuffer
	stdout.limit, stderr.limit = maxOutputBytes, 64<<10
	command := exec.CommandContext(ctx, path, arguments...)
	command.Stdout, command.Stderr = &stdout, &stderr
	if err = command.Run(); err != nil {
		message := strings.TrimSpace(stderr.String())
		if message != "" {
			return nil, fmt.Errorf("%w: %s", err, message)
		}
		return nil, err
	}
	return stdout.Bytes(), nil
}

type boundedBuffer struct {
	bytes.Buffer
	limit int
}

func (b *boundedBuffer) Write(value []byte) (int, error) {
	if b.Buffer.Len()+len(value) > b.limit {
		return 0, errors.New("command output exceeded the bounded discovery limit")
	}
	return b.Buffer.Write(value)
}

func DiscoverLocal(ctx context.Context, runner Runner) (Report, error) {
	if runner == nil {
		runner = CommandRunner{}
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	output, err := runner.Run(ctx, "nvidia-smi", "--query-gpu=index,uuid,name,memory.total,driver_version", "--format=csv,noheader,nounits")
	if err != nil {
		state := "degraded"
		if errors.Is(err, exec.ErrNotFound) {
			state = "unavailable"
		}
		return Report{Contract: ContractVersion, State: state, Source: "nvidia-smi", GPUs: []GPU{}, Limitations: []string{"NVIDIA inventory unavailable: " + boundedMessage(err), "Discovery is read-only and does not transfer lifecycle ownership."}}, nil
	}
	gpus, err := parseNVIDIA(output)
	if err != nil {
		return Report{}, err
	}
	state := "ready"
	if len(gpus) == 0 {
		state = "unavailable"
	}
	return Report{Contract: ContractVersion, State: state, Source: "nvidia-smi", GPUs: gpus, Limitations: []string{"GPU presence does not prove model fit, runtime compatibility, network reachability, or performance.", "Discovery is read-only and does not transfer lifecycle ownership."}}, nil
}

func parseNVIDIA(output []byte) ([]GPU, error) {
	if len(output) > maxOutputBytes {
		return nil, errors.New("nvidia-smi output exceeds one MiB")
	}
	reader := csv.NewReader(bytes.NewReader(output))
	reader.TrimLeadingSpace = true
	result := []GPU{}
	for {
		row, err := reader.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("parse nvidia-smi CSV: %w", err)
		}
		if len(row) == 1 && strings.TrimSpace(row[0]) == "" {
			continue
		}
		if len(row) != 5 || len(result) >= maxGPUs {
			return nil, errors.New("nvidia-smi returned an unsupported or excessive GPU inventory")
		}
		index, indexErr := strconv.Atoi(strings.TrimSpace(row[0]))
		memory, memoryErr := strconv.Atoi(strings.TrimSpace(row[3]))
		gpu := GPU{Index: index, UUID: strings.TrimSpace(row[1]), Name: strings.TrimSpace(row[2]), MemoryTotalMiB: memory, DriverVersion: strings.TrimSpace(row[4])}
		if indexErr != nil || memoryErr != nil || index < 0 || memory <= 0 || gpu.UUID == "" || len(gpu.UUID) > 128 || gpu.Name == "" || len(gpu.Name) > 256 || gpu.DriverVersion == "" || len(gpu.DriverVersion) > 64 {
			return nil, errors.New("nvidia-smi returned invalid bounded GPU identity")
		}
		result = append(result, gpu)
	}
	return result, nil
}

func boundedMessage(err error) string {
	message := strings.TrimSpace(err.Error())
	if len(message) > 512 {
		message = message[:512]
	}
	return message
}
