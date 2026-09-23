package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/infercrane/infercrane/internal/acceleratorlab"
	"github.com/infercrane/infercrane/internal/acceleratorworker"
)

const maxConfigBytes = 1 << 20

type configuration struct {
	listen           string
	tokenFile        string
	catalogFile      string
	receiptDir       string
	artifactDir      string
	publicURL        string
	maxArtifactBytes int64
	maxConcurrent    int
	adapter          string
	adapterEnvFile   string
	tlsCertificate   string
	tlsKey           string
	timeout          time.Duration
}

func main() {
	var config configuration
	flag.StringVar(&config.listen, "listen", "127.0.0.1:8091", "HTTP listen address")
	flag.StringVar(&config.tokenFile, "token-file", "", "owner-only bearer token file")
	flag.StringVar(&config.catalogFile, "catalog-file", "", "worker capability catalog JSON")
	flag.StringVar(&config.receiptDir, "receipt-dir", "", "owner-only durable receipt directory")
	flag.StringVar(&config.artifactDir, "artifact-dir", "", "owner-only content-addressed artifact directory")
	flag.StringVar(&config.publicURL, "public-url", "", "absolute public worker origin used in artifact references")
	flag.Int64Var(&config.maxArtifactBytes, "max-artifact-bytes", 512<<20, "maximum artifact size")
	flag.IntVar(&config.maxConcurrent, "max-concurrent", 1, "maximum concurrent accelerator operations")
	flag.StringVar(&config.adapter, "adapter", "", "absolute provider adapter executable")
	flag.StringVar(&config.adapterEnvFile, "adapter-env-file", "", "optional owner-only JSON object of adapter environment variables")
	flag.StringVar(&config.tlsCertificate, "tls-cert", "", "TLS certificate PEM")
	flag.StringVar(&config.tlsKey, "tls-key", "", "TLS private key PEM")
	flag.DurationVar(&config.timeout, "operation-timeout", 4*time.Hour, "maximum profile/generate/qualify duration")
	flag.Parse()
	if err := run(config); err != nil {
		slog.Error("accelerator worker stopped", "error", err)
		os.Exit(1)
	}
}

func run(config configuration) error {
	if strings.TrimSpace(config.tokenFile) == "" || strings.TrimSpace(config.catalogFile) == "" || strings.TrimSpace(config.receiptDir) == "" || strings.TrimSpace(config.artifactDir) == "" || strings.TrimSpace(config.publicURL) == "" || strings.TrimSpace(config.adapter) == "" {
		return errors.New("token-file, catalog-file, receipt-dir, artifact-dir, public-url, and adapter are required")
	}
	if config.maxConcurrent < 1 || config.maxConcurrent > 1024 {
		return errors.New("max-concurrent must be between 1 and 1024")
	}
	token, err := readProtectedFile(config.tokenFile, true)
	if err != nil {
		return fmt.Errorf("read service token: %w", err)
	}
	catalogData, err := readProtectedFile(config.catalogFile, false)
	if err != nil {
		return fmt.Errorf("read capability catalog: %w", err)
	}
	var catalog acceleratorlab.CapabilityCatalog
	if err = decodeStrict(catalogData, &catalog); err != nil {
		return fmt.Errorf("decode capability catalog: %w", err)
	}
	if err = catalog.Validate(); err != nil {
		return err
	}
	var environment []string
	if strings.TrimSpace(config.adapterEnvFile) != "" {
		data, readErr := readProtectedFile(config.adapterEnvFile, true)
		if readErr != nil {
			return fmt.Errorf("read adapter environment: %w", readErr)
		}
		var values map[string]string
		if decodeErr := decodeStrict(data, &values); decodeErr != nil {
			return fmt.Errorf("decode adapter environment: %w", decodeErr)
		}
		for name, value := range values {
			if name == "" || strings.ContainsAny(name, "=\x00") || strings.ContainsRune(value, '\x00') {
				return errors.New("adapter environment contains an invalid entry")
			}
			environment = append(environment, name+"="+value)
		}
	}
	adapter, err := acceleratorworker.NewCommandAdapter(acceleratorworker.CommandAdapterConfig{
		Executable: config.adapter, Environment: environment, OperationTimeout: config.timeout,
	})
	if err != nil {
		return err
	}
	receipts, err := acceleratorworker.NewFileReceiptStore(config.receiptDir)
	if err != nil {
		return err
	}
	artifacts, err := acceleratorworker.NewLocalArtifactStore(config.artifactDir, config.publicURL, config.maxArtifactBytes)
	if err != nil {
		return err
	}
	worker := &acceleratorworker.Handler{
		Token: strings.TrimSpace(string(token)), Catalog: catalog, Profiler: adapter,
		Generator: adapter, Qualifier: adapter, Receipts: receipts, Artifacts: artifacts,
		MaxArtifactBytes: config.maxArtifactBytes, MaxConcurrent: config.maxConcurrent,
		ReportError: func(_ context.Context, operationErr error) {
			slog.Error("accelerator operation failed", "error", operationErr)
		},
	}
	if err = worker.Validate(); err != nil {
		return err
	}
	mux := http.NewServeMux()
	mux.Handle("/v1/", worker)
	mux.HandleFunc("GET /healthz", func(response http.ResponseWriter, _ *http.Request) {
		response.Header().Set("Cache-Control", "no-store")
		response.WriteHeader(http.StatusNoContent)
	})
	server := &http.Server{
		Addr: config.listen, Handler: mux, ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout: 90 * time.Second, MaxHeaderBytes: 1 << 20,
		TLSConfig: &tls.Config{MinVersion: tls.VersionTLS13},
	}
	if err = validateTransport(config); err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	serverErrors := make(chan error, 1)
	go func() {
		if config.tlsCertificate != "" {
			serverErrors <- server.ListenAndServeTLS(config.tlsCertificate, config.tlsKey)
			return
		}
		serverErrors <- server.ListenAndServe()
	}()
	slog.Info("accelerator worker listening", "address", config.listen, "capability_entries", len(catalog.Capabilities))
	select {
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		return server.Shutdown(shutdown)
	case serveErr := <-serverErrors:
		if errors.Is(serveErr, http.ErrServerClosed) {
			return nil
		}
		return serveErr
	}
}

func validateTransport(config configuration) error {
	certificate := strings.TrimSpace(config.tlsCertificate)
	key := strings.TrimSpace(config.tlsKey)
	if (certificate == "") != (key == "") {
		return errors.New("tls-cert and tls-key must be configured together")
	}
	host, _, err := net.SplitHostPort(config.listen)
	if err != nil {
		return fmt.Errorf("invalid listen address: %w", err)
	}
	if certificate == "" && !loopbackHost(host) {
		return errors.New("non-loopback accelerator workers require TLS")
	}
	return nil
}

func loopbackHost(host string) bool {
	host = strings.Trim(host, "[]")
	return strings.EqualFold(host, "localhost") || net.ParseIP(host) != nil && net.ParseIP(host).IsLoopback()
}

func readProtectedFile(path string, ownerOnly bool) ([]byte, error) {
	path = filepath.Clean(strings.TrimSpace(path))
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() < 1 || info.Size() > maxConfigBytes {
		return nil, errors.New("file must be a bounded regular file")
	}
	if ownerOnly && info.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("secret file must be owner-only")
	}
	if !ownerOnly && info.Mode().Perm()&0o022 != 0 {
		return nil, errors.New("configuration file must not be group/world writable")
	}
	return os.ReadFile(path)
}

func decodeStrict(data []byte, output any) error {
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(output); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("file contains trailing JSON")
	}
	return nil
}
