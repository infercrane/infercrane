package provision

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestCachedImageBootstrapUsesPresentDigestBeforePull(t *testing.T) {
	binDir := t.TempDir()
	logFile := filepath.Join(binDir, "docker.log")
	fakeDocker := filepath.Join(binDir, "docker")
	if err := os.WriteFile(fakeDocker, []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" >>\"$DOCKER_LOG\"\nif [ \"$1 $2\" = 'image inspect' ]; then [ \"${IMAGE_PRESENT:-0}\" = 1 ]; exit; fi\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	image := "registry.example/runtime@sha256:" + strings.Repeat("a", 64)
	run := func(present bool) string {
		t.Helper()
		if err := os.WriteFile(logFile, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		command := exec.Command("sh", "-c", "infercrane_stage() { :; };\n"+cachedImageBootstrap(image))
		command.Env = append(os.Environ(), "PATH="+binDir+":"+os.Getenv("PATH"), "DOCKER_LOG="+logFile)
		if present {
			command.Env = append(command.Env, "IMAGE_PRESENT=1")
		}
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("bootstrap failed: %v: %s", err, output)
		}
		logged, err := os.ReadFile(logFile)
		if err != nil {
			t.Fatal(err)
		}
		return string(logged)
	}
	if logged := run(true); !strings.Contains(logged, "image inspect "+image) || strings.Contains(logged, "pull ") {
		t.Fatalf("present image was not reused: %q", logged)
	}
	if logged := run(false); !strings.Contains(logged, "image inspect "+image) || !strings.Contains(logged, "pull "+image) {
		t.Fatalf("missing image was not pulled: %q", logged)
	}
}
