// Command infercrane-specswitch serves one model on one GPU from several vLLM
// engines, one per speculation setup, and keeps awake the engine whose setup
// suits the present load (internal/specswitch).
//
// It starts every engine in turn, puts all but the first band's engine to
// sleep, then serves the OpenAI-compatible API on the configured address.
// Each engine must listen on 127.0.0.1 and run with --enable-sleep-mode; the
// command sets VLLM_SERVER_DEV_MODE=1 for the sleep routes, which is why no
// engine may listen beyond the loopback interface. If an engine process
// exits, the command stops the others and exits non-zero.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/infercrane/infercrane/internal/specswitch"
)

const maxConfigBytes = 1 << 20

func main() {
	if err := run(context.Background(), os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "infercrane-specswitch:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("infercrane-specswitch", flag.ContinueOnError)
	configPath := flags.String("config", "", "path of the switch configuration (JSON)")
	configJSON := flags.String("config-json", "", "the switch configuration as one JSON argument")
	startTimeout := flags.Duration("start-timeout", 30*time.Minute, "how long one engine may take to start")
	checkOnly := flags.Bool("check", false, "validate the configuration and the engine commands, then exit")
	if err := flags.Parse(args); err != nil {
		return err
	}
	cfg, err := loadConfig(*configPath, *configJSON)
	if err != nil {
		return err
	}
	for _, engine := range cfg.Engines {
		if err = checkEngineCommand(engine); err != nil {
			return err
		}
	}
	if *checkOnly {
		_, err = fmt.Fprintln(stdout, "configuration and engine commands are valid")
		return err
	}
	logger := slog.New(slog.NewJSONHandler(stdout, nil))
	signalled, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithCancelCause(signalled)
	defer cancel(nil)

	group := &engineGroup{logger: logger, out: stdout, exited: cancel}
	defer group.stop(30 * time.Second)
	if err = group.start(ctx, cfg, *startTimeout); err != nil {
		return err
	}
	sw, err := specswitch.New(cfg, specswitch.VLLM{}, specswitch.WithLogger(logger))
	if err != nil {
		return err
	}
	servers := []*http.Server{{Addr: cfg.Listen, Handler: sw.Handler(), ReadHeaderTimeout: 30 * time.Second}}
	if cfg.AdminListen != "" {
		servers = append(servers, &http.Server{Addr: cfg.AdminListen, Handler: sw.AdminHandler(), ReadHeaderTimeout: 10 * time.Second})
	}
	failed := make(chan error, len(servers))
	for _, server := range servers {
		listener, listenErr := net.Listen("tcp", server.Addr)
		if listenErr != nil {
			return listenErr
		}
		go func() {
			if serveErr := server.Serve(listener); !errors.Is(serveErr, http.ErrServerClosed) {
				failed <- serveErr
			}
		}()
	}
	logger.Info("speculation switch serving", "listen", cfg.Listen, "awake", specswitch.InitialEngine(cfg), "policy", cfg.Policy)
	go sw.Run(ctx)
	select {
	case <-ctx.Done():
		err = context.Cause(ctx)
	case err = <-failed:
	}
	shutdown, done := context.WithTimeout(context.Background(), 30*time.Second)
	defer done()
	for _, server := range servers {
		_ = server.Shutdown(shutdown)
	}
	if signalled.Err() != nil {
		// Asked to stop: a clean exit.
		return nil
	}
	return err
}

func loadConfig(path, inline string) (specswitch.Config, error) {
	var raw []byte
	switch {
	case path != "" && inline != "":
		return specswitch.Config{}, errors.New("give --config or --config-json, not both")
	case path != "":
		file, err := os.Open(path)
		if err != nil {
			return specswitch.Config{}, err
		}
		defer file.Close()
		raw, err = io.ReadAll(io.LimitReader(file, maxConfigBytes+1))
		if err != nil {
			return specswitch.Config{}, err
		}
	case inline != "":
		raw = []byte(inline)
	default:
		return specswitch.Config{}, errors.New("usage: infercrane-specswitch --config FILE | --config-json JSON")
	}
	if len(raw) > maxConfigBytes {
		return specswitch.Config{}, errors.New("configuration exceeds 1 MiB")
	}
	var cfg specswitch.Config
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cfg); err != nil {
		return specswitch.Config{}, fmt.Errorf("decode configuration: %w", err)
	}
	return cfg.Normalize()
}

