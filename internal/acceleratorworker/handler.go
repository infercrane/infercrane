// Package acceleratorworker exposes the target-accelerator execution boundary
// consumed by acceleratorlab.WorkerClient. The control plane owns policy and
// promotion; this service owns exact-hardware profiling and qualification.
package acceleratorworker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"

	"github.com/infercrane/infercrane/internal/acceleratorlab"
)

const maxRequestBytes = 8 << 20

type Receipt struct {
	RequestDigest string          `json:"request_digest"`
	Response      json.RawMessage `json:"response"`
}

type ReceiptStore interface {
	Get(context.Context, string) (Receipt, bool, error)
	Put(context.Context, string, Receipt) error
}

type MemoryReceiptStore struct {
	mu      sync.RWMutex
	entries map[string]Receipt
}

func NewMemoryReceiptStore() *MemoryReceiptStore {
	return &MemoryReceiptStore{entries: map[string]Receipt{}}
}

func (s *MemoryReceiptStore) Get(_ context.Context, key string) (Receipt, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	receipt, found := s.entries[key]
	receipt.Response = append(json.RawMessage(nil), receipt.Response...)
	return receipt, found, nil
}

func (s *MemoryReceiptStore) Put(_ context.Context, key string, receipt Receipt) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if current, found := s.entries[key]; found && current.RequestDigest != receipt.RequestDigest {
		return errors.New("idempotency key was already used for a different request")
	}
	receipt.Response = append(json.RawMessage(nil), receipt.Response...)
	s.entries[key] = receipt
	return nil
}

type Handler struct {
	Token            string
	Catalog          acceleratorlab.CapabilityCatalog
	Profiler         acceleratorlab.Profiler
	Generator        acceleratorlab.Generator
	Qualifier        acceleratorlab.Qualifier
	Receipts         ReceiptStore
	Artifacts        ArtifactStore
	MaxArtifactBytes int64
	MaxConcurrent    int
	ReportError      func(context.Context, error)
	lockMu           sync.Mutex
	keyLocks         map[string]*keyLock
	semaphoreOnce    sync.Once
	semaphore        chan struct{}
}

type keyLock struct {
	mu   sync.Mutex
	refs int
}

func (h *Handler) Validate() error {
	if strings.TrimSpace(h.Token) == "" || h.Profiler == nil || h.Qualifier == nil || h.Receipts == nil {
		return errors.New("accelerator worker requires token, profiler, qualifier, and receipt store")
	}
	if err := h.Catalog.Validate(); err != nil {
		return fmt.Errorf("accelerator worker catalog: %w", err)
	}
	for _, capability := range h.Catalog.Capabilities {
		if capability.GeneratedKernels && h.Artifacts == nil {
			return errors.New("generated-kernel capability requires an artifact store")
		}
	}
	return nil
}

func (h *Handler) ServeHTTP(response http.ResponseWriter, request *http.Request) {
	if err := h.Validate(); err != nil {
		writeError(response, http.StatusServiceUnavailable, "worker_not_ready")
		return
	}
	if !h.authorized(request.Header.Get("Authorization")) {
		writeError(response, http.StatusUnauthorized, "unauthorized")
		return
	}
	if strings.HasPrefix(request.URL.Path, "/v1/artifacts/") {
		h.serveArtifact(response, request)
		return
	}
	if request.Method == http.MethodGet && request.URL.Path == "/v1/capabilities" {
		writeJSON(response, http.StatusOK, h.Catalog)
		return
	}
	if request.Method != http.MethodPost {
		writeError(response, http.StatusNotFound, "not_found")
		return
	}
	key := strings.TrimSpace(request.Header.Get("Idempotency-Key"))
	if key == "" || len(key) > 512 {
		writeError(response, http.StatusBadRequest, "idempotency_key_required")
		return
	}
	body, err := io.ReadAll(io.LimitReader(request.Body, maxRequestBytes+1))
	if err != nil || len(body) == 0 || len(body) > maxRequestBytes {
		writeError(response, http.StatusBadRequest, "invalid_request_body")
		return
	}
	digest := sha256.Sum256(body)
	requestDigest := "sha256:" + hex.EncodeToString(digest[:])

	// Serialize one idempotency key so duplicate requests cannot rent two
	// accelerators before the first receipt is committed. Independent keys may
	// run concurrently, within the explicit worker concurrency boundary.
	unlockKey := h.lockKey(key)
	defer unlockKey()
	if receipt, found, lookupErr := h.Receipts.Get(request.Context(), key); lookupErr != nil {
		h.fail(request.Context(), response, lookupErr)
		return
	} else if found {
		if receipt.RequestDigest != requestDigest {
			writeError(response, http.StatusConflict, "idempotency_conflict")
			return
		}
		response.Header().Set("Idempotency-Replayed", "true")
		writeRawJSON(response, http.StatusOK, receipt.Response)
		return
	}
	if err = h.acquireSlot(request.Context()); err != nil {
		writeError(response, http.StatusRequestTimeout, "request_cancelled")
		return
	}
	defer h.releaseSlot()

	var output any
	switch request.URL.Path {
	case "/v1/profiles":
		var input acceleratorlab.ProfileIntent
		if err = decodeStrict(body, &input); err != nil {
			writeError(response, http.StatusBadRequest, "invalid_request")
			return
		}
		output, err = h.Profiler.Capture(request.Context(), input)
	case "/v1/kernel-generations":
		if h.Generator == nil {
			writeError(response, http.StatusNotImplemented, "generation_not_configured")
			return
		}
		var input acceleratorlab.GenerationRequest
		if err = decodeStrict(body, &input); err != nil {
			writeError(response, http.StatusBadRequest, "invalid_request")
			return
		}
		output, err = h.Generator.Generate(request.Context(), input)
	case "/v1/qualifications":
		var input acceleratorlab.QualificationRequest
		if err = decodeStrict(body, &input); err != nil {
			writeError(response, http.StatusBadRequest, "invalid_request")
			return
		}
		output, err = h.Qualifier.Qualify(request.Context(), input)
	default:
		writeError(response, http.StatusNotFound, "not_found")
		return
	}
	if err != nil {
		h.fail(request.Context(), response, err)
		return
	}
	encoded, err := json.Marshal(output)
	if err != nil {
		h.fail(request.Context(), response, err)
		return
	}
	if err = h.Receipts.Put(request.Context(), key, Receipt{RequestDigest: requestDigest, Response: encoded}); err != nil {
		if strings.Contains(err.Error(), "different request") {
			writeError(response, http.StatusConflict, "idempotency_conflict")
			return
		}
		h.fail(request.Context(), response, err)
		return
	}
	writeRawJSON(response, http.StatusOK, encoded)
}

