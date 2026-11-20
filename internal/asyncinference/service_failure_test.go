package asyncinference

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
)

// failureStore hands out one job and keeps the message the worker fails it with.
type failureStore struct {
	failingLeaseStore
	mu      sync.Mutex
	job     *domain.AsyncInferenceJob
	message string
	failed  chan struct{}
}

func (f *failureStore) ClaimAsyncInferenceJob(context.Context, string, string, time.Duration) (domain.AsyncInferenceJob, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.job == nil {
		return domain.AsyncInferenceJob{}, errors.New("no job")
	}
	job := *f.job
	f.job = nil
	return job, nil
}

func (f *failureStore) FailAsyncInferenceJob(_ context.Context, _, _, _, code, message string, _ bool) error {
	f.mu.Lock()
	f.message = code + ": " + message
	f.mu.Unlock()
	close(f.failed)
	return nil
}

func TestAsyncFailureMessageNamesNoInternalAddress(t *testing.T) {
	closed := httptest.NewServer(http.NotFoundHandler())
	closedURL := closed.URL
	closed.Close()
	unavailable := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "Request failed: error sending request for url (http://worker.infercrane-system.svc:8080/v1/chat/completions)", http.StatusServiceUnavailable)
	}))
	defer unavailable.Close()
	for name, test := range map[string]struct {
		gatewayURL, want, logged string
	}{
		"gateway unreachable": {closedURL, "execution_failed: async inference execution failed", strings.TrimPrefix(closedURL, "http://")},
		"gateway answered":    {unavailable.URL, "execution_failed: gateway returned HTTP 503", "gateway returned HTTP 503"},
	} {
		t.Run(name, func(t *testing.T) {
			cipher, err := NewCipher("01234567890123456789012345678901")
			if err != nil {
				t.Fatal(err)
			}
			payload, nonce, err := cipher.Encrypt([]byte(`{"model":"alias","messages":[]}`), []byte("tenant/job-1"))
			if err != nil {
				t.Fatal(err)
			}
			store := &failureStore{failed: make(chan struct{}), job: &domain.AsyncInferenceJob{ID: "job-1", TenantID: "tenant", RequestID: "req-1", Protocol: "chat", PayloadCiphertext: payload, PayloadNonce: nonce, Attempt: 1, ExecutionDeadline: time.Now().Add(time.Minute)}}
			logs := &bytes.Buffer{}
			var logsMu sync.Mutex
			logger := slog.New(slog.NewTextHandler(writerFunc(func(p []byte) (int, error) {
				logsMu.Lock()
				defer logsMu.Unlock()
				return logs.Write(p)
			}), nil))
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				done <- Service{Store: store, Cipher: cipher, GatewayURL: test.gatewayURL, APIKey: "secret", Lease: time.Minute, Logger: logger}.Run(ctx)
			}()
			select {
			case <-store.failed:
			case <-time.After(5 * time.Second):
				t.Fatal("job was never failed")
			}
			cancel()
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			store.mu.Lock()
			message := store.message
			store.mu.Unlock()
			if message != test.want {
				t.Fatalf("error_message=%q want %q", message, test.want)
			}
			for _, text := range []string{"http://", "127.0.0.1", "svc", "dial tcp"} {
				if strings.Contains(message, text) {
					t.Fatalf("error_message %q names %q", message, text)
				}
			}
			logsMu.Lock()
			logged := logs.String()
			logsMu.Unlock()
			if !strings.Contains(logged, "async inference attempt failed") || !strings.Contains(logged, "job_id=job-1") || !strings.Contains(logged, test.logged) {
				t.Fatalf("failure detail missing from log=%s", logged)
			}
		})
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }
