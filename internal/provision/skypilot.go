package provision

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
	"gopkg.in/yaml.v3"
)

var ErrUnavailable = errors.New("SkyPilot unavailable")

type DeploymentSpec struct {
	Name, Model, Cloud, GPU, Region, RuntimeVersion string
	RuntimeArgs                                     []string
	Port                                            int
}
type SkyPilot struct {
	Binary, APIKey string
	HealthTimeout  time.Duration
	Client         *http.Client
}

func (s SkyPilot) Deploy(ctx context.Context, spec DeploymentSpec) (domain.ProvisionedTarget, error) {
	if spec.Port == 0 {
		spec.Port = 8000
	}
	if spec.RuntimeVersion == "" {
		spec.RuntimeVersion = "0.23.0"
	}
	cluster := clusterName(spec.Name)
	binary, err := exec.LookPath(defaultString(s.Binary, "sky"))
	if err != nil {
		return domain.ProvisionedTarget{}, fmt.Errorf("%w: install the SkyPilot CLI", ErrUnavailable)
	}
	infra := spec.Cloud
	if spec.Region != "" {
		infra += "/" + spec.Region
	}
	task := map[string]any{"resources": map[string]any{"infra": infra, "accelerators": spec.GPU, "ports": []int{spec.Port}}, "setup": "python -m pip install 'vllm==" + spec.RuntimeVersion + "'", "run": runCommand(spec)}
	task["secrets"] = map[string]any{"INFERCRANE_WORKER_API_KEY": nil}
	data, _ := yaml.Marshal(task)
	file, err := os.CreateTemp("", "infercrane-skypilot-*.yaml")
	if err != nil {
		return domain.ProvisionedTarget{}, err
	}
	path := file.Name()
	defer os.Remove(path)
	if _, err = file.Write(data); err != nil {
		file.Close()
		return domain.ProvisionedTarget{}, err
	}
	if err = file.Close(); err != nil {
		return domain.ProvisionedTarget{}, err
	}
	cmd := exec.CommandContext(ctx, binary, "launch", "-c", cluster, "-y", "--secret", "INFERCRANE_WORKER_API_KEY", path)
	cmd.Env = append(os.Environ(), "INFERCRANE_WORKER_API_KEY="+s.APIKey)
	if output, err := cmd.CombinedOutput(); err != nil {
		return domain.ProvisionedTarget{}, fmt.Errorf("%w: launch %s: %s", ErrUnavailable, cluster, strings.TrimSpace(string(output)))
	}
	endpointOutput, err := exec.CommandContext(ctx, binary, "status", cluster, "--endpoint", strconvPort(spec.Port)).Output()
	if err != nil {
		return domain.ProvisionedTarget{}, fmt.Errorf("%w: resolve endpoint for %s", ErrUnavailable, cluster)
	}
	endpoint := strings.TrimSpace(string(endpointOutput))
	if err = s.waitHealthy(ctx, endpoint, spec.Model); err != nil {
		return domain.ProvisionedTarget{}, err
	}
	details, _ := json.Marshal(map[string]any{"cloud": spec.Cloud, "region": spec.Region, "gpu": spec.GPU, "runtime": "vllm", "runtime_version": spec.RuntimeVersion, "runtime_args": spec.RuntimeArgs})
	return domain.ProvisionedTarget{Name: spec.Name + "-0", URL: strings.TrimRight(endpoint, "/"), ProviderResourceID: cluster, UpstreamModel: spec.Model, Details: string(details)}, nil
}
func (s SkyPilot) Destroy(ctx context.Context, id string) error {
	binary, err := exec.LookPath(defaultString(s.Binary, "sky"))
	if err != nil {
		return fmt.Errorf("%w: install the SkyPilot CLI", ErrUnavailable)
	}
	output, err := exec.CommandContext(ctx, binary, "down", "-y", id).CombinedOutput()
	if err != nil {
		return fmt.Errorf("destroy %s: %s", id, strings.TrimSpace(string(output)))
	}
	return nil
}
func (s SkyPilot) waitHealthy(ctx context.Context, endpoint, model string) error {
	client := s.Client
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	timeout := s.HealthTimeout
	if timeout == 0 {
		timeout = 15 * time.Minute
	}
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(endpoint, "/")+"/v1/models", nil)
		req.Header.Set("Authorization", "Bearer "+s.APIKey)
		if resp, err := client.Do(req); err == nil {
			var body struct {
				Data []struct {
					ID string `json:"id"`
				} `json:"data"`
			}
			_ = json.NewDecoder(resp.Body).Decode(&body)
			resp.Body.Close()
			for _, item := range body.Data {
				if resp.StatusCode == 200 && item.ID == model {
					return nil
				}
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("%w: vLLM did not become healthy at %s", ErrUnavailable, endpoint)
		case <-ticker.C:
		}
	}
}
func clusterName(name string) string {
	slug := regexp.MustCompile(`[^a-z0-9-]+`).ReplaceAllString(strings.ToLower(name), "-")
	slug = strings.Trim(slug, "-")
	result := "infercrane-" + slug
	if len(result) > 63 {
		result = result[:63]
	}
	return strings.TrimRight(result, "-")
}
func runCommand(s DeploymentSpec) string {
	args := make([]string, len(s.RuntimeArgs))
	for i, arg := range s.RuntimeArgs {
		args[i] = shellQuote(arg)
	}
	return fmt.Sprintf(`vllm serve %s --host 0.0.0.0 --port %d --served-model-name %s --api-key "$INFERCRANE_WORKER_API_KEY" %s`, shellQuote(s.Model), s.Port, shellQuote(s.Model), strings.Join(args, " "))
}
func shellQuote(value string) string { return "'" + strings.ReplaceAll(value, "'", `'"'"'`) + "'" }
func defaultString(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}
func strconvPort(port int) string { return fmt.Sprintf("%d", port) }
