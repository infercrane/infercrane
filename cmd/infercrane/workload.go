package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/infercrane/infercrane/internal/config"
	"github.com/infercrane/infercrane/internal/curatedrecipe"
	"github.com/infercrane/infercrane/internal/workloadproject"
)

func workloadCommand(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: infercrane workload init|validate|build|dev|plan|deploy [PATH] [flags]")
	}
	switch args[0] {
	case "init":
		return workloadInitCommand(args[1:])
	case "validate":
		return workloadValidateCommand(args[1:])
	case "build":
		return workloadBuildCommand(ctx, args[1:])
	case "dev":
		return workloadDevCommand(ctx, args[1:])
	case "plan", "deploy":
		return workloadControlPlaneCommand(ctx, args[0], args[1:])
	default:
		return fmt.Errorf("unknown workload action %q; use init, validate, build, dev, plan, or deploy", args[0])
	}
}

func workloadInitCommand(args []string) error {
	leadingPath, args := workloadLeadingPath(args)
	fs := flag.NewFlagSet("workload init", flag.ContinueOnError)
	model := fs.String("model", "", "Hugging Face model identity")
	recipeName := fs.String("recipe", "", "reviewed curated recipe name")
	profileName := fs.String("profile", "", "reviewed serving profile from the selected recipe")
	name := fs.String("name", "", "deployment name")
	runtimeName := fs.String("runtime", "vllm", "vllm or sglang")
	cloud := fs.String("cloud", "runpod", "infrastructure provider")
	gpu := fs.String("gpu", "L40S", "GPU type")
	region := fs.String("region", "", "provider region")
	force := fs.Bool("force", false, "replace an existing infercrane.yaml")
	output := fs.String("output", "human", "human or json")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := validateOutput(*output); err != nil {
		return err
	}
	explicit := map[string]bool{}
	fs.Visit(func(value *flag.Flag) { explicit[value.Name] = true })
	if fs.NArg() > 1 || (leadingPath != "" && fs.NArg() != 0) {
		return errors.New("workload init accepts at most one project directory")
	}
	directory := leadingPath
	if directory == "" {
		directory = "."
	}
	if fs.NArg() == 1 {
		directory = fs.Arg(0)
	}
	if strings.TrimSpace(*model) == "" && strings.TrimSpace(*recipeName) == "" {
		return errors.New("workload init requires --model MODEL or --recipe NAME; list reviewed recipes with `infercrane recipes curated`")
	}
	modelRevision := ""
	selectedProfile := curatedrecipe.ServingProfile{}
	routing := "round-robin"
	if *recipeName != "" {
		if *model != "" {
			return errors.New("use either --recipe or --model")
		}
		entry, found := curatedrecipe.Get(*recipeName)
		if !found {
			return fmt.Errorf("curated recipe %q was not found; list with `infercrane recipes curated`", *recipeName)
		}
		if len(entry.Profiles) == 0 {
			return fmt.Errorf("curated recipe %q has no serving profiles", *recipeName)
		}
		if *profileName == "" {
			selectedProfile = entry.Profiles[0]
		} else {
			for _, profile := range entry.Profiles {
				if profile.Name == *profileName {
					selectedProfile = profile
					break
				}
			}
			if selectedProfile.Name == "" {
				return fmt.Errorf("serving profile %q is not available for recipe %q", *profileName, *recipeName)
			}
		}
		if explicit["runtime"] && *runtimeName != selectedProfile.Runtime {
			return fmt.Errorf("serving profile %q requires runtime %q; edit the generated DeploymentSpec for an unreviewed combination", selectedProfile.Name, selectedProfile.Runtime)
		}
		*model, modelRevision, *runtimeName = entry.Model, entry.Revision, selectedProfile.Runtime
		if !explicit["gpu"] && selectedProfile.GPUHint != "" {
			*gpu = selectedProfile.GPUHint
		}
		routing = "cache-aware"
		if entry.Gated {
			fmt.Fprintln(os.Stderr, "Notice: this model is gated. Confirm repository access and accept its model license before deployment.")
		}
	} else if *profileName != "" {
		return errors.New("--profile requires --recipe")
	}
	path, err := workloadproject.Init(workloadproject.InitOptions{Directory: directory, Name: *name, Model: *model, ModelRevision: modelRevision, Runtime: *runtimeName, Cloud: *cloud, GPU: *gpu, Region: *region, ComputeMode: selectedProfile.ComputeMode, Routing: routing, RuntimeArgs: selectedProfile.RuntimeArgs, MinReplicas: selectedProfile.MinReplicas, MaxReplicas: selectedProfile.MaxReplicas, Force: *force})
	if err != nil {
		return err
	}
	result := map[string]any{"project": filepath.Dir(path), "spec": path, "model": *model, "model_revision": modelRevision, "runtime": *runtimeName, "provider": *cloud, "curated_recipe": *recipeName, "serving_profile": selectedProfile.Name}
	if *output == "json" {
		return printJSON(result)
	}
	fmt.Printf("Inference project created\nProject  %s\nSpec     %s\n\nNext:\n  cd %s\n  infercrane workload validate\n  infercrane workload plan\n  infercrane workload deploy --wait\n", filepath.Dir(path), path, shellDisplay(filepath.Dir(path)))
	return nil
}