func (h *Handler) lockKey(key string) func() {
	h.lockMu.Lock()
	if h.keyLocks == nil {
		h.keyLocks = map[string]*keyLock{}
	}
	lock := h.keyLocks[key]
	if lock == nil {
		lock = &keyLock{}
		h.keyLocks[key] = lock
	}
	lock.refs++
	h.lockMu.Unlock()
	lock.mu.Lock()
	return func() {
		lock.mu.Unlock()
		h.lockMu.Lock()
		lock.refs--
		if lock.refs == 0 {
			delete(h.keyLocks, key)
		}
		h.lockMu.Unlock()
	}
}

func (h *Handler) acquireSlot(ctx context.Context) error {
	h.semaphoreOnce.Do(func() {
		limit := h.MaxConcurrent
		if limit < 1 {
			limit = 1
		}
		h.semaphore = make(chan struct{}, limit)
	})
	select {
	case h.semaphore <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (h *Handler) releaseSlot() { <-h.semaphore }

func (h *Handler) serveArtifact(response http.ResponseWriter, request *http.Request) {
	if h.Artifacts == nil {
		writeError(response, http.StatusNotImplemented, "artifact_store_not_configured")
		return
	}
	parts := strings.Split(strings.TrimPrefix(path.Clean(request.URL.Path), "/v1/artifacts/"), "/")
	switch request.Method {
	case http.MethodGet:
		if len(parts) != 2 || parts[0] != "sha256" {
			writeError(response, http.StatusNotFound, "artifact_not_found")
			return
		}
		reader, size, err := h.Artifacts.Open(request.Context(), parts[1])
		if errors.Is(err, os.ErrNotExist) {
			writeError(response, http.StatusNotFound, "artifact_not_found")
			return
		}
		if err != nil {
			h.fail(request.Context(), response, err)
			return
		}
		defer reader.Close()
		response.Header().Set("Content-Type", "application/octet-stream")
		response.Header().Set("Content-Length", strconv.FormatInt(size, 10))
		response.Header().Set("Cache-Control", "private, immutable, max-age=31536000")
		response.WriteHeader(http.StatusOK)
		_, _ = io.Copy(response, reader)
	case http.MethodPut:
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" || request.ContentLength < 1 {
			writeError(response, http.StatusBadRequest, "invalid_artifact_request")
			return
		}
		limit := h.MaxArtifactBytes
		if limit <= 0 {
			limit = 512 << 20
		}
		if request.ContentLength > limit {
			writeError(response, http.StatusRequestEntityTooLarge, "artifact_too_large")
			return
		}
		artifact, err := h.Artifacts.Put(request.Context(), parts[1], request.Body, request.ContentLength)
		if err != nil {
			h.fail(request.Context(), response, err)
			return
		}
		writeJSON(response, http.StatusOK, artifact)
	default:
		writeError(response, http.StatusMethodNotAllowed, "method_not_allowed")
	}
}

func (h *Handler) authorized(value string) bool {
	wanted := []byte("Bearer " + strings.TrimSpace(h.Token))
	provided := []byte(strings.TrimSpace(value))
	return len(wanted) == len(provided) && subtle.ConstantTimeCompare(wanted, provided) == 1
}

func (h *Handler) fail(ctx context.Context, response http.ResponseWriter, err error) {
	if h.ReportError != nil {
		h.ReportError(ctx, err)
	}
	writeError(response, http.StatusBadGateway, "worker_execution_failed")
}

func decodeStrict(body []byte, output any) error {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(output); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("request contains trailing JSON")
	}
	return nil
}

func writeJSON(response http.ResponseWriter, status int, value any) {
	encoded, err := json.Marshal(value)
	if err != nil {
		writeError(response, http.StatusInternalServerError, "response_encoding_failed")
		return
	}
	writeRawJSON(response, status, encoded)
}

func writeRawJSON(response http.ResponseWriter, status int, value []byte) {
	response.Header().Set("Content-Type", "application/json")
	response.Header().Set("Cache-Control", "no-store")
	response.WriteHeader(status)
	_, _ = response.Write(append(value, '\n'))
}

func writeError(response http.ResponseWriter, status int, code string) {
	writeJSON(response, status, map[string]string{"error": code})
}
