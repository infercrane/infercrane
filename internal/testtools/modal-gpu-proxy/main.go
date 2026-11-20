// Command modal-gpu-proxy is a development-only model server pod for the
// Modal GPU PoC (tools/poc-modal). It is never part of the product.
//
// InferCrane's Kubernetes provider starts it as an ordinary model server pod.
// On start it asks the laptop broker (tools/poc-modal/broker.py, which holds
// the developer's Modal login) for one remote GPU server running exactly the
// vLLM arguments after "--". It then serves the runtime contract (/health,
// /v1/*, /metrics) by forwarding every request to that server. Its health is
// the remote server's health. When the pod stops, the lease is released and
// the GPU stops; a lease that is not renewed expires on the remote side too.
//
//	modal-gpu-proxy --broker URL --port 8080 --model NAME -- VLLM_ARGS...
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		slog.Error("modal-gpu-proxy stopped", "error", err)
		os.Exit(1)
	}
}

type options struct {
	broker, model string
	port          int
	serverArgs    []string
}

func parse(args []string) (options, error) {
	flags := flag.NewFlagSet("modal-gpu-proxy", flag.ContinueOnError)
	var parsed options
	flags.StringVar(&parsed.broker, "broker", "", "broker URL")
	flags.IntVar(&parsed.port, "port", 8080, "port the pod serves")
	flags.StringVar(&parsed.model, "model", "", "served model name")
	if err := flags.Parse(args); err != nil {
		return options{}, err
	}
	parsed.serverArgs = flags.Args()
	if parsed.broker == "" || parsed.model == "" || parsed.port <= 0 || parsed.port > 65535 {
		return options{}, errors.New("--broker, --model, and a valid --port are required")
	}
	return parsed, nil
}

func run(args []string) error {
	parsed, err := parse(args)
	if err != nil {
		return err
	}
	workerKey := os.Getenv("INFERCRANE_WORKER_API_KEY")
	if workerKey == "" {
		return errors.New("INFERCRANE_WORKER_API_KEY is required")
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	// One connection pool for the whole measurement: a new TLS handshake per
	// request would add round trips to every measured latency.
	transport := &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		DialContext:         (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:   true,
		MaxIdleConns:        512,
		MaxIdleConnsPerHost: 512,
		IdleConnTimeout:     120 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second,
	}
	proxy := &Proxy{WorkerKey: workerKey, Client: &http.Client{Transport: transport}}
	broker := Broker{BaseURL: parsed.broker, Token: workerKey}
	server := &http.Server{Addr: fmt.Sprintf(":%d", parsed.port), Handler: proxy, ReadHeaderTimeout: 10 * time.Second}
	serveErr := make(chan error, 1)
	go func() { serveErr <- server.ListenAndServe() }()

	pod, _ := os.Hostname()
	lease, err := acquire(ctx, broker, pod, parsed.model, parsed.serverArgs, 10*time.Second)
	if err != nil {
		shutdown(server)
		return err
	}
	if err := proxy.SetLease(lease); err != nil {
		_ = broker.Release(context.Background(), lease.ID)
		shutdown(server)
		return err
	}
	slog.Info("remote GPU server ready", "lease_id", lease.ID, "args", parsed.serverArgs)
	heartbeats := make(chan error, 1)
	go func() { heartbeats <- keepAlive(ctx, broker, lease.ID, 20*time.Second) }()

	var result error
	select {
	case <-ctx.Done():
	case err := <-heartbeats:
		proxy.End(err)
		result = err
	case err := <-serveErr:
		result = err
	}
	shutdown(server)
	releaseCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := broker.Release(releaseCtx, lease.ID); err != nil {
		slog.Warn("release remote GPU lease", "lease_id", lease.ID, "error", err)
	}
	return result
}

func shutdown(server *http.Server) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = server.Shutdown(ctx)
}

// acquire creates a lease and waits until its server is ready. The broker
// may refuse while its GPU or budget limit is reached; the pod then stays
// unready and asks again. A failed server is an error: the pod exits and
// Kubernetes decides whether to try again.
func acquire(ctx context.Context, broker Broker, pod, model string, args []string, every time.Duration) (Lease, error) {
	var lease Lease
	for lease.ID == "" {
		created, err := broker.Create(ctx, pod, model, args)
		if err == nil {
			lease = created
			break
		}
		slog.Warn("remote GPU lease not granted yet", "error", err)
		if waitErr := wait(ctx, every); waitErr != nil {
			return Lease{}, waitErr
		}
	}
	slog.Info("remote GPU lease granted; waiting for the server", "lease_id", lease.ID)
	for {
		current, err := broker.Get(ctx, lease.ID)
		if err == nil {
			// Keep the upstream key from the grant: reads never return it.
			current.APIKey = lease.APIKey
			switch current.State {
			case LeaseReady:
				return current, nil
			case LeaseFailed, LeaseStopped:
				return Lease{}, fmt.Errorf("%w: lease %s %s: %s", ErrLeaseEnded, lease.ID, current.State, current.Error)
			}
			// Starting: renew while the server loads.
			_, _ = broker.Heartbeat(ctx, lease.ID)
		}
		if waitErr := wait(ctx, every); waitErr != nil {
			releaseCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			_ = broker.Release(releaseCtx, lease.ID)
			cancel()
			return Lease{}, waitErr
		}
	}
}

// keepAlive renews the lease until ctx ends. It returns when the broker says
// the lease ended (budget reached, server failed, or stopped elsewhere), or
// when the broker has been unreachable long enough that the remote side has
// expired the lease anyway.
func keepAlive(ctx context.Context, broker Broker, id string, every time.Duration) error {
	lastOK := time.Now()
	for {
		if err := wait(ctx, every); err != nil {
			return nil
		}
		lease, err := broker.Heartbeat(ctx, id)
		switch {
		case err == nil && lease.State == LeaseReady:
			lastOK = time.Now()
		case err == nil:
			return fmt.Errorf("%w: lease %s %s: %s", ErrLeaseEnded, id, lease.State, lease.Error)
		case time.Since(lastOK) > 6*every:
			return fmt.Errorf("%w: broker unreachable for %s: %v", ErrLeaseEnded, time.Since(lastOK).Round(time.Second), err)
		}
	}
}

func wait(ctx context.Context, every time.Duration) error {
	timer := time.NewTimer(every)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
