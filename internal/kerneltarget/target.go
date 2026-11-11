// Package kerneltarget profiles and qualifies kernels on one GPU host using
// Docker and AIPerf. Serving containers run offline on an internal network
// without credentials, because a candidate arm executes generated code inside
// a real server. Evidence is returned to the Accelerator Lab, which alone
// decides whether a candidate qualifies.
package kerneltarget

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/infercrane/infercrane/internal/acceleratorlab"
	"github.com/infercrane/infercrane/internal/benchmark"
	"github.com/infercrane/infercrane/internal/brezelexecutor"
	"github.com/infercrane/infercrane/internal/kernelgen"
	"github.com/infercrane/infercrane/internal/kernelplanner"
)

const (
	RuntimeCatalogSchemaV1 = "infercrane.kernel-target-runtimes/v1"

	internalNetwork     = "infercrane-kernel-target"
	defaultReadyTimeout = 30 * time.Minute
	readyPollInterval   = 2 * time.Second
	memoryPollInterval  = 2 * time.Second
	containerModelCache = "/model-cache"
	containerProfileDir = "/profiles"
	maxRuntimeFileBytes = 1 << 20
)

// RuntimeSpec is reviewed launch data for one exact runtime version, so the
// tool never branches on a runtime name.
type RuntimeSpec struct {
	Name           string          `json:"name"`
	Version        string          `json:"version"`
	Repository     string          `json:"repository"`
	Python         string          `json:"python"`
	Launcher       []string        `json:"launcher"`
	Port           int             `json:"port"`
	ProfilerEnv    string          `json:"profiler_env,omitempty"`
	ProfilerArgs   []string        `json:"profiler_args,omitempty"`
	ProfileRequest json.RawMessage `json:"profile_request,omitempty"`
}

// LoadRuntimes reads and validates the reviewed runtime catalog.
func LoadRuntimes(path string) ([]RuntimeSpec, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("inspect runtime catalog: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() > maxRuntimeFileBytes {
		return nil, errors.New("runtime catalog must be a regular file within the size limit")
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read runtime catalog: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var catalog struct {
		SchemaVersion string        `json:"schema_version"`
		Runtimes      []RuntimeSpec `json:"runtimes"`
	}
	if err = decoder.Decode(&catalog); err != nil {
		return nil, fmt.Errorf("decode runtime catalog: %w", err)
	}
	if catalog.SchemaVersion != RuntimeCatalogSchemaV1 || len(catalog.Runtimes) == 0 {
		return nil, errors.New("runtime catalog requires the v1 schema and at least one runtime")
	}
	for _, spec := range catalog.Runtimes {
		if spec.Name == "" || spec.Version == "" || spec.Repository == "" || spec.Python == "" || len(spec.Launcher) == 0 || spec.Port < 1 || spec.Port > 65535 || spec.ProfilerEnv == "" && len(spec.ProfilerArgs) == 0 {
			return nil, fmt.Errorf("runtime catalog entry %q is incomplete", spec.Name)
		}
	}
	return catalog.Runtimes, nil
}

// Commander runs host tools without a shell.
type Commander interface {
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
}

// ExecCommander runs real host commands with an explicit environment.
type ExecCommander struct{ Environment []string }

func (c ExecCommander) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, name, args...)
	command.Env = c.Environment
	var stderr bytes.Buffer
	command.Stderr = &stderr
	output, err := command.Output()
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w: %s", name, firstArgument(args), err, boundedText(stderr.String()))
	}
	return output, nil
}

func firstArgument(args []string) string {
	if len(args) == 0 {
		return ""
	}
	return args[0]
}

func boundedText(value string) string {
	value = strings.TrimSpace(value)
	if len(value) > 400 {
		return value[len(value)-400:]
	}
	return value
}

// LoadGenerator measures a running endpoint.
type LoadGenerator interface {
	Run(context.Context, benchmark.Config) (benchmark.Result, error)
}