// checkEngineCommand refuses an engine command that would expose the sleep
// and development routes beyond the loopback interface, serve on another
// port than the configuration says, or lack sleep mode.
func checkEngineCommand(engine specswitch.Engine) error {
	if len(engine.Command) == 0 {
		return fmt.Errorf("engine %s: a command is required", engine.Name)
	}
	target, err := url.Parse(engine.URL)
	if err != nil {
		return err
	}
	host, port, err := net.SplitHostPort(target.Host)
	if err != nil {
		return fmt.Errorf("engine %s: url needs a port: %w", engine.Name, err)
	}
	value := func(flag string) (string, bool) {
		for index, arg := range engine.Command {
			if arg == flag && index+1 < len(engine.Command) {
				return engine.Command[index+1], true
			}
			if after, ok := strings.CutPrefix(arg, flag+"="); ok {
				return after, true
			}
		}
		return "", false
	}
	if got, ok := value("--host"); !ok || got != host {
		return fmt.Errorf("engine %s: the command must pass --host %s", engine.Name, host)
	}
	if got, ok := value("--port"); !ok || got != port {
		return fmt.Errorf("engine %s: the command must pass --port %s", engine.Name, port)
	}
	if !slices.Contains(engine.Command, "--enable-sleep-mode") {
		return fmt.Errorf("engine %s: the command must pass --enable-sleep-mode", engine.Name)
	}
	return nil
}

// process is one engine process and a channel closed when it has exited.
type process struct {
	command *exec.Cmd
	done    chan struct{}
}

// engineGroup owns the engine processes.
type engineGroup struct {
	logger *slog.Logger
	out    io.Writer
	exited context.CancelCauseFunc

	mu       sync.Mutex
	commands []process
	stopping bool
	outMu    sync.Mutex
}

// start launches the engines one at a time, so each measures the GPU memory
// it may take with the others asleep (specswitch.StartOrder): engines with a
// memory share of their own first, then those whose command takes the share
// measured from the first ones asleep. Every engine but the last is put to
// sleep; then the first band's engine is woken if it is not the last.
func (g *engineGroup) start(ctx context.Context, cfg specswitch.Config, timeout time.Duration) error {
	order := specswitch.StartOrder(cfg)
	control := specswitch.VLLM{}
	share := 0.0
	for index, engine := range order {
		started := time.Now()
		if slices.ContainsFunc(engine.Command, func(arg string) bool { return strings.Contains(arg, specswitch.MemoryShareToken) }) {
			if share == 0 {
				var err error
				if share, err = g.measureShare(ctx, cfg, index); err != nil {
					return err
				}
			}
			engine.Command = specswitch.WithShare(engine.Command, share)
		}
		if err := g.launch(engine); err != nil {
			return err
		}
		stepCtx, cancel := context.WithTimeout(ctx, timeout)
		err := control.WaitHealthy(stepCtx, engine)
		if err == nil {
			err = control.Ready(stepCtx, engine)
		}
		if err == nil && index < len(order)-1 {
			err = control.Sleep(stepCtx, engine)
		}
		cancel()
		if err != nil {
			return fmt.Errorf("start %s: %w", engine.Name, errors.Join(err, context.Cause(ctx)))
		}
		g.logger.Info("engine ready", "engine", engine.Name, "drafts", engine.Drafts, "seconds", time.Since(started).Seconds(), "asleep", index < len(order)-1)
	}
	// The last engine started is awake; the first band's must be.
	last, initial := order[len(order)-1], specswitch.InitialEngine(cfg)
	if last.Name == initial {
		return nil
	}
	awake := cfg.Engines[slices.IndexFunc(cfg.Engines, func(engine specswitch.Engine) bool { return engine.Name == initial })]
	stepCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	started := time.Now()
	err := control.Sleep(stepCtx, last)
	if err == nil {
		err = control.Wake(stepCtx, awake)
	}
	if err == nil {
		err = control.Ready(stepCtx, awake)
	}
	if err != nil {
		return fmt.Errorf("wake %s after the start: %w", initial, errors.Join(err, context.Cause(ctx)))
	}
	g.logger.Info("engine awake", "engine", initial, "after", last.Name, "seconds", time.Since(started).Seconds())
	return nil
}

