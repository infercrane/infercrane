package router

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os/exec"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
)

var ErrUnavailable = errors.New("router unavailable")

type Spec struct {
	DeploymentID, ProcessID string
	Workers                 []string
	Strategy, Host          string
	Port                    int
}
type Backend interface {
	Start(context.Context, Spec) (string, error)
	Stop(string) error
	Running(string) bool
}

type VLLM struct {
	Binary, APIKey string
	mu             sync.Mutex
	processes      map[string]*process
}

type process struct {
	cmd  *exec.Cmd
	done chan error
}

func NewVLLM(binary, apiKey string) *VLLM {
	return &VLLM{Binary: binary, APIKey: apiKey, processes: make(map[string]*process)}
}

func (v *VLLM) Command(binary string, s Spec) []string {
	args := []string{binary, "--host", s.Host, "--port", strconv.Itoa(s.Port), "--policy", domain.RoutingStrategies[s.Strategy], "--worker-urls"}
	args = append(args, s.Workers...)
	// vLLM Router otherwise binds every process to metrics port 29000. Give each
	// generation a deterministic companion port so the candidate and retiring
	// generations can overlap safely during publication.
	metricsPort := s.Port + 30000
	if metricsPort > 65535 {
		metricsPort = s.Port - 10000
	}
	return append(args, "--api-key", v.APIKey, "--retry-max-retries", "1", "--prometheus-port", strconv.Itoa(metricsPort))
}
func (v *VLLM) Start(ctx context.Context, s Spec) (string, error) {
	processID := s.ProcessID
	if processID == "" {
		processID = s.DeploymentID
	}
	if v.Running(processID) {
		return "", fmt.Errorf("%w: process %s is already running", ErrUnavailable, processID)
	}
	binary, err := exec.LookPath(v.Binary)
	if err != nil {
		return "", fmt.Errorf("%w: %q is not installed", ErrUnavailable, v.Binary)
	}
	var stderr bytes.Buffer
	cmd := exec.Command(v.Command(binary, s)[0], v.Command(binary, s)[1:]...)
	cmd.Stderr = &stderr
	if err = cmd.Start(); err != nil {
		return "", fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	running := &process{cmd: cmd, done: make(chan error, 1)}
	go func() { running.done <- cmd.Wait() }()
	v.mu.Lock()
	v.processes[processID] = running
	v.mu.Unlock()
	endpoint := fmt.Sprintf("http://%s:%d", s.Host, s.Port)
	client := http.Client{Timeout: 500 * time.Millisecond}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			_ = v.Stop(processID)
			return "", ctx.Err()
		case <-timer.C:
			_ = v.Stop(processID)
			return "", fmt.Errorf("%w: readiness timeout", ErrUnavailable)
		case <-ticker.C:
			select {
			case <-running.done:
				v.mu.Lock()
				if v.processes[processID] == running {
					delete(v.processes, processID)
				}
				v.mu.Unlock()
				return "", fmt.Errorf("%w: exited during startup: %s", ErrUnavailable, stderr.String())
			default:
			}
			req, _ := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"/health", nil)
			resp, e := client.Do(req)
			if e == nil {
				resp.Body.Close()
				if resp.StatusCode < 500 {
					return endpoint, nil
				}
			}
		}
	}
}
func (v *VLLM) Stop(id string) error {
	v.mu.Lock()
	running := v.processes[id]
	delete(v.processes, id)
	v.mu.Unlock()
	if running == nil || running.cmd.Process == nil {
		return nil
	}
	_ = running.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-running.done:
		return nil
	case <-time.After(5 * time.Second):
		_ = running.cmd.Process.Kill()
		<-running.done
		return nil
	}
}
func (v *VLLM) Running(id string) bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	running := v.processes[id]
	if running == nil || running.cmd.Process == nil {
		return false
	}
	select {
	case <-running.done:
		delete(v.processes, id)
		return false
	default:
		return true
	}
}
