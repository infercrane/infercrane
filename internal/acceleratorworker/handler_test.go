package acceleratorworker

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/acceleratorlab"
	"github.com/infercrane/infercrane/internal/kernelplanner"
)

type profilerFunc func(context.Context, acceleratorlab.ProfileIntent) (acceleratorlab.ProfileEvidence, error)

func (function profilerFunc) Capture(ctx context.Context, input acceleratorlab.ProfileIntent) (acceleratorlab.ProfileEvidence, error) {
	return function(ctx, input)
}

type qualifierFunc func(context.Context, acceleratorlab.QualificationRequest) (acceleratorlab.QualificationEvidence, error)

func (function qualifierFunc) Qualify(ctx context.Context, input acceleratorlab.QualificationRequest) (acceleratorlab.QualificationEvidence, error) {
	return function(ctx, input)
}

func TestHandlerAuthenticatesAndReplaysOneProfileReceipt(t *testing.T) {
	calls := 0
	handler := fixtureHandler(profilerFunc(func(_ context.Context, input acceleratorlab.ProfileIntent) (acceleratorlab.ProfileEvidence, error) {
		calls++
		return acceleratorlab.ProfileEvidence{
			InputDigest: input.InputDigest, Tool: "nsys", ToolVersion: "1", Hardware: input.Hardware,
			RuntimeImageDigest: input.Runtime.ImageDigest, Topology: input.Topology,
			WorkloadDigest: input.Workload.Digest, CapturedAt: time.Now().UTC(),
			Hotspots: []kernelplanner.Hotspot{{
				ID: "attention-1", Name: "attention", Family: kernelplanner.AttentionDecode,
				DeviceTimeFraction: .4,
			}},
			ArtifactDigest: "sha256:" + strings.Repeat("a", 64),
		}, nil
	}))
	body, _ := json.Marshal(profileIntent())
	for index := 0; index < 2; index++ {
		request := httptest.NewRequest(http.MethodPost, "/v1/profiles", bytes.NewReader(body))
		request.Header.Set("Authorization", "Bearer secret")
		request.Header.Set("Idempotency-Key", "profile-1")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("request %d status=%d body=%s", index, response.Code, response.Body.String())
		}
		if index == 1 && response.Header().Get("Idempotency-Replayed") != "true" {
			t.Fatal("second request did not replay its receipt")
		}
	}
	if calls != 1 {
		t.Fatalf("profiler calls=%d", calls)
	}
}

func TestHandlerFailsClosedForAuthUnknownFieldsAndIdempotencyConflict(t *testing.T) {
	handler := fixtureHandler(profilerFunc(func(_ context.Context, input acceleratorlab.ProfileIntent) (acceleratorlab.ProfileEvidence, error) {
		return acceleratorlab.ProfileEvidence{InputDigest: input.InputDigest}, nil
	}))
	body, _ := json.Marshal(profileIntent())
	request := httptest.NewRequest(http.MethodPost, "/v1/profiles", bytes.NewReader(body))
	request.Header.Set("Idempotency-Key", "profile-1")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status=%d", response.Code)
	}

	unknown := append(bytes.TrimSuffix(body, []byte("}")), []byte(",\"unknown\":true}")...)
	request = httptest.NewRequest(http.MethodPost, "/v1/profiles", bytes.NewReader(unknown))
	request.Header.Set("Authorization", "Bearer secret")
	request.Header.Set("Idempotency-Key", "unknown")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("unknown field status=%d body=%s", response.Code, response.Body.String())
	}

	store := NewMemoryReceiptStore()
	_ = store.Put(t.Context(), "collision", Receipt{RequestDigest: "sha256:a", Response: []byte(`{}`)})
	handler.Receipts = store
	request = httptest.NewRequest(http.MethodPost, "/v1/profiles", bytes.NewReader(body))
	request.Header.Set("Authorization", "Bearer secret")
	request.Header.Set("Idempotency-Key", "collision")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusConflict {
		t.Fatalf("idempotency collision status=%d", response.Code)
	}
}

