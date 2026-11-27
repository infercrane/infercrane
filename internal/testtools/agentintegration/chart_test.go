package agentintegration

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestAgentChartImage renders the Helm chart and checks which image the agent
// runs. The release workflow tags the image with the git tag ("v" + the
// version), so a chart installed with its defaults must ask for that tag; a
// bare version would leave the agent's pod unable to pull its image.
func TestAgentChartImage(t *testing.T) {
	helm, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("helm is not installed")
	}
	root := filepath.Join("..", "..", "..")
	chart := filepath.Join(root, "deploy", "helm", "infercrane-agent")

	metadata, err := os.ReadFile(filepath.Join(chart, "Chart.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	match := regexp.MustCompile(`(?m)^appVersion:\s*"?([^"\s]+)"?\s*$`).FindSubmatch(metadata)
	if match == nil {
		t.Fatal("Chart.yaml has no appVersion")
	}
	appVersion := string(match[1])

	// The tag the chart defaults to is the tag the release workflow pushes.
	workflow, err := os.ReadFile(filepath.Join(root, ".github", "workflows", "release.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(workflow), "tags: ghcr.io/infercrane/infercrane:${{ github.ref_name }}") {
		t.Fatal("the release workflow no longer tags the image with the git tag; update the chart's default tag and this test together")
	}
	if strings.HasPrefix(appVersion, "v") {
		t.Fatalf("appVersion %q must be the bare version; the chart adds the v", appVersion)
	}

	const digest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	for _, test := range []struct {
		name string
		set  []string
		want string
	}{
		{name: "default", want: "ghcr.io/infercrane/infercrane:v" + appVersion},
		{name: "tag", set: []string{"image.tag=v9.9.9-rc.1"}, want: "ghcr.io/infercrane/infercrane:v9.9.9-rc.1"},
		{name: "digest", set: []string{"image.repository=registry.example.com/infercrane", "image.digest=" + digest}, want: "registry.example.com/infercrane@" + digest},
		{name: "digest wins over tag", set: []string{"image.tag=v9.9.9", "image.digest=" + digest}, want: "ghcr.io/infercrane/infercrane@" + digest},
	} {
		t.Run(test.name, func(t *testing.T) {
			args := []string{"template", "agent", chart, "--namespace", "models", "--set", "controlPlane.url=https://control.example.com"}
			for _, value := range test.set {
				args = append(args, "--set", value)
			}
			rendered, err := exec.Command(helm, args...).CombinedOutput()
			if err != nil {
				t.Fatalf("helm template: %v\n%s", err, rendered)
			}
			images := regexp.MustCompile(`(?m)^\s+image:\s*(\S+)\s*$`).FindAllSubmatch(rendered, -1)
			if len(images) != 1 {
				t.Fatalf("the chart renders %d images, want 1", len(images))
			}
			if got := string(images[0][1]); got != test.want {
				t.Fatalf("image = %s, want %s", got, test.want)
			}
		})
	}
}