// measureShare reads the GPU's used memory while the engines started so far
// sleep and computes the share the others may take.
func (g *engineGroup) measureShare(ctx context.Context, cfg specswitch.Config, asleep int) (float64, error) {
	probeCtx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	output, err := exec.CommandContext(probeCtx, cfg.Memory.Probe[0], cfg.Memory.Probe[1:]...).Output()
	if err != nil {
		return 0, fmt.Errorf("memory probe: %w", err)
	}
	used, total, err := specswitch.ParseProbe(string(output))
	if err != nil {
		return 0, err
	}
	share, err := specswitch.MemoryShare(used, total, asleep, len(cfg.Engines)-1, cfg.Memory.MarginMiB, cfg.Memory.MaxShare)
	if err != nil {
		return 0, err
	}
	g.logger.Info("memory share", "used_mib", used, "total_mib", total, "asleep", asleep, "kept_each_mib", used/float64(asleep),
		"margin_mib", cfg.Memory.MarginMiB, "share", share)
	return share, nil
}

func (g *engineGroup) launch(engine specswitch.Engine) error {
	command := exec.Command(engine.Command[0], engine.Command[1:]...)
	command.Env = append(os.Environ(), "VLLM_SERVER_DEV_MODE=1")
	for key, value := range engine.Env {
		command.Env = append(command.Env, key+"="+value)
	}
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	output, err := command.StdoutPipe()
	if err != nil {
		return err
	}
	command.Stderr = command.Stdout
	if err = command.Start(); err != nil {
		return fmt.Errorf("start %s: %w", engine.Name, err)
	}
	done := make(chan struct{})
	g.mu.Lock()
	g.commands = append(g.commands, process{command: command, done: done})
	g.mu.Unlock()
	go g.copyLines(engine.Name, output)
	go func() {
		defer close(done)
		err := command.Wait()
		g.mu.Lock()
		stopping := g.stopping
		g.mu.Unlock()
		if !stopping {
			g.exited(fmt.Errorf("engine %s exited: %v", engine.Name, err))
		}
	}()
	return nil
}

// copyLines writes an engine's output with its name in front of each line.
func (g *engineGroup) copyLines(name string, output io.Reader) {
	scanner := bufio.NewScanner(output)
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	for scanner.Scan() {
		g.outMu.Lock()
		fmt.Fprintf(g.out, "[%s] %s\n", name, scanner.Text())
		g.outMu.Unlock()
	}
}

// stop sends SIGTERM to every engine's process group and SIGKILL to those
// still running after grace.
func (g *engineGroup) stop(grace time.Duration) {
	g.mu.Lock()
	g.stopping = true
	processes := append([]process(nil), g.commands...)
	g.mu.Unlock()
	for _, p := range processes {
		_ = syscall.Kill(-p.command.Process.Pid, syscall.SIGTERM)
	}
	deadline := time.After(grace)
	expired := false
	for _, p := range processes {
		if !expired {
			select {
			case <-p.done:
			case <-deadline:
				expired = true
			}
		}
		// The engine's own children share its process group.
		_ = syscall.Kill(-p.command.Process.Pid, syscall.SIGKILL)
	}
}
