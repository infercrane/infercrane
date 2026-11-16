package controlapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/gpuprofile"
)

type fakeGPUProfileStore struct {
	*fakeStore
	recorded  []gpuprofile.Recording
	tenants   []string
	recordErr error
	profile   gpuprofile.Profile
	readErr   error
}

func (f *fakeGPUProfileStore) RecordGPUProfile(_ context.Context, tenant, deployment string, recording gpuprofile.Recording) (gpuprofile.Receipt, error) {
	if f.recordErr != nil {
		return gpuprofile.Receipt{}, f.recordErr
	}
	f.recorded, f.tenants = append(f.recorded, recording), append(f.tenants, tenant)
	return gpuprofile.Receipt{ID: "gpc-" + strings.Repeat("a", 32), Deployment: deployment, Revision: 4, StartedAt: recording.Capture.StartedAt, EndedAt: recording.Capture.EndedAt, Frames: len(recording.Frames), Stacks: len(recording.Stacks), RecordedAt: time.Now().UTC()}, nil
}

func (f *fakeGPUProfileStore) EndpointGPUProfile(context.Context, string, string) (gpuprofile.Profile, error) {
	return f.profile, f.readErr
}

func gpuProfileBody(t *testing.T, mutate func(map[string]any)) string {
	t.Helper()
	started := time.Now().UTC().Add(-2 * time.Minute).Truncate(time.Second)
	body := map[string]any{
		"schema_version": gpuprofile.SchemaVersion,
		"capture": map[string]any{
			"tool": "cupti-pc-sampling", "tool_version": "12.8", "evidence_class": "measured",
			"accelerator": "NVIDIA L40S", "gpus": 1, "content_recorded": false,
			"started_at": started.Format(time.RFC3339), "ended_at": started.Add(time.Minute).Format(time.RFC3339),
		},
		"timeline": map[string]any{"bucket_seconds": 30, "buckets": []any{
			map[string]any{"start": started.Format(time.RFC3339), "gpu_seconds": 12.5},
			map[string]any{"start": started.Add(30 * time.Second).Format(time.RFC3339), "gpu_seconds": nil},
		}},
		"frames": []any{
			map[string]any{"kind": "python", "name": "LlamaAttention.forward", "file": "vllm/model_executor/models/llama.py", "line": 180},
			map[string]any{"kind": "kernel", "name": "flash_fwd_splitkv_kernel"},
			map[string]any{"kind": "stall", "name": "long_scoreboard"},
		},
		"stacks":             []any{map[string]any{"frames": []int{0, 1, 2}, "gpu_ns": 9_000_000_000}},
		"unavailable_layers": []string{"sass"},
	}
	if mutate != nil {
		mutate(body)
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func serve(handler http.Handler, method, path, token, body string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func TestGPUProfileRecordingIsBoundedContentFreeAndRevisionPinned(t *testing.T) {
	store := &fakeGPUProfileStore{fakeStore: &fakeStore{}}
	handler := (API{Store: store, APIKey: "secret"}).Handler()
	const path = "/api/v1/deployments/coder/gpu-profiles"

	response := serve(handler, http.MethodPost, path, "secret", gpuProfileBody(t, nil))
	if response.Code != http.StatusCreated || len(store.recorded) != 1 || !strings.Contains(response.Body.String(), `"revision":4`) || !strings.Contains(response.Body.String(), `"frames":3`) {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if got := store.recorded[0]; got.Capture.StartedAt.Location() != time.UTC || got.UnavailableLayers[0] != "sass" || got.Timeline.Buckets[1].GPUSeconds != nil {
		t.Fatalf("recording was not normalized or lost an empty bucket: %+v", got)
	}

	for name, check := range map[string]struct {
		body   string
		status int
		code   string
	}{
		"unknown field":    {gpuProfileBody(t, func(b map[string]any) { b["prompt"] = "hello" }), http.StatusBadRequest, "invalid_request"},
		"content recorded": {gpuProfileBody(t, func(b map[string]any) { b["capture"].(map[string]any)["content_recorded"] = true }), http.StatusUnprocessableEntity, "invalid_gpu_profile"},
		"fixture evidence": {gpuProfileBody(t, func(b map[string]any) { b["capture"].(map[string]any)["evidence_class"] = "fixture" }), http.StatusUnprocessableEntity, "invalid_gpu_profile"},
		"missing frame":    {gpuProfileBody(t, func(b map[string]any) { b["stacks"] = []any{map[string]any{"frames": []int{0, 7}, "gpu_ns": 1}} }), http.StatusUnprocessableEntity, "invalid_gpu_profile"},
		"two objects":      {gpuProfileBody(t, nil) + "{}", http.StatusBadRequest, "invalid_request"},
	} {
		response = serve(handler, http.MethodPost, path, "secret", check.body)
		if response.Code != check.status || !strings.Contains(response.Body.String(), `"code":"`+check.code+`"`) {
			t.Fatalf("%s: status=%d body=%s", name, response.Code, response.Body.String())
		}
	}
	if len(store.recorded) != 1 {
		t.Fatalf("a rejected recording reached the store: %d", len(store.recorded))
	}

	oversized := gpuProfileBody(t, func(b map[string]any) {
		frames := make([]any, 0, 20_000)
		for index := 0; index < 20_000; index++ {
			frames = append(frames, map[string]any{"kind": "kernel", "name": fmt.Sprintf("%0500d", index)})
		}
		b["frames"] = frames
	})
	response = serve(handler, http.MethodPost, path, "secret", oversized)
	if response.Code != http.StatusRequestEntityTooLarge || !strings.Contains(response.Body.String(), `"code":"gpu_profile_too_large"`) {
		t.Fatalf("oversized: status=%d body=%.300s", response.Code, response.Body.String())
	}

	store.recordErr = domain.ErrNotFound
	if response = serve(handler, http.MethodPost, path, "secret", gpuProfileBody(t, nil)); response.Code != http.StatusNotFound {
		t.Fatalf("unknown deployment status=%d", response.Code)
	}
	store.recordErr = fmt.Errorf("%w: changed", domain.ErrConflict)
	if response = serve(handler, http.MethodPost, path, "secret", gpuProfileBody(t, nil)); response.Code != http.StatusConflict {
		t.Fatalf("changed retry status=%d", response.Code)
	}
	if response = serve(handler, http.MethodPost, path, "", gpuProfileBody(t, nil)); response.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous status=%d", response.Code)
	}
}

func TestGPUProfileReadNeedsOnlyReadAndNamesAMissingCapture(t *testing.T) {
	now := time.Now().UTC()
	gpus := 1
	store := &fakeGPUProfileStore{fakeStore: &fakeStore{principal: domain.Principal{ID: "viewer-1", TenantID: "tenant-1", Name: "viewer", Role: "viewer", Scopes: []string{"read"}}}}
	store.profile = gpuprofile.Profile{
		SchemaVersion: gpuprofile.SchemaVersion, Endpoint: "coder", Deployment: "coder", Revision: 4,
		Capture:  gpuprofile.Capture{ID: "gpc-" + strings.Repeat("b", 32), CaptureMetadata: gpuprofile.CaptureMetadata{Tool: "cupti-pc-sampling", ToolVersion: "12.8", EvidenceClass: "measured", GPUs: &gpus, StartedAt: now.Add(-time.Minute), EndedAt: now}},
		Timeline: gpuprofile.Timeline{BucketSeconds: 60, Buckets: []gpuprofile.Bucket{}}, Frames: []gpuprofile.Frame{{Kind: "kernel", Name: "gemm"}},
		Stacks: []gpuprofile.Stack{{Frames: []int{0}, GPUNS: 1}}, UnavailableLayers: []string{}, RecordedAt: now,
	}
	handler := (API{Store: store, Authenticator: store}).Handler()

	response := serve(handler, http.MethodGet, "/api/v1/endpoints/coder/gpu-profile", "viewer-session", "")
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var decoded map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"schema_version", "endpoint", "deployment", "revision", "capture", "timeline", "frames", "stacks", "unavailable_layers", "recorded_at"} {
		if _, ok := decoded[key]; !ok {
			t.Fatalf("response is missing %s: %s", key, response.Body.String())
		}
	}
	if capture := decoded["capture"].(map[string]any); capture["content_recorded"] != false || capture["accelerator"] != nil {
		t.Fatalf("capture=%v", capture)
	}
	if response = serve(handler, http.MethodPost, "/api/v1/deployments/coder/gpu-profiles", "viewer-session", gpuProfileBody(t, nil)); response.Code != http.StatusForbidden {
		t.Fatalf("a viewer recorded a capture: status=%d", response.Code)
	}
	if response = serve(handler, http.MethodGet, "/api/v1/endpoints/coder/gpu-profile?window_seconds=60", "viewer-session", ""); response.Code != http.StatusBadRequest {
		t.Fatalf("query status=%d", response.Code)
	}

	store.readErr = gpuprofile.ErrNotRecorded
	response = serve(handler, http.MethodGet, "/api/v1/endpoints/coder/gpu-profile", "viewer-session", "")
	if response.Code != http.StatusNotFound || !strings.Contains(response.Body.String(), `"code":"gpu_profile_not_found"`) {
		t.Fatalf("not recorded: status=%d body=%s", response.Code, response.Body.String())
	}
	store.readErr = domain.ErrNotFound
	response = serve(handler, http.MethodGet, "/api/v1/endpoints/missing/gpu-profile", "viewer-session", "")
	if response.Code != http.StatusNotFound || !strings.Contains(response.Body.String(), `"code":"not_found"`) {
		t.Fatalf("unknown endpoint: status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestGPUProfileRoutesNeedACapableStore(t *testing.T) {
	handler := (API{Store: &fakeStore{}, APIKey: "secret"}).Handler()
	for _, route := range [][2]string{{http.MethodGet, "/api/v1/endpoints/coder/gpu-profile"}, {http.MethodPost, "/api/v1/deployments/coder/gpu-profiles"}} {
		response := serve(handler, route[0], route[1], "secret", gpuProfileBody(t, nil))
		if response.Code != http.StatusNotImplemented || !bytes.Contains(response.Body.Bytes(), []byte(`"capability_unavailable"`)) {
			t.Fatalf("%s %s status=%d body=%s", route[0], route[1], response.Code, response.Body.String())
		}
	}
}
