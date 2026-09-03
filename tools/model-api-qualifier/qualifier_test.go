package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/supplieradapter"
)

const testSecret = "deepseek-test-secret-must-never-escape"

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func validTestConfig(t *testing.T) qualifierConfig {
	t.Helper()
	root := t.TempDir()
	return qualifierConfig{
		OfferID: "deepseek-direct", OfferVersion: 1, QualificationID: "deepseek-direct-q-1",
		TupleKey:         "deepseek|deepseek-v4-flash|openai|global",
		ExpectedRevision: supplieradapter.DeepSeekV4FlashRevision,
		EvidenceRef:      "s3://infercrane-qualification/deepseek-direct-q-1.json",
		EvidenceOutput:   filepath.Join(root, "evidence.json"), QualificationOutput: filepath.Join(root, "qualification.json"),
		SamplesPerMode: 2, MaxOutputTokens: 64, RequestTimeout: time.Second, TotalTimeout: 5 * time.Second,
		ValidFor: time.Hour, MaxStreamBytes: 1 << 20, ConfirmLive: true,
	}
}

func testClient(t *testing.T, mutate func(*http.Request) (*http.Response, error)) *http.Client {
	t.Helper()
	return &http.Client{Timeout: time.Second, Transport: roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		if request.Header.Get("Authorization") != "Bearer "+testSecret {
			t.Fatalf("missing in-memory supplier authorization")
		}
		return mutate(request)
	})}
}

func successfulSupplier(t *testing.T) *http.Client {
	t.Helper()
	buffered, streaming := 0, 0
	return testClient(t, func(request *http.Request) (*http.Response, error) {
		switch {
		case request.Method == http.MethodGet && request.URL.String() == supplieradapter.DeepSeekBaseURL+"/models":
			return jsonResponse(`{"object":"list","data":[{"id":"deepseek-v4-pro"},{"id":"deepseek-v4-flash"}]}`), nil
		case request.Method == http.MethodPost && request.URL.String() == supplieradapter.DeepSeekBaseURL+"/chat/completions":
			body, err := io.ReadAll(request.Body)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(body), testSecret) {
				t.Fatal("credential leaked into supplier request body")
			}
			var payload struct {
				Model     string `json:"model"`
				MaxTokens int    `json:"max_tokens"`
				Stream    bool   `json:"stream"`
				Thinking  struct {
					Type string `json:"type"`
				} `json:"thinking"`
			}
			if err = json.Unmarshal(body, &payload); err != nil {
				t.Fatal(err)
			}
			if payload.Model != supplieradapter.DeepSeekV4FlashModelID || payload.MaxTokens != 64 || payload.Thinking.Type != "disabled" {
				t.Fatalf("unexpected qualified payload: %s", body)
			}
			if payload.Stream {
				streaming++
				return streamResponse(streaming, validSSE(supplieradapter.DeepSeekV4FlashModelID, true)), nil
			}
			buffered++
			return bufferedResponse(buffered, supplieradapter.DeepSeekV4FlashModelID, true), nil
		default:
			t.Fatalf("unexpected supplier request: %s %s", request.Method, request.URL)
			return nil, nil
		}
	})
}

func TestRunQualificationEmitsSecretFreeEvidenceAndOperatorManifest(t *testing.T) {
	cfg := validTestConfig(t)
	raw, manifest, err := runQualification(context.Background(), cfg, successfulSupplier(t), []byte(testSecret))
	if err != nil {
		t.Fatal(err)
	}
	if raw.Status != "passed" || !raw.Inventory.TargetPresent || raw.Summary.BufferedSamples != 2 || raw.Summary.StreamingSamples != 2 {
		t.Fatalf("raw evidence=%+v", raw)
	}
	if raw.Summary.TTFTP95MS <= 0 || raw.Summary.OutputTokensP5 <= 0 {
		t.Fatalf("performance summary=%+v", raw.Summary)
	}
	if manifest.OfferID != cfg.OfferID || manifest.OfferVersion != cfg.OfferVersion || manifest.Evidence.State != "qualified" || manifest.Evidence.SampleCount != 4 {
		t.Fatalf("qualification manifest=%+v", manifest)
	}
	if !strings.HasPrefix(manifest.Evidence.EvidenceDigest, "sha256:") || manifest.Evidence.Scope == "" {
		t.Fatalf("qualification evidence=%+v", manifest.Evidence)
	}
	assertSecretFree(t, raw)
	assertSecretFree(t, manifest)
	if err = writeArtifacts(cfg, raw, manifest); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{cfg.EvidenceOutput, cfg.QualificationOutput} {
		content, readErr := os.ReadFile(path)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if strings.Contains(string(content), testSecret) {
			t.Fatalf("secret leaked into %s", path)
		}
		info, statErr := os.Stat(path)
		if statErr != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("artifact permissions=%v err=%v", info.Mode().Perm(), statErr)
		}
	}
	if err = writeArtifacts(cfg, raw, manifest); err == nil {
		t.Fatal("artifacts must be append-only and refuse overwrite")
	}
}