// ArtifactOpener reads build output from the worker's content-addressed store.
type ArtifactOpener interface {
	Open(context.Context, brezelexecutor.ArtifactRef) (io.ReadCloser, error)
}

// Target holds the host resources one profile or qualification run uses.
type Target struct {
	Runtimes     []RuntimeSpec
	ModelCache   string
	WorkDir      string
	PackDir      string
	ReplayDir    string
	ReplayType   string
	AIPerf       string
	HasHFToken   bool
	GPUHourlyUSD float64
	Commands     Commander
	Load         LoadGenerator
	Evaluator    kernelgen.Evaluator
	Artifacts    ArtifactOpener
	HTTP         *http.Client
	ReadyTimeout time.Duration
	PollInterval time.Duration
	Now          func() time.Time
}

func (t Target) now() time.Time {
	if t.Now != nil {
		return t.Now().UTC()
	}
	return time.Now().UTC()
}

func (t Target) docker(ctx context.Context, args ...string) ([]byte, error) {
	return t.Commands.Run(ctx, "docker", args...)
}

func (t Target) client() *http.Client {
	if t.HTTP != nil {
		return t.HTTP
	}
	return &http.Client{Timeout: 10 * time.Minute}
}

func (t Target) elapsedCost(started time.Time) float64 {
	return t.now().Sub(started).Hours() * t.GPUHourlyUSD
}

func (t Target) runtime(identity kernelplanner.RuntimeIdentity) (RuntimeSpec, error) {
	if !digestPattern.MatchString(identity.ImageDigest) {
		return RuntimeSpec{}, errors.New("runtime image digest must be a sha256 digest")
	}
	for _, spec := range t.Runtimes {
		if spec.Name == identity.Name && spec.Version == identity.Version {
			return spec, nil
		}
	}
	return RuntimeSpec{}, fmt.Errorf("no reviewed target runtime for %s %s", identity.Name, identity.Version)
}

