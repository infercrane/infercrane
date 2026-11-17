package controlapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/gpuprofile"
	"github.com/infercrane/infercrane/internal/gputrace"
	"github.com/infercrane/infercrane/internal/testtools/tracefixture"
)

type fakeGPUProfileHistory struct {
	*fakeGPUProfileStore
	profiles map[string]gpuprofile.Profile
	active   map[string]bool
	listErr  error
	tenant   string
}

func (f *fakeGPUProfileHistory) EndpointGPUProfiles(_ context.Context, tenant, _ string) ([]gpuprofile.Summary, error) {
	f.tenant = tenant
	if f.listErr != nil {
		return nil, f.listErr
	}
	list := []gpuprofile.Summary{}
	for id, profile := range f.profiles {
		list = append(list, gpuprofile.SummaryOf(profile, f.active[id]))
	}
	return list, nil
}

func (f *fakeGPUProfileHistory) EndpointGPUProfileByID(_ context.Context, tenant, _, id string) (gpuprofile.Profile, bool, error) {
	f.tenant = tenant
	profile, ok := f.profiles[id]
	if !ok {
		return gpuprofile.Profile{}, false, gpuprofile.ErrNotRecorded
	}
	return profile, f.active[id], nil
}

func analyzedProfile(t *testing.T, id string, revision int, base time.Time) gpuprofile.Profile {
	t.Helper()
	trace, err := gputrace.ParseKineto(bytes.NewReader(tracefixture.VLLMSingleGPU(base)))
	if err != nil {
		t.Fatal(err)
	}
	recording, err := gputrace.Profile(trace, gputrace.Options{Engine: "vllm"}, gputrace.CaptureFromTrace(trace, "torch-profiler", "vllm-0.22.1", time.Time{}))
	if err != nil {
		t.Fatal(err)
	}
	profile := gpuprofile.ProfileFromRecording(recording)
	profile.Capture.ID, profile.Endpoint, profile.Deployment, profile.Revision, profile.RecordedAt = id, "coder", "coder", revision, base.Add(time.Minute)
	return profile
}