func TestRunQualificationFailsClosedOnIdentityUsageAndTerminalDrift(t *testing.T) {
	tests := map[string]func(*http.Request) (*http.Response, error){
		"buffered model identity": func(request *http.Request) (*http.Response, error) {
			if request.Method == http.MethodGet {
				return jsonResponse(`{"data":[{"id":"deepseek-v4-flash"}]}`), nil
			}
			return bufferedResponse(1, "deepseek-v4-pro", true), nil
		},
		"buffered usage": func(request *http.Request) (*http.Response, error) {
			if request.Method == http.MethodGet {
				return jsonResponse(`{"data":[{"id":"deepseek-v4-flash"}]}`), nil
			}
			return bufferedResponse(1, supplieradapter.DeepSeekV4FlashModelID, false), nil
		},
		"stream terminal": func(request *http.Request) (*http.Response, error) {
			if request.Method == http.MethodGet {
				return jsonResponse(`{"data":[{"id":"deepseek-v4-flash"}]}`), nil
			}
			body, _ := io.ReadAll(request.Body)
			if strings.Contains(string(body), `"stream":true`) {
				return streamResponse(1, validSSE(supplieradapter.DeepSeekV4FlashModelID, false)), nil
			}
			return bufferedResponse(1, supplieradapter.DeepSeekV4FlashModelID, true), nil
		},
	}
	for name, behavior := range tests {
		t.Run(name, func(t *testing.T) {
			cfg := validTestConfig(t)
			cfg.SamplesPerMode = 1
			_, _, err := runQualification(context.Background(), cfg, testClient(t, behavior), []byte(testSecret))
			if err == nil {
				t.Fatal("contract drift must fail qualification")
			}
			if strings.Contains(err.Error(), testSecret) {
				t.Fatalf("secret leaked in error: %v", err)
			}
			if _, statErr := os.Stat(cfg.QualificationOutput); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatal("failed qualification must not publish a manifest")
			}
		})
	}
}

func TestRunQualificationSanitizesMaliciousTransportErrors(t *testing.T) {
	cfg := validTestConfig(t)
	cfg.SamplesPerMode = 1
	client := testClient(t, func(*http.Request) (*http.Response, error) {
		return nil, errors.New("transport included " + testSecret)
	})
	_, _, err := runQualification(context.Background(), cfg, client, []byte(testSecret))
	if err == nil || strings.Contains(err.Error(), testSecret) || strings.Contains(err.Error(), "transport included") {
		t.Fatalf("unsafe error=%v", err)
	}
}

func TestConfigRequiresExactRevisionAndExplicitLiveConfirmation(t *testing.T) {
	cfg := validTestConfig(t)
	cfg.ExpectedRevision = "DeepSeek-V4-Flash-latest"
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "revision drift") {
		t.Fatalf("revision error=%v", err)
	}
	cfg.ExpectedRevision = supplieradapter.DeepSeekV4FlashRevision
	cfg.ConfirmLive = false
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "billable") {
		t.Fatalf("confirmation error=%v", err)
	}
}

