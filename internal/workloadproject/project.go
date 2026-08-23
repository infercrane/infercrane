// Package workloadproject provides the local, provider-neutral project workflow.
// It deliberately delegates image construction to Docker Buildx and deployment
// lifecycle to the InferCrane control plane.
package workloadproject

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/infercrane/infercrane/internal/planning"
	"github.com/infercrane/infercrane/internal/spec"
	"gopkg.in/yaml.v3"
)

const (
	SpecName      = "infercrane.yaml"
	MetadataDir   = ".infercrane"
	BuildMetadata = "build.json"
)

var safeName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)

type InitOptions struct {
	Directory     string
	Name          string
	Model         string
	ModelRevision string
	Runtime       string
	Cloud         string
	GPU           string
	Region        string
	ComputeMode   string
	Routing       string
	RuntimeArgs   []string
	MinReplicas   int
	MaxReplicas   int
	Force         bool
}

type BuildPlan struct {
	ProjectDir string   `json:"project_dir"`
	SpecPath   string   `json:"spec_path"`
	Context    string   `json:"context"`
	Dockerfile string   `json:"dockerfile"`
	Tag        string   `json:"tag"`
	Push       bool     `json:"push"`
	Platform   string   `json:"platform,omitempty"`
	Args       []string `json:"args"`
}

type BuildResult struct {
	Image        string `json:"image"`
	Tag          string `json:"tag"`
	Digest       string `json:"digest"`
	MetadataPath string `json:"metadata_path"`
	SpecPath     string `json:"spec_path"`
}

func Init(options InitOptions) (string, error) {
	directory := strings.TrimSpace(options.Directory)
	if directory == "" {
		directory = "."
	}
	abs, err := filepath.Abs(directory)
	if err != nil {
		return "", fmt.Errorf("resolve project directory: %w", err)
	}
	if strings.TrimSpace(options.Model) == "" {
		return "", errors.New("model is required")
	}
	name := strings.TrimSpace(options.Name)
	if name == "" {
		name = planning.DefaultName(options.Model)
	}
	if !safeName.MatchString(name) {
		return "", errors.New("name must start with a lowercase letter and contain only lowercase letters, digits, and hyphens")
	}
	runtimeName := defaultValue(options.Runtime, "vllm")
	if runtimeName != "vllm" && runtimeName != "sglang" {
		return "", errors.New("project init supports runtime vllm or sglang; use an explicit DeploymentSpec for custom OCI")
	}
	cloud := defaultValue(options.Cloud, "runpod")
	gpu := defaultValue(options.GPU, "L40S")
	if cloud == "aws" && strings.TrimSpace(options.Region) == "" {
		return "", errors.New("AWS projects require --region")
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return "", fmt.Errorf("create project directory: %w", err)
	}
	path := filepath.Join(abs, SpecName)
	if _, err = os.Stat(path); err == nil && !options.Force {
		return "", fmt.Errorf("%s already exists; use --force to replace it", path)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("inspect project spec: %w", err)
	}
	computeMode := defaultValue(options.ComputeMode, "elastic")
	routing := defaultValue(options.Routing, "round-robin")
	minReplicas, maxReplicas := options.MinReplicas, options.MaxReplicas
	if minReplicas == 0 && maxReplicas == 0 {
		minReplicas, maxReplicas = 1, 1
	}
	if minReplicas < 0 || maxReplicas < minReplicas {
		return "", errors.New("project replica bounds must satisfy 0 <= min <= max")
	}
	content := renderSpec(name, options.Model, options.ModelRevision, runtimeName, cloud, gpu, options.Region, computeMode, routing, options.RuntimeArgs, minReplicas, maxReplicas)
	if err = os.WriteFile(path, []byte(content), 0o644); err != nil {
		return "", fmt.Errorf("write project spec: %w", err)
	}
	return path, nil
}

