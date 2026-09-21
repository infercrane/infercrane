package brezelexecutor

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestRunUsesFixedRunnerValidatesResultAndCleansSandbox(t *testing.T) {
	var calls []string
	var manifest []byte
	inputBytes := []byte("pinned-source")
	artifactBytes := []byte("compiled-artifact")
	artifactDigest := sha256.Sum256(artifactBytes)
	published := false
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer protected-token" || request.Header.Get("X-Project-ID") != "infercrane" {
			t.Fatalf("missing Brezel authentication headers")
		}
		calls = append(calls, request.Method+" "+request.URL.Path)
		switch request.Method + " " + request.URL.Path {
		case "POST /v1/sandboxes":
			if request.Header.Get("Idempotency-Key") == "" {
				t.Fatal("sandbox create omitted idempotency key")
			}
			var body map[string]any
			_ = json.NewDecoder(request.Body).Decode(&body)
			if body["environment_revision"] != "envr_runner_v1" || body["network"].(map[string]any)["allow_internet"] != false {
				t.Fatalf("unexpected sandbox policy: %#v", body)
			}
			response.WriteHeader(http.StatusAccepted)
			_, _ = io.WriteString(response, `{"resource":{"id":"sbx_1","state":"running"}}`)
		case "GET /v1/sandboxes/sbx_1/files":
			if request.URL.Query().Get("path") == outputArtifactDir+"/kernel.so" {
				_, _ = response.Write(artifactBytes)
				return
			}
			if request.URL.Query().Get("path") == resultPath && manifest == nil {
				response.WriteHeader(http.StatusNotFound)
				return
			}
			var job Job
			_ = json.Unmarshal(manifest, &job)
			digest := sha256.Sum256(manifest)
			result := Result{SchemaVersion: ResultSchemaVersion, JobID: job.ID, InputDigest: hex.EncodeToString(digest[:]), Status: "passed", Artifacts: []OutputArtifact{{Kind: "shared-library", Path: outputArtifactDir + "/kernel.so", SHA256: hex.EncodeToString(artifactDigest[:]), Size: int64(len(artifactBytes))}}}
			_ = json.NewEncoder(response).Encode(result)
		case "PUT /v1/sandboxes/sbx_1/files":
			switch request.URL.Query().Get("path") {
			case inputArtifactDir + "/source":
				data, _ := io.ReadAll(request.Body)
				if string(data) != string(inputBytes) {
					t.Fatalf("input=%q", data)
				}
			case manifestPath:
				manifest, _ = io.ReadAll(request.Body)
			default:
				t.Fatalf("unexpected upload path %q", request.URL.Query().Get("path"))
			}
			_, _ = io.WriteString(response, `{}`)
		case "POST /v1/sandboxes/sbx_1/commands":
			var body struct {
				Argv []string          `json:"argv"`
				Env  map[string]string `json:"env"`
			}
			_ = json.NewDecoder(request.Body).Decode(&body)
			want := []string{"/opt/infercrane/bin/run-optimization-job", "--manifest", manifestPath, "--result", resultPath}
			if !reflect.DeepEqual(body.Argv, want) || len(body.Env) != 0 {
				t.Fatalf("command must be fixed and secret-free: %#v", body)
			}
			response.Header().Set("Content-Type", "application/x-ndjson")
			fmt.Fprintf(response, "{\"type\":\"stdout\",\"execution_id\":\"exec_1\",\"data\":%q}\n", base64.StdEncoding.EncodeToString([]byte("ok")))
			_, _ = io.WriteString(response, "{\"type\":\"exited\",\"execution_id\":\"exec_1\",\"exit_code\":0}\n")
		case "DELETE /v1/sandboxes/sbx_1":
			if request.Header.Get("Idempotency-Key") == "" {
				t.Fatal("sandbox delete omitted idempotency key")
			}
			response.WriteHeader(http.StatusAccepted)
			_, _ = io.WriteString(response, `{"resource":{"id":"sbx_1","state":"deleted"}}`)
		case "GET /v1/sandboxes/sbx_1/receipt":
			_, _ = io.WriteString(response, `{"payloadType":"application/vnd.in-toto+json","payload":"signed","signatures":[]}`)
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()

	resolver := inputResolverFunc(func(_ context.Context, _ ArtifactRef) (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader(string(inputBytes))), nil
	})
	publisher := artifactPublisherFunc(func(_ context.Context, jobID, kind string, reader io.Reader, size int64) (string, error) {
		data, _ := io.ReadAll(reader)
		published = jobID == "job_1" && kind == "shared-library" && size == int64(len(artifactBytes)) && string(data) == string(artifactBytes)
		return "s3://infercrane-artifacts/kernel.so", nil
	})
	client, err := New(Config{BaseURL: server.URL, Token: "protected-token", ProjectID: "infercrane", EnvironmentRevision: "envr_runner_v1", SandboxTTL: 10 * time.Minute, InputResolver: resolver, ArtifactPublisher: publisher})
	if err != nil {
		t.Fatal(err)
	}
	result, err := client.Run(context.Background(), validJob())
	if err != nil {
		t.Fatal(err)
	}
	if result.SandboxID != "sbx_1" || result.ExecutionID != "exec_1" || len(result.Receipt) == 0 || !published || result.Artifacts[0].URI != "s3://infercrane-artifacts/kernel.so" {
		t.Fatalf("unexpected result: %#v", result)
	}
	wantCalls := []string{"POST /v1/sandboxes", "GET /v1/sandboxes/sbx_1/files", "PUT /v1/sandboxes/sbx_1/files", "PUT /v1/sandboxes/sbx_1/files", "POST /v1/sandboxes/sbx_1/commands", "GET /v1/sandboxes/sbx_1/files", "GET /v1/sandboxes/sbx_1/files", "DELETE /v1/sandboxes/sbx_1", "GET /v1/sandboxes/sbx_1/receipt"}
	if !reflect.DeepEqual(calls, wantCalls) {
		t.Fatalf("calls=%v, want %v", calls, wantCalls)
	}
	if strings.Contains(string(manifest), "protected-token") || strings.Contains(string(manifest), "argv") {
		t.Fatal("manifest leaked a credential or command surface")
	}
}