var (
	digestPattern = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
	modelRepoID   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*/[A-Za-z0-9][A-Za-z0-9._-]*$`)
	modelCommit   = regexp.MustCompile(`^[a-f0-9]{40,64}$`)
)

func validateBoundary(model kernelplanner.ModelIdentity, topology acceleratorlab.Topology) error {
	if !modelRepoID.MatchString(model.Repository) || !modelCommit.MatchString(model.Revision) {
		return errors.New("target run requires an owner/name model and an immutable commit")
	}
	if topology.Nodes != 1 || topology.Accelerators != 1 {
		return errors.New("this target adapter supports exactly one node and one accelerator")
	}
	return nil
}

// verifyAccelerator checks the physical GPU so a result can never be labeled
// with hardware it did not run on.
func (t Target) verifyAccelerator(ctx context.Context, hardware kernelplanner.HardwareIdentity) error {
	output, err := t.Commands.Run(ctx, "nvidia-smi", "--query-gpu=name", "--format=csv,noheader")
	if err != nil {
		return fmt.Errorf("identify GPU: %w", err)
	}
	names := strings.Split(strings.TrimSpace(string(output)), "\n")
	if len(names) != 1 || strings.TrimSpace(names[0]) == "" {
		return errors.New("this target adapter requires exactly one visible GPU")
	}
	if !strings.Contains(strings.ToUpper(names[0]), strings.ToUpper(hardware.Accelerator)) {
		return fmt.Errorf("host GPU %q does not match requested accelerator %q", strings.TrimSpace(names[0]), hardware.Accelerator)
	}
	return nil
}

func (t Target) snapshotPath(model kernelplanner.ModelIdentity) string {
	return filepath.Join(t.ModelCache, "hub", "models--"+strings.ReplaceAll(model.Repository, "/", "--"), "snapshots", model.Revision)
}

// materialize downloads the pinned model with the trusted runtime image. It is
// the only step with internet access and the only one given the Hub token.
func (t Target) materialize(ctx context.Context, spec RuntimeSpec, image string, model kernelplanner.ModelIdentity) error {
	if _, err := os.Stat(filepath.Join(t.snapshotPath(model), "config.json")); err == nil {
		return nil
	}
	const download = "import sys; from huggingface_hub import snapshot_download; snapshot_download(repo_id=sys.argv[1], revision=sys.argv[2], cache_dir=sys.argv[3])"
	args := []string{"run", "--rm", "--entrypoint", spec.Python, "--volume", t.ModelCache + ":" + containerModelCache}
	if t.HasHFToken {
		args = append(args, "--env", "HF_TOKEN")
	}
	args = append(args, image, "-c", download, model.Repository, model.Revision, containerModelCache+"/hub")
	if _, err := t.docker(ctx, args...); err != nil {
		return fmt.Errorf("materialize model: %w", err)
	}
	return nil
}

func (t Target) ensureNetwork(ctx context.Context) error {
	if _, err := t.docker(ctx, "network", "inspect", internalNetwork); err == nil {
		return nil
	}
	if _, err := t.docker(ctx, "network", "create", "--internal", internalNetwork); err != nil {
		return fmt.Errorf("create internal network: %w", err)
	}
	return nil
}

// privateDirectory creates a run directory a container user can write to. The
// configured work directory stays owner-only and protects it from other users.
func (t Target) privateDirectory(prefix string) (string, error) {
	directory, err := os.MkdirTemp(t.WorkDir, prefix)
	if err != nil {
		return "", fmt.Errorf("create run directory: %w", err)
	}
	if err = os.Chmod(directory, 0o777); err != nil {
		return "", fmt.Errorf("prepare run directory: %w", err)
	}
	return directory, nil
}

type kernelMount struct {
	SourceDirectory string
	Filename        string
	StateDirectory  string
	Integration     string
	Args            []string
}

type serverRequest struct {
	Spec       RuntimeSpec
	Image      string
	Model      kernelplanner.ModelIdentity
	Name       string
	ProfileDir string
	Kernel     *kernelMount
}

type server struct {
	target  Target
	name    string
	baseURL string
}

func (t Target) start(ctx context.Context, request serverRequest) (*server, error) {
	// A crashed earlier run must not hold the GPU or the container name.
	_, _ = t.docker(ctx, "rm", "--force", request.Name)
	args := []string{
		"run", "--detach", "--name", request.Name, "--gpus", "all", "--network", internalNetwork,
		"--read-only", "--tmpfs", "/tmp:rw,nosuid,size=8g", "--shm-size", "16g", "--cap-drop", "ALL",
		"--security-opt", "no-new-privileges", "--pids-limit", "4096", "--entrypoint", request.Spec.Launcher[0],
		"--env", "HF_HOME=" + containerModelCache, "--env", "HF_HUB_OFFLINE=1", "--env", "TRANSFORMERS_OFFLINE=1",
		"--env", "HOME=/tmp", "--env", "TMPDIR=/tmp", "--env", "TRITON_CACHE_DIR=/tmp/triton",
		"--volume", t.ModelCache + ":" + containerModelCache + ":ro",
		"--volume", request.ProfileDir + ":" + containerProfileDir,
	}
	if request.Spec.ProfilerEnv != "" {
		args = append(args, "--env", request.Spec.ProfilerEnv+"="+containerProfileDir)
	}
	if request.Kernel != nil {
		args = append(args,
			"--volume", request.Kernel.SourceDirectory+":/kernel:ro",
			"--volume", request.Kernel.StateDirectory+":/kernel-state",
			"--volume", request.Kernel.Integration+":/kernel-integration:ro",
			"--env", "PYTHONPATH=/kernel-integration",
			"--env", "INFERCRANE_KERNEL_SOURCE=/kernel/"+request.Kernel.Filename,
			"--env", "INFERCRANE_KERNEL_STATE=/kernel-state",
		)
	}
	args = append(args, request.Image)
	replacer := strings.NewReplacer("${MODEL_REVISION}", request.Model.Revision, "${MODEL}", request.Model.Repository, "${PORT}", strconv.Itoa(request.Spec.Port), "${PROFILE_DIR}", containerProfileDir)
	launch := append(append([]string(nil), request.Spec.Launcher[1:]...), request.Spec.ProfilerArgs...)
	if request.Kernel != nil {
		launch = append(launch, request.Kernel.Args...)
	}
	for _, argument := range launch {
		args = append(args, replacer.Replace(argument))
	}
	if _, err := t.docker(ctx, args...); err != nil {
		return nil, fmt.Errorf("start serving container: %w", err)
	}
	instance := &server{target: t, name: request.Name}
	address, err := t.docker(ctx, "inspect", "--format", "{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}", request.Name)
	if err != nil {
		instance.stop()
		return nil, fmt.Errorf("find serving container: %w", err)
	}
	ip := net.ParseIP(strings.TrimSpace(string(address)))
	if ip == nil {
		instance.stop()
		return nil, errors.New("serving container has no address")
	}
	instance.baseURL = "http://" + net.JoinHostPort(ip.String(), strconv.Itoa(request.Spec.Port))
	if err = instance.waitReady(ctx); err != nil {
		instance.stop()
		return nil, err
	}
	return instance, nil
}

func (s *server) waitReady(ctx context.Context) error {
	timeout := s.target.ReadyTimeout
	if timeout <= 0 {
		timeout = defaultReadyTimeout
	}
	interval := s.target.PollInterval
	if interval <= 0 {
		interval = readyPollInterval
	}
	readyContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for {
		request, err := http.NewRequestWithContext(readyContext, http.MethodGet, s.baseURL+"/health", nil)
		if err != nil {
			return err
		}
		if response, doErr := s.target.client().Do(request); doErr == nil {
			response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return nil
			}
		}
		// A server that has exited will never become ready, so do not keep
		// paying for the GPU until the timeout.
		if state, stateErr := s.target.docker(readyContext, "inspect", "--format", "{{.State.Running}}", s.name); stateErr == nil && strings.TrimSpace(string(state)) == "false" {
			return errors.New("serving container exited before becoming ready")
		}
		select {
		case <-readyContext.Done():
			return errors.New("serving container did not become ready")
		case <-time.After(interval):
		}
	}
}

// stop always uses a fresh context so cleanup survives a cancelled run.
func (s *server) stop() {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	_, _ = s.target.docker(ctx, "rm", "--force", s.name)
}

func (s *server) post(ctx context.Context, path string, body []byte) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, s.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := s.target.client().Do(request)
	if err != nil {
		return nil, fmt.Errorf("post %s: %w", path, err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 16<<20))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return nil, fmt.Errorf("post %s: unexpected status %d", path, response.StatusCode)
	}
	return data, nil
}

// peakMemory samples GPU memory until stop is called.
func (t Target) peakMemory(ctx context.Context) func() float64 {
	interval := t.PollInterval
	if interval <= 0 {
		interval = memoryPollInterval
	}
	sample := func() float64 {
		output, err := t.Commands.Run(ctx, "nvidia-smi", "--query-gpu=memory.used", "--format=csv,noheader,nounits")
		if err != nil {
			return 0
		}
		value, err := strconv.ParseFloat(strings.TrimSpace(string(output)), 64)
		if err != nil {
			return 0
		}
		return value
	}
	done := make(chan struct{})
	result := make(chan float64, 1)
	go func() {
		peak := sample()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				if latest := sample(); latest > peak {
					peak = latest
				}
				result <- peak
				return
			case <-ticker.C:
				if latest := sample(); latest > peak {
					peak = latest
				}
			}
		}
	}()
	return func() float64 {
		close(done)
		return <-result
	}
}
