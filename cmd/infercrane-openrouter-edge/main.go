package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/infercrane/infercrane/internal/openrouterprovider"
)

func main() {
	listen := flag.String("listen", env("INFERCRANE_OPENROUTER_LISTEN", ":8080"), "provider edge listen address")
	catalogFile := flag.String("catalog", os.Getenv("INFERCRANE_OPENROUTER_PROVIDER_CATALOG_FILE"), "provider catalog file")
	publicModel := flag.String("public-model", env("INFERCRANE_OPENROUTER_PUBLIC_MODEL", "qwen/qwen3.8-27b"), "public OpenRouter model id")
	upstreamModel := flag.String("upstream-model", env("INFERCRANE_OPENROUTER_UPSTREAM_MODEL", "Qwen/Qwen3.8-27B-FP8"), "model id served by the local runtime")
	upstreamURL := flag.String("upstream-url", env("INFERCRANE_OPENROUTER_UPSTREAM_URL", "http://127.0.0.1:30000"), "local model server URL")
	keyFile := flag.String("api-key-file", os.Getenv("INFERCRANE_OPENROUTER_API_KEY_FILE"), "file containing the inbound provider API key")
	upstreamKeyFile := flag.String("upstream-api-key-file", os.Getenv("INFERCRANE_OPENROUTER_UPSTREAM_API_KEY_FILE"), "optional file containing the runtime API key")
	receiptFile := flag.String("receipt-file", os.Getenv("INFERCRANE_OPENROUTER_RECEIPT_FILE"), "optional owner-only JSONL request receipt file")
	metricsKeyFile := flag.String("metrics-key-file", os.Getenv("INFERCRANE_OPENROUTER_METRICS_KEY_FILE"), "optional owner-only bearer key for the operational metrics endpoint")
	maxInFlight := flag.Int("max-in-flight", 8, "maximum accepted in-flight requests; excess receives HTTP 429")
	minInFlight := flag.Int("min-in-flight", 0, "minimum adaptive admission limit; zero keeps a fixed maximum")
	initialInFlight := flag.Int("initial-in-flight", 0, "initial adaptive admission limit; zero starts at the maximum")
	admissionWindow := flag.Int("admission-window", 32, "completed requests per admission adjustment window")
	targetTTFT := flag.Duration("target-ttft", 0, "streaming TTFT objective used by adaptive admission; zero disables adaptation")
	qualifiedOutputTPS := flag.Float64("qualified-output-tps", 0, "measured SLO-qualified output capacity used for productive-utilization telemetry")
	inputPrice := flag.Float64("input-price-per-million", 0, "public input-token price in USD per million")
	outputPrice := flag.Float64("output-price-per-million", 0, "public output-token price in USD per million")
	gpuHourlyCost := flag.Float64("gpu-hourly-cost", 0, "all-in GPU cost in USD per hour")
	maxPrefillTokens := flag.Int64("max-prefill-tokens-in-flight", 0, "maximum estimated prompt tokens admitted before first output tokens; zero disables this boundary")
	flag.Parse()
	if *catalogFile == "" || *keyFile == "" {
		fatal(errors.New("--catalog and --api-key-file are required"))
	}
	catalog, err := openrouterprovider.Load(*catalogFile)
	if err != nil {
		fatal(err)
	}
	key, err := readSecret(*keyFile)
	if err != nil {
		fatal(fmt.Errorf("read inbound API key: %w", err))
	}
	upstreamKey := ""
	if *upstreamKeyFile != "" {
		upstreamKey, err = readSecret(*upstreamKeyFile)
		if err != nil {
			fatal(fmt.Errorf("read upstream API key: %w", err))
		}
	}
	metricsKey := ""
	if *metricsKeyFile != "" {
		metricsKey, err = readSecret(*metricsKeyFile)
		if err != nil {
			fatal(fmt.Errorf("read metrics API key: %w", err))
		}
	}
	var receiptRecorder *openrouterprovider.JSONLReceiptRecorder
	if *receiptFile != "" {
		receiptRecorder, err = openrouterprovider.NewJSONLReceiptRecorder(*receiptFile)
		if err != nil {
			fatal(fmt.Errorf("open request receipt file: %w", err))
		}
		defer receiptRecorder.Close()
	}
	edge := &openrouterprovider.Edge{
		Catalog: catalog, PublicModel: *publicModel, UpstreamModel: *upstreamModel,
		APIKey: key, UpstreamURL: *upstreamURL, UpstreamKey: upstreamKey,
		MaxInFlight: *maxInFlight, MinInFlight: *minInFlight,
		InitialInFlight: *initialInFlight, AdmissionWindow: *admissionWindow,
		TargetTTFT:                     *targetTTFT,
		QualifiedOutputTokensPerSecond: *qualifiedOutputTPS,
		InputPricePerMillionUSD:        *inputPrice,
		OutputPricePerMillionUSD:       *outputPrice,
		GPUHourlyCostUSD:               *gpuHourlyCost,
		MaxPrefillTokensInFlight:       *maxPrefillTokens,
		MetricsKey:                     metricsKey, ReceiptRecorder: receiptRecorder,
	}
	handler, err := edge.Handler()
	if err != nil {
		fatal(err)
	}
	server := &http.Server{Addr: *listen, Handler: handler, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 90 * time.Second, MaxHeaderBytes: 1 << 20}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}()
	slog.Info("OpenRouter provider edge listening", "address", *listen, "public_model", *publicModel, "initial_in_flight", *initialInFlight, "max_in_flight", *maxInFlight, "target_ttft", targetTTFT.String())
	if err = server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fatal(err)
	}
}

func readSecret(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", errors.New("secret path must be absolute")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Mode()&0o077 != 0 {
		return "", errors.New("secret must be an owner-only regular file")
	}
	value, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	secret := strings.TrimSpace(string(value))
	if secret == "" {
		return "", errors.New("secret file is empty")
	}
	return secret, nil
}

func env(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func fatal(err error) {
	_, _ = fmt.Fprintln(os.Stderr, "infercrane-openrouter-edge:", err)
	os.Exit(1)
}