func workloadValidateCommand(args []string) error {
	leadingPath, args := workloadLeadingPath(args)
	fs := flag.NewFlagSet("workload validate", flag.ContinueOnError)
	output := fs.String("output", "human", "human or json")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := validateOutput(*output); err != nil {
		return err
	}
	if fs.NArg() > 1 || (leadingPath != "" && fs.NArg() != 0) {
		return errors.New("workload validate accepts at most one project path")
	}
	path := leadingPath
	if path == "" {
		path = "."
	}
	if fs.NArg() == 1 {
		path = fs.Arg(0)
	}
	specPath, deployment, err := workloadproject.Validate(path)
	if err != nil {
		return err
	}
	result := map[string]any{"valid": true, "spec": specPath, "name": deployment.Name, "model": deployment.Model.ID, "runtime": deployment.Runtime.Engine, "provider": deployment.Provider.Cloud, "compute_mode": deployment.Compute.Mode}
	if *output == "json" {
		return printJSON(result)
	}
	fmt.Printf("Project valid\nSpec      %s\nWorkload  %s\nModel     %s\nRuntime   %s\nProvider  %s\n", specPath, deployment.Name, deployment.Model.ID, deployment.Runtime.Engine, deployment.Provider.Cloud)
	return nil
}

func workloadBuildCommand(ctx context.Context, args []string) error {
	leadingPath, args := workloadLeadingPath(args)
	fs := flag.NewFlagSet("workload build", flag.ContinueOnError)
	tag := fs.String("tag", "", "registry image tag")
	dockerfile := fs.String("file", "Dockerfile", "Dockerfile path inside the project")
	platform := fs.String("platform", "", "target platform, for example linux/amd64")
	push := fs.Bool("push", false, "push and record the registry-confirmed immutable digest")
	output := fs.String("output", "human", "human or json")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := validateOutput(*output); err != nil {
		return err
	}
	if fs.NArg() > 1 || (leadingPath != "" && fs.NArg() != 0) {
		return errors.New("workload build accepts at most one project path")
	}
	path := leadingPath
	if path == "" {
		path = "."
	}
	if fs.NArg() == 1 {
		path = fs.Arg(0)
	}
	_, deployment, err := workloadproject.Validate(path)
	if err != nil {
		return err
	}
	if deployment.Runtime.Engine != "custom-oci" || deployment.Runtime.Workload.Empty() {
		return errors.New("workload build requires a custom-oci DeploymentSpec; vLLM/SGLang projects use their qualified runtime adapter")
	}
	plan, err := workloadproject.PlanBuild(path, *tag, *dockerfile, *platform, *push)
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Join(plan.ProjectDir, workloadproject.MetadataDir), 0o755); err != nil {
		return fmt.Errorf("create build metadata directory: %w", err)
	}
	command := exec.CommandContext(ctx, "docker", plan.Args...)
	command.Dir = plan.ProjectDir
	command.Stdin = os.Stdin
	command.Stderr = os.Stderr
	if *output == "human" {
		command.Stdout = os.Stdout
	} else {
		command.Stdout = os.Stderr
	}
	if err = command.Run(); err != nil {
		return fmt.Errorf("Docker Buildx failed: %w", err)
	}
	if !plan.Push {
		result := map[string]any{"built": true, "deployable": false, "tag": plan.Tag, "reason": "local build has no registry-confirmed immutable digest", "next": "rerun with --push to update runtime.workload.image"}
		if *output == "json" {
			return printJSON(result)
		}
		fmt.Printf("Local image built  %s\nDeployable         no\n\nPush and record an immutable registry digest:\n  infercrane workload build --tag %s --push\n", plan.Tag, plan.Tag)
		return nil
	}
	result, err := workloadproject.FinalizeBuild(plan)
	if err != nil {
		return err
	}
	if *output == "json" {
		return printJSON(result)
	}
	fmt.Printf("Workload image published\nImage   %s\nSpec    %s\n\nThe DeploymentSpec now references the registry-confirmed immutable digest.\n", result.Image, result.SpecPath)
	return nil
}

