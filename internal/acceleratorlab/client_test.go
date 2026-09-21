package acceleratorlab

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/brezelexecutor"
	"github.com/infercrane/infercrane/internal/kernelplanner"
)

func TestWorkerClientProfilesAndQualifiesWithoutLeakingTokenInBody(t *testing.T) {
	request := fixtureRequest(ModalityText)
	var calls []string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, httpRequest *http.Request) {
		calls = append(calls, httpRequest.URL.Path)
		if httpRequest.Header.Get("Authorization") != "Bearer worker-token" || httpRequest.Header.Get("Idempotency-Key") == "" {
			t.Fatalf("headers=%v", httpRequest.Header)
		}
		var body map[string]any
		if err := json.NewDecoder(httpRequest.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		encoded, _ := json.Marshal(body)
		if strings.Contains(string(encoded), "worker-token") {
			t.Fatal("worker credential reached request body")
		}
		writer.Header().Set("Content-Type", "application/json")
		switch httpRequest.URL.Path {
		case "/v1/profiles":
			var input ProfileIntent
			remarshal(body, &input)
			_ = json.NewEncoder(writer).Encode(ProfileEvidence{InputDigest: input.InputDigest, Tool: "nsight", ToolVersion: "1", Hardware: input.Hardware, RuntimeImageDigest: input.Runtime.ImageDigest, Topology: input.Topology, WorkloadDigest: input.Workload.Digest, CapturedAt: time.Now().UTC(), Hotspots: []kernelplanner.Hotspot{{ID: "one", Name: "one", Family: kernelplanner.SwiGLU, DeviceTimeFraction: .1}}, ArtifactDigest: digestOf("a")})
		case "/v1/qualifications":
			var input QualificationRequest
			remarshal(body, &input)
			_ = json.NewEncoder(writer).Encode(QualificationEvidence{InputDigest: input.InputDigest, Hardware: input.Hardware, RuntimeImageDigest: input.Runtime.ImageDigest, Topology: input.Topology, WorkloadDigest: input.Workload.Digest, Modality: input.Workload.Modality, Media: input.Workload.Media, Replay: input.Workload.Replay, QualitySuite: input.Workload.QualitySuite, BuildArtifactDigest: input.Build.Artifacts[0].SHA256, CorrectnessPassed: true, ProfilerArtifactDigest: digestOf("b"), MeasuredAt: time.Now().UTC()})
		case "/v1/kernel-generations":
			var input GenerationRequest
			remarshal(body, &input)
			_ = json.NewEncoder(writer).Encode(GenerationEvidence{InputDigest: input.InputDigest, CandidateID: input.Candidate.ID, Generator: "agent", GeneratorVersion: "1", ProfileArtifactDigest: input.Profile.ArtifactDigest, Source: SourcePin{ImplementationID: "generated", Revision: strings.Repeat("f", 40), License: "Apache-2.0", Artifacts: []Artifact{{Kind: "source", URI: "https://worker.example/v1/artifacts/generated", SHA256: digestOf("f"), Size: 1}}}, GeneratedAt: time.Now().UTC()})
		default:
			http.NotFound(writer, httpRequest)
		}
	}))
	defer server.Close()

	client, err := NewWorkerClient(WorkerConfig{BaseURL: server.URL, Token: "worker-token", Client: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	profile, err := client.Capture(context.Background(), ProfileIntent{InputDigest: digestOf("c"), Hardware: request.Hardware, Workload: request.Workload})
	if err != nil || profile.InputDigest == "" {
		t.Fatalf("profile=%+v err=%v", profile, err)
	}
	generation, err := client.Generate(context.Background(), GenerationRequest{InputDigest: digestOf("c"), Candidate: kernelplanner.Candidate{ID: "experiment-1"}, Profile: profile})
	if err != nil || generation.CandidateID != "experiment-1" {
		t.Fatalf("generation=%+v err=%v", generation, err)
	}
	build := BuildEvidence{Artifacts: []Artifact{{SHA256: digestOf("d")}}}
	qualification, err := client.Qualify(context.Background(), QualificationRequest{InputDigest: digestOf("e"), Experiment: kernelplanner.Candidate{ID: "experiment-1"}, Build: build, Runtime: request.Runtime, Hardware: request.Hardware, Workload: request.Workload})
	if err != nil || qualification.InputDigest == "" {
		t.Fatalf("qualification=%+v err=%v", qualification, err)
	}
	if strings.Join(calls, ",") != "/v1/profiles,/v1/kernel-generations,/v1/qualifications" {
		t.Fatalf("calls=%v", calls)
	}
}

func TestWorkerClientRequiresHTTPSOutsideLoopbackAndOwnerOnlyTokenFile(t *testing.T) {
	if _, err := NewWorkerClient(WorkerConfig{BaseURL: "http://worker.example", Token: "token"}); err == nil {
		t.Fatal("insecure non-loopback worker accepted")
	}
	directory := t.TempDir()
	path := filepath.Join(directory, "token")
	if err := os.WriteFile(path, []byte("token"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := NewWorkerClientFromTokenFile(WorkerConfig{BaseURL: "http://127.0.0.1:1"}, path); err == nil {
		t.Fatal("group-readable token file accepted")
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewWorkerClientFromTokenFile(WorkerConfig{BaseURL: "http://127.0.0.1:1"}, path); err != nil {
		t.Fatal(err)
	}
}

func TestWorkerClientRejectsUnknownEvidenceFields(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte(`{"input_digest":"x","unexpected":"field"}`))
	}))
	defer server.Close()
	client, err := NewWorkerClient(WorkerConfig{BaseURL: server.URL, Token: "token", Client: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = client.Capture(context.Background(), ProfileIntent{InputDigest: "x"}); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("unknown evidence field accepted: %v", err)
	}
}

func TestWorkerClientLoadsOnlyWorkerDeclaredCapabilities(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet || request.URL.Path != "/v1/capabilities" || request.Header.Get("Authorization") != "Bearer token" {
			http.NotFound(writer, request)
			return
		}
		_ = json.NewEncoder(writer).Encode(CapabilityCatalog{Version: "worker-2026-09-21", Capabilities: []Capability{{
			Vendor: "nvidia", Profiler: "nsight-systems+nsight-compute", RuntimeAllowlist: []string{"tensorrt-llm"}, Modalities: []Modality{ModalityText}, MultiNode: true, DisaggregatedServing: true, ExpertParallel: true, AdapterState: "ready", Qualification: "exact target required",
		}}})
	}))
	defer server.Close()
	client, err := NewWorkerClient(WorkerConfig{BaseURL: server.URL, Token: "token", Client: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := client.Capabilities(context.Background())
	if err != nil || len(catalog.Capabilities) != 1 || catalog.Capabilities[0].Vendor != "nvidia" || catalog.Capabilities[0].AdapterState != "ready" {
		t.Fatalf("catalog=%+v err=%v", catalog, err)
	}
}

func TestWorkerClientRestrictsAndVerifiesArtifactBroker(t *testing.T) {
	payload := []byte("kernel-source-or-binary")
	digest := sha256.Sum256(payload)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer token" {
			t.Fatal("artifact request omitted worker credential")
		}
		switch request.Method + " " + request.URL.Path {
		case "GET /v1/artifacts/source":
			_, _ = writer.Write(payload)
		case "PUT /v1/artifacts/job-1/kernel":
			data, _ := io.ReadAll(request.Body)
			if string(data) != string(payload) || request.Header.Get("Idempotency-Key") != "job-1:kernel" {
				t.Fatalf("published=%q headers=%v", data, request.Header)
			}
			_ = json.NewEncoder(writer).Encode(Artifact{Kind: "kernel", URI: "s3://infercrane-artifacts/published", SHA256: "sha256:" + hex.EncodeToString(digest[:]), Size: int64(len(payload))})
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	client, err := NewWorkerClient(WorkerConfig{BaseURL: server.URL, Token: "token", Client: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	reader, err := client.Open(context.Background(), brezelexecutor.ArtifactRef{Name: "source", URI: server.URL + "/v1/artifacts/source", SHA256: hex.EncodeToString(digest[:])})
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(reader)
	reader.Close()
	if string(data) != string(payload) {
		t.Fatalf("read=%q", data)
	}
	if _, err = client.Open(context.Background(), brezelexecutor.ArtifactRef{Name: "source", URI: "https://metadata.google.internal/v1/artifacts/source", SHA256: hex.EncodeToString(digest[:])}); err == nil {
		t.Fatal("cross-origin artifact URI accepted")
	}
	uri, err := client.Publish(context.Background(), "job-1", "kernel", strings.NewReader(string(payload)), int64(len(payload)))
	if err != nil || uri != "s3://infercrane-artifacts/published" {
		t.Fatalf("uri=%q err=%v", uri, err)
	}
}

func remarshal(input any, output any) {
	encoded, _ := json.Marshal(input)
	_ = json.Unmarshal(encoded, output)
}