func TestQualificationProfilesPinProductionAdapterIdentityAndCredential(t *testing.T) {
	tests := []struct {
		name, supplier, adapter, model, credential, revision, baseURL string
		runPod                                                        bool
	}{
		{profileDeepSeekV4Flash, supplieradapter.DeepSeekSupplier, supplieradapter.DeepSeekAdapterName, supplieradapter.DeepSeekV4FlashModelID, "DEEPSEEK_API_KEY", supplieradapter.DeepSeekV4FlashRevision, supplieradapter.DeepSeekBaseURL, false},
		{profileGLM52, supplieradapter.ZAISupplier, supplieradapter.ZAIAdapterName, supplieradapter.ZAIGLM52ModelID, "ZAI_API_KEY", supplieradapter.ZAIGLM52ModelID, supplieradapter.ZAIBaseURL, false},
		{profileGLM53, supplieradapter.ZAISupplier, supplieradapter.ZAIAdapterName, supplieradapter.ZAIGLM53ModelID, "ZAI_API_KEY", supplieradapter.ZAIGLM53ModelID, supplieradapter.ZAIBaseURL, false},
		{profileGLM53Flash, supplieradapter.ZAISupplier, supplieradapter.ZAIAdapterName, supplieradapter.ZAIGLM53FlashModelID, "ZAI_API_KEY", supplieradapter.ZAIGLM53FlashModelID, supplieradapter.ZAIBaseURL, false},
		{profileQwen38RunPod, supplieradapter.RunPodSupplier, supplieradapter.RunPodSGLangLBAdapterName, supplieradapter.RunPodQwen38SupplierModelID, "RUNPOD_API_KEY", qwen38Revision, "https://qwen38pilot.api.runpod.ai", true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := validTestConfig(t)
			cfg.Profile = test.name
			cfg.ExpectedRevision = test.revision
			cfg.TupleKey = strings.Join([]string{test.supplier, test.model, qualificationProto, "global"}, "|")
			if test.runPod {
				cfg.EndpointOrigin = test.baseURL
				cfg.Region = "EU-RO-1"
				cfg.TupleKey = strings.Join([]string{test.supplier, test.model, qualificationProto, cfg.Region}, "|")
			}
			if err := cfg.Validate(); err != nil {
				t.Fatal(err)
			}
			profile, err := resolveQualificationProfile(test.name)
			if err != nil {
				t.Fatal(err)
			}
			target, err := profile.target(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if profile.Supplier != test.supplier || profile.AdapterName != test.adapter || profile.SupplierModelID != test.model || profile.CredentialEnv != test.credential || profile.ExpectedRevision != test.revision || target.BaseURL != test.baseURL || target.CredentialReference != "env://"+test.credential || profile.NewAdapter(&http.Client{}).Name() != test.adapter {
				t.Fatalf("profile=%+v target=%+v", profile, target)
			}
		})
	}
}

func TestRunQualificationSupportsEveryExplicitProfile(t *testing.T) {
	for _, name := range []string{profileDeepSeekV4Flash, profileGLM52, profileGLM53, profileGLM53Flash, profileQwen38RunPod} {
		t.Run(name, func(t *testing.T) {
			profile, err := resolveQualificationProfile(name)
			if err != nil {
				t.Fatal(err)
			}
			cfg := validTestConfig(t)
			cfg.Profile = name
			cfg.ExpectedRevision = profile.ExpectedRevision
			cfg.SamplesPerMode = 1
			cfg.TupleKey = strings.Join([]string{profile.Supplier, profile.SupplierModelID, qualificationProto, "global"}, "|")
			if name == profileQwen38RunPod {
				cfg.EndpointOrigin = "https://qwen38pilot.api.runpod.ai"
				cfg.Region = "EU-RO-1"
				cfg.TupleKey = strings.Join([]string{profile.Supplier, profile.SupplierModelID, qualificationProto, cfg.Region}, "|")
			}
			raw, manifest, err := runQualification(context.Background(), cfg, successfulProfileSupplier(t, profile, cfg), []byte(testSecret))
			if err != nil {
				t.Fatal(err)
			}
			if raw.Supplier != profile.Supplier || raw.Adapter != profile.AdapterName || raw.Model.SupplierModelID != profile.SupplierModelID || raw.Model.ExpectedRevision != profile.ExpectedRevision || raw.Model.TargetOriginSHA256 == "" || !raw.Inventory.TargetPresent || len(raw.Samples) != 2 || manifest.Evidence.Region != raw.Region || !strings.Contains(manifest.Evidence.Scope, "target_origin_sha256=") {
				t.Fatalf("raw=%+v manifest=%+v", raw, manifest)
			}
			assertSecretFree(t, raw)
			assertSecretFree(t, manifest)
		})
	}
}