func workloadDevCommand(ctx context.Context, args []string) error {
	leadingPath, args := workloadLeadingPath(args)
	fs := flag.NewFlagSet("workload dev", flag.ContinueOnError)
	hostPort := fs.Int("port", 8000, "loopback port")
	detach := fs.Bool("detach", false, "run in the background")
	output := fs.String("output", "human", "human or json")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := validateOutput(*output); err != nil {
		return err
	}
	if *hostPort < 1 || *hostPort > 65535 {
		return errors.New("--port must be between 1 and 65535")
	}
	if fs.NArg() > 1 || (leadingPath != "" && fs.NArg() != 0) {
		return errors.New("workload dev accepts at most one project path")
	}
	path := leadingPath
	if path == "" {
		path = "."
	}
	if fs.NArg() == 1 {
		path = fs.Arg(0)
	}
	_, deployment, err := workloadproject.Validate(path)
	if err != nil {
		return err
	}
	if deployment.Runtime.Engine != "custom-oci" || deployment.Runtime.Workload.Empty() {
		return errors.New("workload dev requires a custom-oci DeploymentSpec; vLLM/SGLang model projects deploy through their qualified runtime adapter")
	}
	containerName := "infercrane-dev-" + deployment.Name
	port := strconv.Itoa(*hostPort) + ":" + strconv.Itoa(deployment.Runtime.Workload.Port)
	dockerArgs := []string{"run", "--rm", "--name", containerName, "--publish", "127.0.0.1:" + port, "--env", "MODEL=" + deployment.Model.ID, "--env", "PORT=" + strconv.Itoa(deployment.Runtime.Workload.Port)}
	if *detach {
		dockerArgs = append(dockerArgs, "--detach")
	}
	dockerArgs = append(dockerArgs, deployment.Runtime.Workload.Image)
	dockerArgs = append(dockerArgs, deployment.Runtime.Workload.Command...)
	command := exec.CommandContext(ctx, "docker", dockerArgs...)
	command.Stdin, command.Stderr = os.Stdin, os.Stderr
	if *output == "human" {
		command.Stdout = os.Stdout
	} else {
		command.Stdout = os.Stderr
	}
	if err = command.Run(); err != nil {
		return fmt.Errorf("start local workload: %w", err)
	}
	result := map[string]any{"container": containerName, "endpoint": fmt.Sprintf("http://127.0.0.1:%d/v1", *hostPort), "detached": *detach}
	if *output == "json" {
		return printJSON(result)
	}
	if *detach {
		fmt.Printf("\nLocal workload started\nEndpoint  %s\nLogs      docker logs --follow %s\nStop      docker stop %s\n", result["endpoint"], containerName, containerName)
	}
	return nil
}

func workloadControlPlaneCommand(ctx context.Context, action string, args []string) error {
	path := "."
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		path = args[0]
		args = args[1:]
	}
	specPath, _, err := workloadproject.Validate(path)
	if err != nil {
		return err
	}
	cfg, err := config.LoadClient()
	if err != nil {
		return err
	}
	commandArgs := append([]string{specPath}, args...)
	if action == "plan" {
		return planCommand(ctx, cfg, commandArgs)
	}
	return deployAPICommand(ctx, cfg, "deploy", commandArgs)
}

func shellDisplay(value string) string {
	if value == "" {
		return "'.'"
	}
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func workloadLeadingPath(args []string) (string, []string) {
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		return args[0], args[1:]
	}
	return "", args
}