func TestGPUProfileHistoryReadAndCompare(t *testing.T) {
	before, after := "gpc-"+strings.Repeat("1", 32), "gpc-"+strings.Repeat("2", 32)
	base := time.Now().UTC().Add(-time.Hour)
	beforeProfile := analyzedProfile(t, before, 3, base)
	afterProfile := analyzedProfile(t, after, 4, base.Add(10*time.Minute))
	afterProfile.Analysis.Utilization.BusyShare, afterProfile.Analysis.Utilization.IdleShare = 0.92, 0.08
	store := &fakeGPUProfileHistory{
		fakeGPUProfileStore: &fakeGPUProfileStore{fakeStore: &fakeStore{principal: domain.Principal{ID: "viewer-1", TenantID: "tenant-1", Name: "viewer", Role: "viewer", Scopes: []string{"read"}}}},
		profiles:            map[string]gpuprofile.Profile{before: beforeProfile, after: afterProfile},
		active:              map[string]bool{after: true},
	}
	handler := (API{Store: store, Authenticator: store}).Handler()

	response := serve(handler, http.MethodGet, "/api/v1/endpoints/coder/gpu-profiles", "viewer-session", "")
	var list struct {
		Data []gpuprofile.Summary `json:"data"`
	}
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &list) != nil || len(list.Data) != 2 || store.tenant != "tenant-1" {
		t.Fatalf("list status=%d body=%.300s", response.Code, response.Body.String())
	}
	response = serve(handler, http.MethodGet, "/api/v1/endpoints/coder/gpu-profiles/"+before, "viewer-session", "")
	var profile gpuprofile.Profile
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &profile) != nil || profile.Revision != 3 || profile.Analysis == nil || profile.TimelineExcerpt == nil || profile.SchemaVersion != gpuprofile.SchemaVersionV2 {
		t.Fatalf("by id status=%d body=%.300s", response.Code, response.Body.String())
	}
	response = serve(handler, http.MethodGet, "/api/v1/endpoints/coder/gpu-profiles/compare?base="+before+"&head="+after, "viewer-session", "")
	var comparison gpuprofile.Comparison
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &comparison) != nil || comparison.Base.ID != before || !comparison.Head.Active || len(comparison.Metrics) == 0 {
		t.Fatalf("compare status=%d body=%.300s", response.Code, response.Body.String())
	}
	if busy := comparison.Metrics[0]; busy.Metric != "busy_share" || busy.Improved == nil || !*busy.Improved {
		t.Fatalf("busier GPU after the change is better: %+v", busy)
	}

	for name, check := range map[string]struct {
		path   string
		status int
		code   string
	}{
		"unknown capture":      {"/api/v1/endpoints/coder/gpu-profiles/gpc-" + strings.Repeat("9", 32), http.StatusNotFound, "gpu_profile_not_found"},
		"bad id shape":         {"/api/v1/endpoints/coder/gpu-profiles/gpc-XYZ", http.StatusBadRequest, "invalid_request"},
		"same capture twice":   {"/api/v1/endpoints/coder/gpu-profiles/compare?base=" + before + "&head=" + before, http.StatusBadRequest, "invalid_request"},
		"compare missing head": {"/api/v1/endpoints/coder/gpu-profiles/compare?base=" + before, http.StatusBadRequest, "invalid_request"},
		"compare extra query":  {"/api/v1/endpoints/coder/gpu-profiles/compare?base=" + before + "&head=" + after + "&x=1", http.StatusBadRequest, "invalid_request"},
		"list with query":      {"/api/v1/endpoints/coder/gpu-profiles?limit=5", http.StatusBadRequest, "invalid_request"},
	} {
		response = serve(handler, http.MethodGet, check.path, "viewer-session", "")
		if response.Code != check.status || check.code != "" && !strings.Contains(response.Body.String(), `"code":"`+check.code+`"`) {
			t.Fatalf("%s: status=%d body=%s", name, response.Code, response.Body.String())
		}
	}

	callStacksOnly := beforeProfile
	callStacksOnly.Analysis, callStacksOnly.SchemaVersion = nil, gpuprofile.SchemaVersion
	store.profiles[before] = callStacksOnly
	response = serve(handler, http.MethodGet, "/api/v1/endpoints/coder/gpu-profiles/compare?base="+before+"&head="+after, "viewer-session", "")
	if response.Code != http.StatusUnprocessableEntity || !strings.Contains(response.Body.String(), `"code":"gpu_profile_not_comparable"`) {
		t.Fatalf("v1 compare status=%d body=%s", response.Code, response.Body.String())
	}
	store.listErr = domain.ErrNotFound
	if response = serve(handler, http.MethodGet, "/api/v1/endpoints/missing/gpu-profiles", "viewer-session", ""); response.Code != http.StatusNotFound {
		t.Fatalf("unknown endpoint status=%d", response.Code)
	}
	if response = serve(handler, http.MethodGet, "/api/v1/endpoints/coder/gpu-profiles", "", ""); response.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous status=%d", response.Code)
	}
}

func TestAnalyzedRecordingIsAcceptedThroughTheAPI(t *testing.T) {
	store := &fakeGPUProfileStore{fakeStore: &fakeStore{}}
	handler := (API{Store: store, APIKey: "secret"}).Handler()
	trace, err := gputrace.ParseKineto(bytes.NewReader(tracefixture.VLLMSingleGPU(time.Now().UTC().Add(-time.Hour))))
	if err != nil {
		t.Fatal(err)
	}
	recording, err := gputrace.Profile(trace, gputrace.Options{}, gputrace.CaptureFromTrace(trace, "torch-profiler", "vllm-0.22.1", time.Time{}))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(recording)
	response := serve(handler, http.MethodPost, "/api/v1/deployments/coder/gpu-profiles", "secret", string(body))
	if response.Code != http.StatusCreated || len(store.recorded) != 1 || store.recorded[0].Analysis == nil {
		t.Fatalf("status=%d body=%.300s", response.Code, response.Body.String())
	}
	recording.Analysis.Utilization.BusyShare = 2
	body, _ = json.Marshal(recording)
	if response = serve(handler, http.MethodPost, "/api/v1/deployments/coder/gpu-profiles", "secret", string(body)); response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("an out-of-range analysis was accepted: status=%d", response.Code)
	}
}

func TestGPUProfileHistoryRoutesNeedACapableStore(t *testing.T) {
	handler := (API{Store: &fakeGPUProfileStore{fakeStore: &fakeStore{}}, APIKey: "secret"}).Handler()
	for _, path := range []string{"/api/v1/endpoints/coder/gpu-profiles", "/api/v1/endpoints/coder/gpu-profiles/gpc-" + strings.Repeat("1", 32), "/api/v1/endpoints/coder/gpu-profiles/compare?base=a&head=b"} {
		if response := serve(handler, http.MethodGet, path, "secret", ""); response.Code != http.StatusNotImplemented {
			t.Fatalf("%s status=%d", path, response.Code)
		}
	}
}
