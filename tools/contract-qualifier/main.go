// contract-qualifier runs hermetic integration contract suites and emits a
// commit-addressed, secret-free JSON manifest. It never contacts real providers.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/infercrane/infercrane/internal/integration"
)

type manifest struct {
	SchemaVersion      int                  `json:"schema_version"`
	Status             string               `json:"status"`
	Commit             string               `json:"commit"`
	Dirty              bool                 `json:"dirty"`
	Environment        string               `json:"environment"`
	StartedAt          time.Time            `json:"started_at"`
	FinishedAt         time.Time            `json:"finished_at"`
	Command            []string             `json:"command"`
	OutputSHA256       string               `json:"output_sha256"`
	Integrations       integration.Snapshot `json:"integrations"`
	RealProvider       string               `json:"real_provider_qualification"`
	RealProviderReason string               `json:"real_provider_reason"`
}

func main() {
	var output string
	var allowDirty bool
	flag.StringVar(&output, "output", "", "manifest output path")
	flag.BoolVar(&allowDirty, "allow-dirty", false, "allow a development worktree; release evidence must remain clean")
	flag.Parse()
	if output == "" {
		fatal(errors.New("--output is required"))
	}
	root, err := git("rev-parse", "--show-toplevel")
	if err != nil {
		fatal(err)
	}
	commit, err := gitAt(root, "rev-parse", "HEAD")
	if err != nil {
		fatal(err)
	}
	status, err := gitAt(root, "status", "--porcelain")
	if err != nil {
		fatal(err)
	}
	dirty := status != ""
	if dirty && !allowDirty {
		fatal(errors.New("contract release qualification requires a clean worktree"))
	}
	registry, err := integration.V02Catalog()
	if err != nil {
		fatal(err)
	}
	command := []string{"go", "test", "-count=1", "./internal/integration", "./internal/conformance", "./internal/provision", "./internal/gateway", "./internal/reconcile", "./internal/workflows"}
	started := time.Now().UTC()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, command[0], command[1:]...)
	cmd.Dir = root
	testOutput, testErr := cmd.CombinedOutput()
	digest := sha256.Sum256(testOutput)
	result := manifest{
		SchemaVersion: 1, Status: "passed", Commit: commit, Dirty: dirty,
		Environment: "hermetic-local", StartedAt: started, FinishedAt: time.Now().UTC(),
		Command: command, OutputSHA256: hex.EncodeToString(digest[:]), Integrations: registry.Snapshot(),
		RealProvider: "deferred", RealProviderReason: "awaiting consolidated v1 manual qualification",
	}
	if testErr != nil {
		result.Status = "failed"
	}
	encoded, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		fatal(err)
	}
	absolute := output
	if !filepath.IsAbs(absolute) {
		absolute = filepath.Join(root, output)
	}
	if err = os.MkdirAll(filepath.Dir(absolute), 0o755); err != nil {
		fatal(err)
	}
	if err = os.WriteFile(absolute, append(encoded, '\n'), 0o644); err != nil {
		fatal(err)
	}
	if testErr != nil {
		_, _ = os.Stderr.Write(testOutput)
		fatal(fmt.Errorf("contract suite failed: %w", testErr))
	}
	fmt.Println(absolute)
}

func git(args ...string) (string, error) { return gitAt("", args...) }

func gitAt(root string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = root
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}
	return strings.TrimSpace(string(output)), nil
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "contract qualification:", err)
	os.Exit(1)
}