func TestRunPodProfileRejectsNonExactOriginAndRevision(t *testing.T) {
	for _, origin := range []string{
		"", "http://qwen38pilot.api.runpod.ai", "https://api.runpod.ai/v2/qwen38/openai",
		"https://qwen38pilot.api.runpod.ai/path", "https://qwen38pilot.api.runpod.ai?token=secret",
		"https://qwen38pilot.api.runpod.ai.evil.test", "https://nested.qwen38pilot.api.runpod.ai",
	} {
		cfg := validTestConfig(t)
		cfg.Profile = profileQwen38RunPod
		cfg.ExpectedRevision = qwen38Revision
		cfg.EndpointOrigin = origin
		cfg.Region = "EU-RO-1"
		if err := cfg.Validate(); err == nil {
			t.Fatalf("unsafe origin %q was accepted", origin)
		}
	}
	cfg := validTestConfig(t)
	cfg.Profile = profileQwen38RunPod
	cfg.ExpectedRevision = "main"
	cfg.EndpointOrigin = "https://qwen38pilot.api.runpod.ai"
	cfg.Region = "EU-RO-1"
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), qwen38Revision) {
		t.Fatalf("revision error=%v", err)
	}
}

func TestProfileRejectsTupleFromAnotherSupplierOrModel(t *testing.T) {
	cfg := validTestConfig(t)
	cfg.Profile = profileGLM53
	cfg.ExpectedRevision = supplieradapter.ZAIGLM53ModelID
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "--tuple-key must exactly match") {
		t.Fatalf("tuple error=%v", err)
	}
}

func successfulProfileSupplier(t *testing.T, profile qualificationProfile, cfg qualifierConfig) *http.Client {
	t.Helper()
	sequence := 0
	return testClient(t, func(request *http.Request) (*http.Response, error) {
		if request.Method == http.MethodGet {
			expected := profile.DefaultBaseURL + "/models"
			if profile.Name == profileQwen38RunPod {
				expected = cfg.EndpointOrigin + "/v1/models"
			}
			if request.URL.String() != expected {
				t.Fatalf("inventory URL=%s expected=%s", request.URL, expected)
			}
			response := jsonResponse(fmt.Sprintf(`{"data":[{"id":%q}]}`, profile.SupplierModelID))
			response.Request = request
			return response, nil
		}
		if request.Method != http.MethodPost {
			t.Fatalf("unexpected supplier method: %s", request.Method)
		}
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(body), testSecret) {
			t.Fatal("credential leaked into supplier request body")
		}
		var payload struct {
			Model     string `json:"model"`
			MaxTokens int    `json:"max_tokens"`
			Stream    bool   `json:"stream"`
		}
		if err = json.Unmarshal(body, &payload); err != nil {
			t.Fatal(err)
		}
		if payload.Model != profile.SupplierModelID || payload.MaxTokens < 1 || payload.MaxTokens > cfg.MaxOutputTokens {
			t.Fatalf("unexpected payload: %s", body)
		}
		sequence++
		if payload.Stream {
			response := profileStreamResponse(profile, sequence)
			response.Request = request
			return response, nil
		}
		response := profileBufferedResponse(profile, sequence)
		response.Request = request
		return response, nil
	})
}

func profileBufferedResponse(profile qualificationProfile, sequence int) *http.Response {
	cache := `"prompt_cache_hit_tokens":0`
	requestID := ""
	if profile.Supplier == supplieradapter.ZAISupplier {
		cache = `"prompt_tokens_details":{"cached_tokens":0}`
		requestID = `,"request_id":"zai-buffered-request"`
	}
	body := fmt.Sprintf(`{"id":"chatcmpl-%d"%s,"model":%q,"choices":[{"index":0,"message":{"role":"assistant","content":"one two three four five six seven eight"},"finish_reason":"stop"}],"usage":{"prompt_tokens":12,"completion_tokens":8,%s}}`, sequence, requestID, profile.SupplierModelID, cache)
	response := jsonResponse(body)
	if profile.Supplier == supplieradapter.DeepSeekSupplier {
		response.Header.Set("X-Request-ID", "ds-buffered-request")
	} else if profile.Supplier == supplieradapter.RunPodSupplier {
		response.Header.Set("X-Runpod-Request-Id", "runpod-buffered-request")
	}
	return response
}