func TestRunCleansSandboxWhenCommandIsIndeterminate(t *testing.T) {
	deleted := false
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.Method + " " + request.URL.Path {
		case "POST /v1/sandboxes":
			response.WriteHeader(http.StatusAccepted)
			_, _ = io.WriteString(response, `{"resource":{"id":"sbx_1","state":"running"}}`)
		case "GET /v1/sandboxes/sbx_1/files":
			response.WriteHeader(http.StatusNotFound)
		case "PUT /v1/sandboxes/sbx_1/files":
			_, _ = io.WriteString(response, `{}`)
		case "POST /v1/sandboxes/sbx_1/commands":
			response.Header().Set("Content-Type", "application/x-ndjson")
			_, _ = io.WriteString(response, "{\"type\":\"error\"}\n")
		case "DELETE /v1/sandboxes/sbx_1":
			deleted = true
			response.WriteHeader(http.StatusAccepted)
			_, _ = io.WriteString(response, `{"resource":{"id":"sbx_1","state":"deleted"}}`)
		}
	}))
	defer server.Close()
	client, _ := New(Config{BaseURL: server.URL, Token: "protected-token", ProjectID: "infercrane", EnvironmentRevision: "envr_runner_v1"})
	job := validJob()
	job.Inputs = nil
	_, err := client.Run(context.Background(), job)
	if !errorsIs(err, ErrCommandIndeterminate) || !deleted {
		t.Fatalf("err=%v deleted=%v", err, deleted)
	}
}

func TestConfigurationAndJobValidationFailClosed(t *testing.T) {
	if _, err := New(Config{BaseURL: "http://example.com", Token: "token", ProjectID: "project", EnvironmentRevision: "envr"}); err == nil {
		t.Fatal("non-loopback HTTP must be rejected")
	}
	client, err := New(Config{BaseURL: "http://127.0.0.1:8080", Token: "token", ProjectID: "project", EnvironmentRevision: "envr"})
	if err != nil {
		t.Fatal(err)
	}
	job := validJob()
	job.Inputs[0].SHA256 = "not-a-digest"
	if _, _, err = encodeJob(job); err == nil {
		t.Fatal("unpinned input must be rejected")
	}
	_ = client
}

func TestNewFromTokenFileRequiresOwnerOnlyRegularFile(t *testing.T) {
	directory := t.TempDir()
	tokenFile := filepath.Join(directory, "brezel.token")
	if err := os.WriteFile(tokenFile, []byte("protected-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	client, err := NewFromTokenFile(Config{BaseURL: "http://127.0.0.1:8080", ProjectID: "project", EnvironmentRevision: "envr"}, tokenFile)
	if err != nil || client.token != "protected-token" {
		t.Fatalf("client=%#v err=%v", client, err)
	}
	if err = os.Chmod(tokenFile, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err = NewFromTokenFile(Config{BaseURL: "http://127.0.0.1:8080", ProjectID: "project", EnvironmentRevision: "envr"}, tokenFile); err == nil {
		t.Fatal("group-readable token file must be rejected")
	}
}

func validJob() Job {
	sourceDigest := sha256.Sum256([]byte("pinned-source"))
	return Job{
		ID: "job_1", CampaignID: "campaign_1", CandidateID: "candidate_1", Kind: KindArtifactBuild,
		Model: "Qwen/Qwen3-8B", ModelRevision: "0123456789abcdef", TargetSM: "sm_90",
		Candidate: Candidate{ImplementationID: "flashinfer-fa3", OperatorFamily: "attention", Backend: "cuda", SourceRevision: "abcdef0123456789", License: "Apache-2.0"},
		Inputs:    []ArtifactRef{{Name: "source", URI: "https://artifacts.example.invalid/source.tar.zst", SHA256: hex.EncodeToString(sourceDigest[:])}}, TimeoutSecs: 600,
	}
}

type inputResolverFunc func(context.Context, ArtifactRef) (io.ReadCloser, error)

func (function inputResolverFunc) Open(ctx context.Context, reference ArtifactRef) (io.ReadCloser, error) {
	return function(ctx, reference)
}

type artifactPublisherFunc func(context.Context, string, string, io.Reader, int64) (string, error)

func (function artifactPublisherFunc) Publish(ctx context.Context, jobID, kind string, reader io.Reader, size int64) (string, error) {
	return function(ctx, jobID, kind, reader, size)
}

func errorsIs(err, target error) bool {
	for err != nil {
		if err == target {
			return true
		}
		type unwrapper interface{ Unwrap() error }
		unwrapped, ok := err.(unwrapper)
		if !ok {
			return false
		}
		err = unwrapped.Unwrap()
	}
	return false
}
