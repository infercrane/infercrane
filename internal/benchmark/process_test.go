package benchmark

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// leftoverScript starts a background worker that inherits the output pipe, as
// AIPerf's dataset workers do, records its PID, and then runs body.
func leftoverScript(t *testing.T, body string) (string, string) {
	t.Helper()
	pidFile := filepath.Join(t.TempDir(), "worker.pid")
	return "sleep 30 & echo $! > " + pidFile + "; " + body, pidFile
}

func workerPID(t *testing.T, pidFile string) int {
	t.Helper()
	raw, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("read worker pid: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatalf("parse worker pid %q: %v", raw, err)
	}
	return pid
}

func requireStopped(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
	t.Fatalf("worker %d still running after the run returned", pid)
}

func TestExecutableReturnsWhenWorkersOutliveASuccessfulRun(t *testing.T) {
	script, pidFile := leftoverScript(t, "echo done")
	started := time.Now()
	output, err := executable{wait: 200 * time.Millisecond}.Run(context.Background(), "sh", "-c", script)
	if err != nil {
		t.Fatalf("run with leftover worker: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 10*time.Second {
		t.Fatalf("run waited %s for a leftover worker", elapsed)
	}
	if !strings.Contains(string(output), "done") {
		t.Fatalf("output = %q, want the run's own output", output)
	}
	requireStopped(t, workerPID(t, pidFile))
}

func TestExecutableKeepsTheExitErrorWhenWorkersOutliveAFailedRun(t *testing.T) {
	script, pidFile := leftoverScript(t, "exit 3")
	_, err := executable{wait: 200 * time.Millisecond}.Run(context.Background(), "sh", "-c", script)
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 3 {
		t.Fatalf("err = %v, want exit status 3", err)
	}
	requireStopped(t, workerPID(t, pidFile))
}

func TestExecutableStopsEveryWorkerWhenCancelled(t *testing.T) {
	script, pidFile := leftoverScript(t, "sleep 30")
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := executable{wait: 200 * time.Millisecond}.Run(ctx, "sh", "-c", script)
	if err == nil {
		t.Fatal("cancelled run returned no error")
	}
	if elapsed := time.Since(started); elapsed > 10*time.Second {
		t.Fatalf("cancelled run took %s to return", elapsed)
	}
	requireStopped(t, workerPID(t, pidFile))
}
