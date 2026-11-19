package benchmark

import (
	"context"
	"errors"
	"os/exec"
	"syscall"
	"time"
)

// leftoverWait bounds how long a finished AIPerf run may keep its output pipes
// open. AIPerf starts one dataset worker per CPU; when a model server dies
// mid-run, the main process can exit while those workers live on and hold the
// pipes, and waiting for them would block the measurement forever.
const leftoverWait = 30 * time.Second

type executable struct {
	// wait overrides leftoverWait in tests.
	wait time.Duration
}

// Run starts the command in its own process group so that the run, and every
// worker it started, can be stopped together: on cancellation, and after the
// main process has exited.
func (e executable) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	wait := e.wait
	if wait <= 0 {
		wait = leftoverWait
	}
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return killGroup(cmd) }
	cmd.WaitDelay = wait
	output, err := cmd.CombinedOutput()
	// Workers left behind by the run must not outlive it.
	_ = killGroup(cmd)
	if errors.Is(err, exec.ErrWaitDelay) && cmd.ProcessState != nil && cmd.ProcessState.Success() {
		// AIPerf itself finished and its exports are on disk; only leftover
		// workers held the pipes, and they are stopped above.
		err = nil
	}
	return output, err
}

func killGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}