func profileStreamResponse(profile qualificationProfile, sequence int) *http.Response {
	id := fmt.Sprintf("chatcmpl-%d", sequence)
	var body string
	switch profile.Supplier {
	case supplieradapter.ZAISupplier:
		body = fmt.Sprintf("data: {\"id\":%q,\"request_id\":\"zai-stream-request\",\"model\":%q,\"choices\":[{\"index\":0,\"delta\":{\"content\":\"one two three four\"},\"finish_reason\":null}]}\n\n", id, profile.SupplierModelID) +
			fmt.Sprintf("data: {\"id\":%q,\"request_id\":\"zai-stream-request\",\"model\":%q,\"choices\":[{\"index\":0,\"delta\":{\"content\":\" five six seven eight\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":12,\"completion_tokens\":8,\"prompt_tokens_details\":{\"cached_tokens\":0}}}\n\n", id, profile.SupplierModelID)
	case supplieradapter.RunPodSupplier:
		body = fmt.Sprintf("data: {\"id\":%q,\"model\":%q,\"choices\":[{\"index\":0,\"delta\":{\"content\":\"one two three four\"},\"finish_reason\":null}]}\n\n", id, profile.SupplierModelID) +
			fmt.Sprintf("data: {\"id\":%q,\"model\":%q,\"choices\":[{\"index\":0,\"delta\":{\"content\":\" five six seven eight\"},\"finish_reason\":\"stop\"}]}\n\n", id, profile.SupplierModelID) +
			fmt.Sprintf("data: {\"id\":%q,\"model\":%q,\"choices\":[],\"usage\":{\"prompt_tokens\":12,\"completion_tokens\":8,\"prompt_tokens_details\":{\"cached_tokens\":0}}}\n\n", id, profile.SupplierModelID)
	default:
		body = validSSE(profile.SupplierModelID, false)
	}
	body += "data: [DONE]\n\n"
	response := streamResponse(sequence, body)
	if profile.Supplier == supplieradapter.ZAISupplier {
		response.Header.Del("X-Request-ID")
	} else if profile.Supplier == supplieradapter.RunPodSupplier {
		response.Header.Del("X-Request-ID")
		response.Header.Set("X-Runpod-Request-Id", "runpod-stream-request")
	}
	return response
}

func TestNearestRankUsesConservativeSmallSampleTails(t *testing.T) {
	values := []float64{2, 100, 4}
	if got := nearestRank(values, .95); got != 100 {
		t.Fatalf("p95=%v", got)
	}
	if got := nearestRank(values, .05); got != 2 {
		t.Fatalf("p5=%v", got)
	}
}

func assertSecretFree(t *testing.T, value any) {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), testSecret) || strings.Contains(string(encoded), "Authorization") {
		t.Fatalf("secret-bearing field escaped: %s", encoded)
	}
}

func jsonResponse(body string) *http.Response {
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}
}

func bufferedResponse(sequence int, model string, withUsage bool) *http.Response {
	usage := "null"
	if withUsage {
		usage = `{"prompt_tokens":12,"completion_tokens":8,"prompt_cache_hit_tokens":0}`
	}
	body := `{"id":"chatcmpl-` + string(rune('a'+sequence)) + `","model":"` + model + `","choices":[{"index":0,"message":{"role":"assistant","content":"one two three four five six seven eight"},"finish_reason":"stop"}],"usage":` + usage + `}`
	response := jsonResponse(body)
	response.Header.Set("X-Request-ID", "ds-buffered-request")
	return response
}

func validSSE(model string, includeDone bool) string {
	body := `data: {"model":"` + model + `","choices":[{"index":0,"delta":{"content":"one two three four"},"finish_reason":null}],"usage":null}` + "\n\n" +
		`data: {"model":"` + model + `","choices":[{"index":0,"delta":{"content":" five six seven eight"},"finish_reason":"stop"}],"usage":{"prompt_tokens":12,"completion_tokens":8,"prompt_cache_hit_tokens":0}}` + "\n\n"
	if includeDone {
		body += "data: [DONE]\n\n"
	}
	return body
}

func streamResponse(sequence int, body string) *http.Response {
	return &http.Response{StatusCode: http.StatusOK,
		Header: http.Header{"Content-Type": {"text/event-stream; charset=utf-8"}, "X-Request-ID": {"ds-stream-request" + string(rune('a'+sequence))}},
		Body:   io.NopCloser(strings.NewReader(body))}
}