func TestHandlerPublishesAndReadsContentAddressedArtifacts(t *testing.T) {
	artifacts, err := NewLocalArtifactStore(filepath.Join(t.TempDir(), "artifacts"), "https://worker.example", 1024)
	if err != nil {
		t.Fatal(err)
	}
	handler := fixtureHandler(profilerFunc(func(_ context.Context, input acceleratorlab.ProfileIntent) (acceleratorlab.ProfileEvidence, error) {
		return acceleratorlab.ProfileEvidence{InputDigest: input.InputDigest}, nil
	}))
	handler.Artifacts = artifacts
	publish := httptest.NewRequest(http.MethodPut, "/v1/artifacts/job-1/kernel", strings.NewReader("payload"))
	publish.Header.Set("Authorization", "Bearer secret")
	publish.ContentLength = 7
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, publish)
	if response.Code != http.StatusOK {
		t.Fatalf("publish status=%d body=%s", response.Code, response.Body.String())
	}
	var artifact acceleratorlab.Artifact
	if err = json.Unmarshal(response.Body.Bytes(), &artifact); err != nil {
		t.Fatal(err)
	}
	digest := strings.TrimPrefix(artifact.SHA256, "sha256:")
	read := httptest.NewRequest(http.MethodGet, "/v1/artifacts/sha256/"+digest, nil)
	read.Header.Set("Authorization", "Bearer secret")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, read)
	data, _ := io.ReadAll(response.Result().Body)
	if response.Code != http.StatusOK || string(data) != "payload" {
		t.Fatalf("read status=%d payload=%q", response.Code, data)
	}
}

func TestHandlerRunsIndependentKeysWithinConcurrencyBoundary(t *testing.T) {
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	handler := fixtureHandler(profilerFunc(func(_ context.Context, input acceleratorlab.ProfileIntent) (acceleratorlab.ProfileEvidence, error) {
		entered <- struct{}{}
		<-release
		return acceleratorlab.ProfileEvidence{InputDigest: input.InputDigest}, nil
	}))
	handler.MaxConcurrent = 2
	body, _ := json.Marshal(profileIntent())
	var wait sync.WaitGroup
	for _, key := range []string{"profile-a", "profile-b"} {
		wait.Add(1)
		go func(key string) {
			defer wait.Done()
			request := httptest.NewRequest(http.MethodPost, "/v1/profiles", bytes.NewReader(body))
			request.Header.Set("Authorization", "Bearer secret")
			request.Header.Set("Idempotency-Key", key)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusOK {
				t.Errorf("%s status=%d", key, response.Code)
			}
		}(key)
	}
	for index := 0; index < 2; index++ {
		select {
		case <-entered:
		case <-time.After(time.Second):
			t.Fatal("independent operation was unnecessarily serialized")
		}
	}
	close(release)
	wait.Wait()
}

func fixtureHandler(profiler acceleratorlab.Profiler) *Handler {
	return &Handler{
		Token: "secret", Catalog: acceleratorlab.CapabilityCatalog{
			Version: acceleratorlab.CapabilityCatalogVersion,
			Capabilities: []acceleratorlab.Capability{{
				Vendor: "nvidia", Profiler: "nsys", RuntimeAllowlist: []string{"sglang"},
				Modalities: []acceleratorlab.Modality{acceleratorlab.ModalityText}, AdapterState: "ready", Qualification: "exact-target",
			}},
		},
		Profiler: profiler,
		Qualifier: qualifierFunc(func(_ context.Context, input acceleratorlab.QualificationRequest) (acceleratorlab.QualificationEvidence, error) {
			return acceleratorlab.QualificationEvidence{InputDigest: input.InputDigest}, nil
		}),
		Receipts: NewMemoryReceiptStore(),
	}
}

func profileIntent() acceleratorlab.ProfileIntent {
	return acceleratorlab.ProfileIntent{
		InputDigest: "sha256:" + strings.Repeat("b", 64),
		Model:       kernelplanner.ModelIdentity{Repository: "acme/previously-unseen-open-model", Revision: strings.Repeat("c", 40)},
		Runtime:     kernelplanner.RuntimeIdentity{Name: "sglang", Version: "0.5.20", ImageDigest: "sha256:" + strings.Repeat("d", 64)},
		Hardware:    kernelplanner.HardwareIdentity{Vendor: "nvidia", Accelerator: "H200", ComputeCapability: "sm90"},
		Topology:    acceleratorlab.Topology{Mode: "aggregated", Nodes: 1, Accelerators: 1, TensorParallel: 1, PipelineParallel: 1, ExpertParallel: 1},
		Workload:    acceleratorlab.Workload{Digest: "sha256:" + strings.Repeat("e", 64), Modality: acceleratorlab.ModalityText, Phase: kernelplanner.PhaseDecode, BatchSize: 1, Concurrency: 1, InputTokens: 3919, OutputTokens: 295, QualitySuite: "customer-suite-v1", Replay: "authorized-shape-replay-v1"},
	}
}