func Discover(start string) (string, error) {
	if strings.TrimSpace(start) == "" {
		start = "."
	}
	abs, err := filepath.Abs(start)
	if err != nil {
		return "", fmt.Errorf("resolve project path: %w", err)
	}
	info, err := os.Stat(abs)
	if err == nil && !info.IsDir() {
		if filepath.Base(abs) != SpecName {
			return "", fmt.Errorf("project file must be named %s", SpecName)
		}
		return abs, nil
	}
	if err != nil {
		return "", fmt.Errorf("inspect project path: %w", err)
	}
	for {
		candidate := filepath.Join(abs, SpecName)
		if _, statErr := os.Stat(candidate); statErr == nil {
			return candidate, nil
		} else if !errors.Is(statErr, os.ErrNotExist) {
			return "", fmt.Errorf("inspect project spec: %w", statErr)
		}
		parent := filepath.Dir(abs)
		if parent == abs {
			break
		}
		abs = parent
	}
	return "", fmt.Errorf("no %s found in this directory or its parents", SpecName)
}

func Validate(start string) (string, spec.Deployment, error) {
	path, err := Discover(start)
	if err != nil {
		return "", spec.Deployment{}, err
	}
	deployment, err := spec.Load(path)
	if err != nil {
		return path, deployment, err
	}
	return path, deployment, nil
}

func PlanBuild(start, tag, dockerfile, platform string, push bool) (BuildPlan, error) {
	specPath, err := Discover(start)
	if err != nil {
		return BuildPlan{}, err
	}
	if strings.TrimSpace(tag) == "" {
		return BuildPlan{}, errors.New("--tag is required and must name a registry image")
	}
	projectDir := filepath.Dir(specPath)
	if dockerfile == "" {
		dockerfile = "Dockerfile"
	}
	dockerfilePath, err := withinProject(projectDir, dockerfile)
	if err != nil {
		return BuildPlan{}, fmt.Errorf("dockerfile: %w", err)
	}
	info, err := os.Stat(dockerfilePath)
	if err != nil {
		return BuildPlan{}, fmt.Errorf("inspect Dockerfile: %w", err)
	}
	if info.IsDir() {
		return BuildPlan{}, errors.New("Dockerfile path is a directory")
	}
	metadataPath := filepath.Join(projectDir, MetadataDir, "buildx-metadata.json")
	args := []string{"buildx", "build", "--file", dockerfilePath, "--tag", tag, "--metadata-file", metadataPath}
	if platform != "" {
		args = append(args, "--platform", platform)
	}
	if push {
		args = append(args, "--push")
	} else {
		args = append(args, "--load")
	}
	args = append(args, projectDir)
	return BuildPlan{ProjectDir: projectDir, SpecPath: specPath, Context: projectDir, Dockerfile: dockerfilePath, Tag: tag, Push: push, Platform: platform, Args: args}, nil
}

func FinalizeBuild(plan BuildPlan) (BuildResult, error) {
	metadataPath := filepath.Join(plan.ProjectDir, MetadataDir, "buildx-metadata.json")
	data, err := os.ReadFile(metadataPath)
	if err != nil {
		return BuildResult{}, fmt.Errorf("read Buildx metadata: %w", err)
	}
	var metadata map[string]any
	if err = json.Unmarshal(data, &metadata); err != nil {
		return BuildResult{}, fmt.Errorf("parse Buildx metadata: %w", err)
	}
	digest, _ := metadata["containerimage.digest"].(string)
	if !plan.Push {
		return BuildResult{}, errors.New("local build completed, but deployable identity requires --push so the registry digest can be verified")
	}
	if !regexp.MustCompile(`^sha256:[a-f0-9]{64}$`).MatchString(digest) {
		return BuildResult{}, errors.New("Buildx did not return a valid registry image digest")
	}
	image := stripDigest(plan.Tag) + "@" + digest
	if err = updateWorkloadImage(plan.SpecPath, image); err != nil {
		return BuildResult{}, err
	}
	result := BuildResult{Image: image, Tag: plan.Tag, Digest: digest, MetadataPath: filepath.Join(plan.ProjectDir, MetadataDir, BuildMetadata), SpecPath: plan.SpecPath}
	encoded, _ := json.MarshalIndent(result, "", "  ")
	if err = os.WriteFile(result.MetadataPath, append(encoded, '\n'), 0o644); err != nil {
		return BuildResult{}, fmt.Errorf("write build metadata: %w", err)
	}
	return result, nil
}

