package agentintegration

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/infercrane/infercrane/internal/provision"
)

// TestAgentChartExecutorTolerations renders the Helm chart and checks that
// executor.tolerations reach the agent as its --tolerations flag, in a form
// the agent's own parser accepts, and that they change nothing else: they are
// for candidate pods, not for the agent's pod.
func TestAgentChartExecutorTolerations(t *testing.T) {
	helm, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("helm is not installed")
	}
	chart := filepath.Join("..", "..", "..", "deploy", "helm", "infercrane-agent")
	render := func(t *testing.T, values string) (string, error) {
		t.Helper()
		path := filepath.Join(t.TempDir(), "values.yaml")
		if err := os.WriteFile(path, []byte(values), 0o600); err != nil {
			t.Fatal(err)
		}
		rendered, err := exec.Command(helm, "template", "agent", chart, "--namespace", "models", "--values", path).CombinedOutput()
		return string(rendered), err
	}
	const executor = `
controlPlane:
  url: https://control.example.com
executor:
  enabled: true
  allowedImages: [vllm/vllm-openai]
`
	flag := regexp.MustCompile(`(?m)^\s+- ("--tolerations=.*")\s*$`)
	podTolerations := regexp.MustCompile(`(?m)^\s+tolerations:`)

	t.Run("none by default", func(t *testing.T) {
		rendered, err := render(t, executor)
		if err != nil {
			t.Fatalf("helm template: %v\n%s", err, rendered)
		}
		if !strings.Contains(rendered, "--gpu-resource=nvidia.com/gpu") || strings.Contains(rendered, "toleration") {
			t.Fatalf("the default chart mentions tolerations or lost its executor flags:\n%s", rendered)
		}
	})

	t.Run("tainted accelerator nodes", func(t *testing.T) {
		rendered, err := render(t, executor+`
  tolerations:
    - key: nvidia.com/gpu
      operator: Exists
      effect: NoSchedule
    - key: dedicated
      operator: Equal
      value: inference
      effect: NoExecute
`)
		if err != nil {
			t.Fatalf("helm template: %v\n%s", err, rendered)
		}
		flags := flag.FindAllStringSubmatch(rendered, -1)
		if len(flags) != 1 {
			t.Fatalf("the chart renders %d --tolerations flags, want 1:\n%s", len(flags), rendered)
		}
		argument, err := strconv.Unquote(flags[0][1])
		if err != nil {
			t.Fatalf("flag %s: %v", flags[0][1], err)
		}
		tolerations, err := provision.ParseKubernetesTolerations(strings.TrimPrefix(argument, "--tolerations="))
		want := []provision.KubernetesToleration{
			{Key: "nvidia.com/gpu", Operator: "Exists", Effect: "NoSchedule"},
			{Key: "dedicated", Operator: "Equal", Value: "inference", Effect: "NoExecute"},
		}
		if err != nil || !reflect.DeepEqual(tolerations, want) {
			t.Fatalf("the agent reads %#v from %s: %v", tolerations, argument, err)
		}
		if podTolerations.MatchString(rendered) {
			t.Fatalf("candidate tolerations reached the agent's own pod:\n%s", rendered)
		}
	})

	t.Run("the agent's own pod", func(t *testing.T) {
		rendered, err := render(t, executor+`
tolerations:
  - key: node-role.example.com/platform
    operator: Exists
    effect: NoSchedule
`)
		if err != nil {
			t.Fatalf("helm template: %v\n%s", err, rendered)
		}
		if !podTolerations.MatchString(rendered) || flag.MatchString(rendered) {
			t.Fatalf("top-level tolerations must reach the agent's pod only:\n%s", rendered)
		}
	})

	t.Run("without the executor", func(t *testing.T) {
		rendered, err := render(t, `
controlPlane:
  url: https://control.example.com
executor:
  tolerations:
    - {key: nvidia.com/gpu, operator: Exists, effect: NoSchedule}
`)
		if err != nil {
			t.Fatalf("helm template: %v\n%s", err, rendered)
		}
		if strings.Contains(rendered, "toleration") {
			t.Fatalf("a chart without the executor renders tolerations:\n%s", rendered)
		}
	})

	for name, entry := range map[string]string{
		"no operator":        "{key: nvidia.com/gpu, effect: NoSchedule}",
		"no effect":          "{key: nvidia.com/gpu, operator: Exists}",
		"no key":             "{operator: Exists, effect: NoSchedule}",
		"toleration seconds": "{key: nvidia.com/gpu, operator: Exists, effect: NoExecute, tolerationSeconds: 60}",
		"a taint string":     "nvidia.com/gpu:NoSchedule",
	} {
		t.Run("refuses "+name, func(t *testing.T) {
			rendered, err := render(t, executor+"  tolerations:\n    - "+entry+"\n")
			if err == nil || !strings.Contains(rendered, "executor.tolerations") {
				t.Fatalf("helm template accepted %s: %v\n%s", entry, err, rendered)
			}
		})
	}
}
