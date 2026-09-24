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
	receiptLedgerURL := flag.String("receipt-ledger-url", os.Getenv("INFERCRANE_MARKETPLACE_LEDGER_URL"), "optional durable InferCrane marketplace receipt ledger URL")
	receiptLedgerTokenFile := flag.String("receipt-ledger-token-file", os.Getenv("INFERCRANE_MARKETPLACE_LEDGER_TOKEN_FILE"), "optional owner-only marketplace ledger bearer token file")
	metricsKeyFile := flag.String("metrics-key-file", os.Getenv("INFERCRANE_OPENROUTER_METRICS_KEY_FILE"), "optional owner-only bearer key for the operational metrics endpoint")
	huggingFaceKeyFile := flag.String("huggingface-api-key-file", os.Getenv("INFERCRANE_HUGGINGFACE_API_KEY_FILE"), "optional owner-only Hugging Face provider API key file")
	requestyKeyFile := flag.String("requesty-api-key-file", os.Getenv("INFERCRANE_REQUESTY_API_KEY_FILE"), "optional owner-only Requesty provider API key file")
	vercelKeyFile := flag.String("vercel-api-key-file", os.Getenv("INFERCRANE_VERCEL_API_KEY_FILE"), "optional owner-only Vercel provider API key file")
	openCodeKeyFile := flag.String("opencode-api-key-file", os.Getenv("INFERCRANE_OPENCODE_API_KEY_FILE"), "optional owner-only OpenCode Zen provider API key file")
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
	huggingFaceMaxInFlight := flag.Int("huggingface-max-in-flight", 3, "maximum concurrent Hugging Face requests; zero shares the global limit")
	requestyMaxInFlight := flag.Int("requesty-max-in-flight", 2, "maximum concurrent Requesty requests; zero shares the global limit")
	vercelMaxInFlight := flag.Int("vercel-max-in-flight", 2, "maximum concurrent Vercel requests; zero shares the global limit")
	openCodeMaxInFlight := flag.Int("opencode-max-in-flight", 2, "maximum concurrent OpenCode Zen requests; zero shares the global limit")
	huggingFaceInputPrice := flag.Float64("huggingface-input-price-per-million", 0, "Hugging Face input price in USD per million; zero inherits the catalog price")
	huggingFaceOutputPrice := flag.Float64("huggingface-output-price-per-million", 0, "Hugging Face output price in USD per million; zero inherits the catalog price")
	requestyInputPrice := flag.Float64("requesty-input-price-per-million", 0, "Requesty input price in USD per million; zero inherits the catalog price")
	requestyOutputPrice := flag.Float64("requesty-output-price-per-million", 0, "Requesty output price in USD per million; zero inherits the catalog price")
	vercelInputPrice := flag.Float64("vercel-input-price-per-million", 0, "Vercel input price in USD per million; zero inherits the catalog price")
	vercelOutputPrice := flag.Float64("vercel-output-price-per-million", 0, "Vercel output price in USD per million; zero inherits the catalog price")
	openCodeInputPrice := flag.Float64("opencode-input-price-per-million", 0, "OpenCode Zen input price in USD per million; zero inherits the catalog price")
	openCodeOutputPrice := flag.Float64("opencode-output-price-per-million", 0, "OpenCode Zen output price in USD per million; zero inherits the catalog price")
	flag.Parse()
	if *catalogFile == "" {
		fatal(errors.New("--catalog is required"))
	}
	if *keyFile == "" && strings.TrimSpace(os.Getenv("INFERCRANE_OPENROUTER_API_KEY")) == "" {
		fatal(errors.New("--api-key-file or INFERCRANE_OPENROUTER_API_KEY is required"))
	}
	catalog, err := openrouterprovider.Load(*catalogFile)
	if err != nil {
		fatal(err)
	}
	key, err := readSecretOrEnvironment(*keyFile, "INFERCRANE_OPENROUTER_API_KEY")
	if err != nil {
		fatal(fmt.Errorf("read inbound API key: %w", err))
	}
	upstreamKey := ""
	if *upstreamKeyFile != "" || os.Getenv("INFERCRANE_OPENROUTER_UPSTREAM_API_KEY") != "" {
		upstreamKey, err = readSecretOrEnvironment(*upstreamKeyFile, "INFERCRANE_OPENROUTER_UPSTREAM_API_KEY")
		if err != nil {
			fatal(fmt.Errorf("read upstream API key: %w", err))
		}
	}
	metricsKey := ""
	if *metricsKeyFile != "" || os.Getenv("INFERCRANE_OPENROUTER_METRICS_KEY") != "" {
		metricsKey, err = readSecretOrEnvironment(*metricsKeyFile, "INFERCRANE_OPENROUTER_METRICS_KEY")
		if err != nil {
			fatal(fmt.Errorf("read metrics API key: %w", err))
		}
	}
	credentials := []openrouterprovider.ChannelCredential{{Channel: openrouterprovider.ChannelOpenRouter, APIKey: key}}
	for _, optional := range []struct {
		channel         string
		path            string
		environmentName string
	}{
		{openrouterprovider.ChannelHuggingFace, *huggingFaceKeyFile, "INFERCRANE_HUGGINGFACE_API_KEY"},
		{openrouterprovider.ChannelRequesty, *requestyKeyFile, "INFERCRANE_REQUESTY_API_KEY"},
		{openrouterprovider.ChannelVercel, *vercelKeyFile, "INFERCRANE_VERCEL_API_KEY"},
		{openrouterprovider.ChannelOpenCode, *openCodeKeyFile, "INFERCRANE_OPENCODE_API_KEY"},
	} {
		optionalKey, present, credentialErr := readOptionalSecretOrEnvironment(optional.path, optional.environmentName)
		if credentialErr != nil {
			fatal(fmt.Errorf("read %s inbound API key: %w", optional.channel, credentialErr))
		}
		if present {
			credentials = append(credentials, openrouterprovider.ChannelCredential{Channel: optional.channel, APIKey: optionalKey})
		}
	}
	if *receiptFile != "" && *receiptLedgerURL != "" {
		fatal(errors.New("configure either --receipt-file or --receipt-ledger-url, not both"))
	}
	if hasChannel(credentials, openrouterprovider.ChannelHuggingFace) && *receiptFile == "" && *receiptLedgerURL == "" {
		fatal(errors.New("--receipt-file or --receipt-ledger-url is required when the Hugging Face marketplace channel is enabled"))
	}
	var receiptRecorder openrouterprovider.ReceiptRecorder
	if *receiptFile != "" {
		fileRecorder, recorderErr := openrouterprovider.NewJSONLReceiptRecorder(*receiptFile)
		err = recorderErr
		if err != nil {
			fatal(fmt.Errorf("open request receipt file: %w", err))
		}
		receiptRecorder = fileRecorder
		defer fileRecorder.Close()
	}
	if *receiptLedgerURL != "" {
		ledgerToken, tokenErr := readSecretOrEnvironment(*receiptLedgerTokenFile, "INFERCRANE_MARKETPLACE_LEDGER_TOKEN")
		if tokenErr != nil {
			fatal(fmt.Errorf("read marketplace ledger token: %w", tokenErr))
		}
		remoteLedger, ledgerErr := openrouterprovider.NewHTTPReceiptLedger(*receiptLedgerURL, ledgerToken, nil)
		if ledgerErr != nil {
			fatal(fmt.Errorf("configure marketplace receipt ledger: %w", ledgerErr))
		}
		receiptRecorder = remoteLedger
	}
	edge := &openrouterprovider.Edge{
		Catalog: catalog, PublicModel: *publicModel, UpstreamModel: *upstreamModel,
		Credentials: credentials, UpstreamURL: *upstreamURL, UpstreamKey: upstreamKey,
		ChannelPolicies: map[string]openrouterprovider.ChannelPolicy{
			openrouterprovider.ChannelHuggingFace: {MaxInFlight: *huggingFaceMaxInFlight, InputPricePerMillionUSD: *huggingFaceInputPrice, OutputPricePerMillionUSD: *huggingFaceOutputPrice, BillingLookupEnabled: true},
			openrouterprovider.ChannelRequesty:    {MaxInFlight: *requestyMaxInFlight, InputPricePerMillionUSD: *requestyInputPrice, OutputPricePerMillionUSD: *requestyOutputPrice},
			openrouterprovider.ChannelVercel:      {MaxInFlight: *vercelMaxInFlight, InputPricePerMillionUSD: *vercelInputPrice, OutputPricePerMillionUSD: *vercelOutputPrice},
			openrouterprovider.ChannelOpenCode:    {MaxInFlight: *openCodeMaxInFlight, InputPricePerMillionUSD: *openCodeInputPrice, OutputPricePerMillionUSD: *openCodeOutputPrice},
		},
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
	slog.Info("marketplace provider edge listening", "address", *listen, "public_model", *publicModel, "channels", len(credentials), "initial_in_flight", *initialInFlight, "max_in_flight", *maxInFlight, "target_ttft", targetTTFT.String())
	if err = server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fatal(err)
	}
}

func readSecretOrEnvironment(path, environmentName string) (string, error) {
	if path != "" {
		return readSecret(path)
	}
	value := strings.TrimSpace(os.Getenv(environmentName))
	_ = os.Unsetenv(environmentName)
	if value == "" {
		return "", errors.New("secret file or environment value is required")
	}
	return value, nil
}

func readOptionalSecretOrEnvironment(path, environmentName string) (string, bool, error) {
	if path == "" && strings.TrimSpace(os.Getenv(environmentName)) == "" {
		return "", false, nil
	}
	value, err := readSecretOrEnvironment(path, environmentName)
	return value, err == nil, err
}

func hasChannel(credentials []openrouterprovider.ChannelCredential, channel string) bool {
	for _, credential := range credentials {
		if credential.Channel == channel {
			return true
		}
	}
	return false
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