func updateWorkloadImage(path, image string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read project spec: %w", err)
	}
	var document yaml.Node
	if err = yaml.Unmarshal(data, &document); err != nil {
		return fmt.Errorf("parse project spec: %w", err)
	}
	root := mappingValue(&document, "runtime")
	if root == nil {
		return errors.New("project spec has no runtime mapping")
	}
	workload := mappingValue(root, "workload")
	if workload == nil {
		return errors.New("workload build is only valid for a custom OCI spec with runtime.workload")
	}
	imageNode := mappingValue(workload, "image")
	if imageNode == nil {
		workload.Content = append(workload.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: "image"}, &yaml.Node{Kind: yaml.ScalarNode, Value: image})
	} else {
		imageNode.Value = image
	}
	var output bytes.Buffer
	encoder := yaml.NewEncoder(&output)
	encoder.SetIndent(2)
	if err = encoder.Encode(&document); err != nil {
		return fmt.Errorf("encode project spec: %w", err)
	}
	if err = os.WriteFile(path, output.Bytes(), 0o644); err != nil {
		return fmt.Errorf("write immutable workload identity: %w", err)
	}
	return nil
}

func withinProject(projectDir, value string) (string, error) {
	path := value
	if !filepath.IsAbs(path) {
		path = filepath.Join(projectDir, path)
	}
	path = filepath.Clean(path)
	relative, err := filepath.Rel(projectDir, path)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", errors.New("path must stay inside the project directory")
	}
	return path, nil
}

func mappingValue(node *yaml.Node, key string) *yaml.Node {
	if node == nil {
		return nil
	}
	if node.Kind == yaml.DocumentNode && len(node.Content) == 1 {
		return mappingValue(node.Content[0], key)
	}
	if node.Kind != yaml.MappingNode {
		return nil
	}
	for index := 0; index+1 < len(node.Content); index += 2 {
		if node.Content[index].Value == key {
			return node.Content[index+1]
		}
	}
	return nil
}

func stripDigest(tag string) string {
	if at := strings.Index(tag, "@sha256:"); at >= 0 {
		return tag[:at]
	}
	return tag
}

func defaultValue(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return strings.TrimSpace(value)
}

func renderSpec(name, model, modelRevision, runtimeName, cloud, gpu, region, computeMode, routing string, runtimeArgs []string, minReplicas, maxReplicas int) string {
	regionLine := ""
	if strings.TrimSpace(region) != "" {
		regionLine = "\n  region: " + region
	}
	revisionLine := ""
	if strings.TrimSpace(modelRevision) != "" {
		revisionLine = "\n  revision: " + modelRevision
	}
	argsLine := ""
	if len(runtimeArgs) > 0 {
		encoded, _ := json.Marshal(runtimeArgs)
		argsLine = "\n  args: " + string(encoded)
	}
	return fmt.Sprintf(`# yaml-language-server: $schema=https://raw.githubusercontent.com/infercrane/infercrane/main/schemas/deployment-v1.schema.json
apiVersion: infercrane.dev/v1
kind: Deployment
name: %s

model:
  id: %s%s

runtime:
  engine: %s%s

compute:
  mode: %s

resources:
  gpu: %s

provider:
  cloud: %s%s

scaling:
  min_replicas: %d
  max_replicas: %d

routing:
  strategy: %s
`, name, model, revisionLine, runtimeName, argsLine, computeMode, gpu, cloud, regionLine, minReplicas, maxReplicas, routing)
}
