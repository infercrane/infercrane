package modelcheck

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/pricing"
	"github.com/infercrane/infercrane/internal/support"
)

// The files under testdata/huggingface are real Hugging Face responses
// recorded on 2026-11-17, trimmed to the fields the resolver reads
// (siblings shortened, chat templates and long card metadata removed).
var fixtures = map[string]string{
	"microsoft/phi-4":                      "phi-4",
	"meta-llama/Llama-3.2-1B-Instruct":     "llama-3.2-1b-gated",
	"RWKV/rwkv-4-169m-pile":                "rwkv-4-169m",
	"BAAI/bge-small-en-v1.5":               "bge-small-en",
	"Qwen/Qwen2.5-7B-Instruct-AWQ":         "qwen2.5-7b-awq",
	"deepseek-ai/DeepSeek-V2-Lite-Chat":    "deepseek-v2-lite",
	"stabilityai/stable-diffusion-xl-base": "sdxl",
	"TheBloke/Llama-2-7B-GGUF":             "llama-2-7b-gguf",
}

// gatedConfigWithToken stands in for the gated config.json, which cannot be
// recorded without accepting Meta's terms. Its values are Llama 3.2 1B's
// published architecture.
const gatedConfigWithToken = `{"architectures":["LlamaForCausalLM"],"model_type":"llama","num_hidden_layers":16,"hidden_size":2048,"num_attention_heads":32,"num_key_value_heads":8,"head_dim":64,"max_position_embeddings":131072,"torch_dtype":"bfloat16"}`

const testToken = "hf_test_token_value"

type hub struct {
	server   *httptest.Server
	requests atomic.Int64
	mu       sync.Mutex
	headers  []string
	delay    time.Duration
}

func newHub(t *testing.T) *hub {
	t.Helper()
	h := &hub{}
	h.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.requests.Add(1)
		h.mu.Lock()
		h.headers = append(h.headers, r.Header.Get("Authorization"))
		h.mu.Unlock()
		if h.delay > 0 {
			select {
			case <-time.After(h.delay):
			case <-r.Context().Done():
				return
			}
		}
		h.serve(t, w, r)
	}))
	t.Cleanup(h.server.Close)
	return h
}

func (h *hub) serve(t *testing.T, w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	if rest, ok := strings.CutPrefix(path, "/api/models/"); ok {
		repository, revision, _ := strings.Cut(rest, "/revision/")
		if revision == "nope" {
			w.Header().Set("X-Error-Code", "RevisionNotFound")
			http.Error(w, `{"error":"Revision Not Found"}`, http.StatusNotFound)
			return
		}
		if r.URL.Query()["expand[]"] == nil {
			t.Errorf("model info request did not ask for expanded fields: %s", r.URL)
		}
		name, ok := fixtures[repository]
		if !ok {
			http.Error(w, readFixture(t, "missing.info.json"), http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(readFixture(t, name+".info.json")))
		return
	}
	owner, rest, _ := strings.Cut(strings.TrimPrefix(path, "/"), "/")
	name, file, _ := strings.Cut(rest, "/raw/")
	fixture, ok := fixtures[owner+"/"+name]
	if !ok || !strings.HasSuffix(file, "/config.json") {
		http.NotFound(w, r)
		return
	}
	if fixture == "llama-3.2-1b-gated" {
		if r.Header.Get("Authorization") != "Bearer "+testToken {
			http.Error(w, readFixture(t, fixture+".config.json"), http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(gatedConfigWithToken))
		return
	}
	if fixture == "sdxl" {
		http.Error(w, "Entry not found", http.StatusNotFound)
		return
	}
	_, _ = w.Write([]byte(readFixture(t, fixture+".config.json")))
}

func readFixture(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "huggingface", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return string(raw)
}

func newResolver(t *testing.T, h *hub, options ResolverOptions) *Resolver {
	t.Helper()
	options.Endpoint = h.server.URL
	resolver, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	return resolver
}

var testGPUs = []GPU{{Name: "T4", MemoryGiB: 16}, {Name: "L4", MemoryGiB: 24}, {Name: "L40S", MemoryGiB: 48}, {Name: "H100", MemoryGiB: 80}, {Name: "H200", MemoryGiB: 141}}

func check(t *testing.T, resolver *Resolver, request Request, gpus []GPU) Report {
	t.Helper()
	model, err := resolver.Resolve(context.Background(), request)
	if err != nil {
		t.Fatalf("resolve %s: %v", request.Repository, err)
	}
	registries, err := Registries()
	if err != nil {
		t.Fatal(err)
	}
	return Evaluate(model, EvaluateOptions{GPUs: gpus, Registries: registries})
}

func TestRegistrySnapshotMatchesPinnedRuntimes(t *testing.T) {
	registries, err := Registries()
	if err != nil {
		t.Fatal(err)
	}
	vllm, ok := registries.Runtime("vllm")
	if !ok || vllm.Version != support.DefaultRuntimeVersion {
		t.Fatalf("vLLM registry %q does not match the pinned runtime %q; rerun scripts/snapshot-runtime-model-registries.py", vllm.Version, support.DefaultRuntimeVersion)
	}
	sglang, ok := registries.Runtime("sglang")
	if !ok || sglang.Version != support.SGLangRuntimeVersion {
		t.Fatalf("SGLang registry %q does not match the pinned runtime %q; rerun scripts/snapshot-runtime-model-registries.py", sglang.Version, support.SGLangRuntimeVersion)
	}
	if len(vllm.QuantizationMethods) == 0 || !strings.Contains(vllm.SourceURL, vllm.Commit) || !strings.Contains(sglang.SourceURL, sglang.Commit) {
		t.Fatalf("registry snapshot lost its sources: %+v %+v", vllm.SourceURL, sglang.SourceURL)
	}
	if _, err := parseRegistries([]byte(`{"schema_version":"other"}`)); err == nil {
		t.Fatal("a registry document with another schema was accepted")
	}
}

func TestPublicModelIsSupportedAndSized(t *testing.T) {
	h := newHub(t)
	report := check(t, newResolver(t, h, ResolverOptions{}), Request{Repository: "microsoft/phi-4"}, testGPUs)
	if report.Verdict != Supported || report.Task != TaskChat || report.Revision != "2db69c1c3e91a05d2c64a3185acfbaf36f744e25" || report.RequestedRevision != "main" {
		t.Fatalf("unexpected verdict: %+v", report)
	}
	if *report.Architecture.Name != "Phi3ForCausalLM" || *report.Architecture.Runtime != "vllm" || *report.Architecture.RuntimeVersion != support.DefaultRuntimeVersion {
		t.Fatalf("unexpected architecture: %+v", report.Architecture)
	}
	if *report.Weights.Parameters != 14659507200 || *report.Weights.StoredBytes != 2*14659507200 || *report.License != "mit" || report.Access.State != AccessPublic {
		t.Fatalf("unexpected facts: weights=%+v license=%v access=%+v", report.Weights, report.License, report.Access)
	}
	if *report.Config.KVCacheBytesPerToken != 2*40*10*128*2 || *report.Config.ContextLength != 16384 {
		t.Fatalf("unexpected config: %+v", report.Config)
	}
	// 27.3 GiB of weights + 12.5 GiB of KV + 2 GiB fits 90% of an L40S.
	if report.Memory.State != Fits || *report.Memory.GPU != "L40S" || *report.Memory.GPUCount != 1 || report.Memory.ContextTokens != DefaultContextTokens || report.Memory.Concurrency != DefaultConcurrency {
		t.Fatalf("unexpected memory fit: %+v", report.Memory)
	}
	if report.Reviewed || report.PerformanceClaims {
		t.Fatal("an automatic check must not claim review or performance")
	}
	if names := checkNames(report); names != "architecture,weights,memory,license,access" {
		t.Fatalf("unexpected checks: %s", names)
	}
	// A longer context needs more memory and moves to a larger GPU.
	model, _ := newResolver(t, h, ResolverOptions{}).Resolve(context.Background(), Request{Repository: "microsoft/phi-4"})
	registries, _ := Registries()
	larger := Evaluate(model, EvaluateOptions{ContextTokens: 16384, Concurrency: 16, GPUs: testGPUs, Registries: registries})
	if larger.Memory.State != Fits || *larger.Memory.GPU != "H200" || *larger.Memory.GPUCount != 1 || larger.Memory.Layouts[1].GPU != "L40S" || larger.Memory.Layouts[1].GPUCount != 2 {
		t.Fatalf("unexpected larger fit: %+v", larger.Memory)
	}
	// Asking for more context than the model declares is capped at its length.
	capped := Evaluate(model, EvaluateOptions{ContextTokens: 65536, Concurrency: 1, GPUs: testGPUs, Registries: registries})
	if capped.Memory.ContextTokens != 16384 {
		t.Fatalf("context was not capped at the declared length: %d", capped.Memory.ContextTokens)
	}
}

func TestGatedModelNeedsTokenAndTokenReadsStayScoped(t *testing.T) {
	h := newHub(t)
	resolver := newResolver(t, h, ResolverOptions{})
	public := check(t, resolver, Request{Repository: "meta-llama/Llama-3.2-1B-Instruct"}, testGPUs)
	if public.Verdict != Unknown || public.Access.State != AccessTokenRequired || !public.Access.Gated || *public.Access.GatedMode != "manual" {
		t.Fatalf("a gated model without a token must stay unknown: %+v", public)
	}
	if public.Architecture.State != Supported || public.Config.State != ConfigTokenRequired || public.Memory.State != Unknown || !strings.Contains(public.Memory.Reason, "token") {
		t.Fatalf("unexpected gated facts: arch=%+v config=%+v memory=%+v", public.Architecture, public.Config, public.Memory)
	}
	withToken := check(t, resolver, Request{Repository: "meta-llama/Llama-3.2-1B-Instruct", Token: testToken, Scope: "tenant-a/secret-1"}, testGPUs)
	if withToken.Verdict != Supported || withToken.Access.State != AccessTokenUsed || withToken.Memory.State != Fits {
		t.Fatalf("a token read must finish the check: %+v", withToken)
	}
	// The token read is cached under its own scope; a public request still
	// cannot see the gated config.json.
	again := check(t, resolver, Request{Repository: "meta-llama/Llama-3.2-1B-Instruct", Revision: public.Revision}, testGPUs)
	if again.Access.State != AccessTokenRequired {
		t.Fatalf("a token read leaked into the public cache: %+v", again.Access)
	}
	other := check(t, resolver, Request{Repository: "meta-llama/Llama-3.2-1B-Instruct", Revision: public.Revision, Token: "hf_other", Scope: "tenant-b/secret-9"}, testGPUs)
	if other.Access.State != AccessTokenRequired || !other.Access.TokenUsed {
		t.Fatalf("one tenant's token read served another tenant: %+v", other.Access)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, header := range h.headers {
		if header != "" && header != "Bearer "+testToken && header != "Bearer hf_other" {
			t.Fatalf("unexpected Authorization header %q", header)
		}
	}
	if !strings.Contains(strings.Join(h.headers, ","), ",,") && h.headers[0] != "" {
		t.Fatalf("public reads sent a token: %q", h.headers)
	}
	encoded, _ := json.Marshal(withToken)
	if strings.Contains(string(encoded), testToken) {
		t.Fatal("the report contains the token")
	}
}

func TestUnsupportedModelsSayWhy(t *testing.T) {
	h := newHub(t)
	resolver := newResolver(t, h, ResolverOptions{})
	for _, test := range []struct{ repository, reason string }{
		{"RWKV/rwkv-4-169m-pile", "RwkvForCausalLM is not in the model registry of vLLM " + support.DefaultRuntimeVersion},
		{"stabilityai/stable-diffusion-xl-base", "Hugging Face lists this one as text-to-image"},
		{"TheBloke/Llama-2-7B-GGUF", "GGUF files only"},
	} {
		report := check(t, resolver, Request{Repository: test.repository}, testGPUs)
		if report.Verdict != Unsupported || !strings.Contains(report.Reason, test.reason) {
			t.Fatalf("%s: verdict %s reason %q", test.repository, report.Verdict, report.Reason)
		}
	}
}

func TestEmbeddingQuantizedAndLatentAttentionModels(t *testing.T) {
	h := newHub(t)
	resolver := newResolver(t, h, ResolverOptions{})
	embedding := check(t, resolver, Request{Repository: "BAAI/bge-small-en-v1.5"}, testGPUs)
	// 32-bit weights are served at 16 bits, and embeddings keep no KV cache.
	if embedding.Verdict != Supported || embedding.Task != TaskEmbeddings || *embedding.Memory.GPU != "T4" || *embedding.Memory.WeightsBytes != 2*33360000+8*512 || *embedding.Memory.KVCacheBytes != 0 {
		t.Fatalf("unexpected embedding check: %+v %+v", embedding, embedding.Memory)
	}
	awq := check(t, resolver, Request{Repository: "Qwen/Qwen2.5-7B-Instruct-AWQ"}, testGPUs)
	if awq.Verdict != Supported || *awq.Weights.Quantization != "awq" || *awq.Weights.StoredBytes != 4*6525288448+2*1090328064 {
		t.Fatalf("unexpected quantized check: %+v", awq.Weights)
	}
	if names := checkNames(awq); names != "architecture,weights,quantization,memory,license,access" {
		t.Fatalf("unexpected quantized checks: %s", names)
	}
	latent := check(t, resolver, Request{Repository: "deepseek-ai/DeepSeek-V2-Lite-Chat"}, testGPUs)
	if latent.Verdict != Supported || *latent.Config.KVCacheBytesPerToken != 27*(512+64)*2 || !latent.Architecture.CustomCode {
		t.Fatalf("unexpected latent attention check: %+v %+v", latent.Config, latent.Architecture)
	}
	if !strings.Contains(checkNames(latent), "custom_code") {
		t.Fatalf("custom code was not shown: %s", checkNames(latent))
	}
}

func TestModelTooLargeForAnyPricedLayout(t *testing.T) {
	registries, _ := Registries()
	model := Model{Repository: "acme/huge", Commit: strings.Repeat("a", 40), PipelineTag: "text-generation", Architectures: []string{"LlamaForCausalLM"},
		Weights: &Weights{Parameters: 2_000_000_000_000, ByDType: map[string]int64{"BF16": 2_000_000_000_000}},
		Config:  &Config{Layers: 80, AttentionHeads: 64, KVHeads: 8, HeadDim: 128, ContextLength: 8192}, ConfigState: ConfigRead}
	report := Evaluate(model, EvaluateOptions{GPUs: testGPUs, Registries: registries})
	if report.Verdict != Unsupported || report.Memory.State != DoesNotFit || !strings.Contains(report.Reason, "8 × H200") {
		t.Fatalf("unexpected fit: %+v", report.Memory)
	}
	unpriced := Evaluate(model, EvaluateOptions{Registries: registries})
	if unpriced.Memory.State != Unknown || unpriced.Verdict != Unknown {
		t.Fatalf("no priced GPU must leave the fit unknown: %+v", unpriced.Memory)
	}
	// Tensor parallelism must divide the attention heads.
	odd := Model{Repository: "acme/odd", Commit: strings.Repeat("b", 40), PipelineTag: "text-generation", Architectures: []string{"LlamaForCausalLM"},
		Weights: &Weights{Parameters: 30_000_000_000, ByDType: map[string]int64{"BF16": 30_000_000_000}},
		Config:  &Config{Layers: 48, AttentionHeads: 6, KVHeads: 6, HeadDim: 128, ContextLength: 8192}, ConfigState: ConfigRead}
	fit := Evaluate(odd, EvaluateOptions{GPUs: []GPU{{Name: "L40S", MemoryGiB: 48}}, Registries: registries}).Memory
	if fit.State != Fits || *fit.GPUCount != 2 {
		t.Fatalf("expected two L40S for 60 GB of weights: %+v", fit)
	}
	for _, layout := range fit.Layouts {
		if 6%layout.GPUCount != 0 {
			t.Fatalf("layout %+v does not divide the attention heads", layout)
		}
	}
}

func TestResolverErrorsAndCache(t *testing.T) {
	h := newHub(t)
	resolver := newResolver(t, h, ResolverOptions{})
	_, err := resolver.Resolve(context.Background(), Request{Repository: "infercrane/does-not-exist-xyz"})
	var checkErr *Error
	if !errors.As(err, &checkErr) || checkErr.Code != CodeModelNotFound {
		t.Fatalf("expected model_not_found, got %v", err)
	}
	if _, err = resolver.Resolve(context.Background(), Request{Repository: "microsoft/phi-4", Revision: "nope"}); !errors.As(err, &checkErr) || checkErr.Code != CodeRevisionNotFound {
		t.Fatalf("expected revision_not_found, got %v", err)
	}
	for _, request := range []Request{{Repository: "phi-4"}, {Repository: "a/b/c"}, {Repository: "a--b/c"}, {Repository: "microsoft/phi-4", Revision: "../main"}} {
		if _, err = resolver.Resolve(context.Background(), request); !errors.As(err, &checkErr) || checkErr.Code != CodeInvalidModel {
			t.Fatalf("%+v: expected invalid_model, got %v", request, err)
		}
	}
	if _, err = resolver.Resolve(context.Background(), Request{Repository: "microsoft/phi-4", Token: "x"}); err == nil {
		t.Fatal("a token read without a cache scope was accepted")
	}
	before := h.requests.Load()
	first, err := resolver.Resolve(context.Background(), Request{Repository: "microsoft/phi-4"})
	if err != nil || first.Cached {
		t.Fatalf("first read: %+v %v", first, err)
	}
	if h.requests.Load()-before != 2 {
		t.Fatalf("expected one info and one config request, got %d", h.requests.Load()-before)
	}
	before = h.requests.Load()
	for _, revision := range []string{"main", first.Commit} {
		cached, cacheErr := resolver.Resolve(context.Background(), Request{Repository: "microsoft/phi-4", Revision: revision})
		if cacheErr != nil || !cached.Cached || cached.Commit != first.Commit || cached.RequestedRevision != revision {
			t.Fatalf("cached read %s: %+v %v", revision, cached, cacheErr)
		}
	}
	if h.requests.Load() != before {
		t.Fatal("a cached commit reached Hugging Face")
	}
}

func TestResolverRefTTLAndEviction(t *testing.T) {
	h := newHub(t)
	now := time.Date(2026, 11, 17, 0, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	resolver := newResolver(t, h, ResolverOptions{RefTTL: time.Minute, MaxEntries: 1, Now: clock})
	if _, err := resolver.Resolve(context.Background(), Request{Repository: "microsoft/phi-4"}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	now = now.Add(2 * time.Minute)
	mu.Unlock()
	before := h.requests.Load()
	if _, err := resolver.Resolve(context.Background(), Request{Repository: "microsoft/phi-4"}); err != nil {
		t.Fatal(err)
	}
	if h.requests.Load() == before {
		t.Fatal("an expired branch name was not re-pinned")
	}
	if _, err := resolver.Resolve(context.Background(), Request{Repository: "BAAI/bge-small-en-v1.5"}); err != nil {
		t.Fatal(err)
	}
	resolver.mu.Lock()
	entries := len(resolver.commits)
	resolver.mu.Unlock()
	if entries != 1 {
		t.Fatalf("cache kept %d entries past its bound", entries)
	}
}

func TestResolverTimeoutAndRedirects(t *testing.T) {
	h := newHub(t)
	h.delay = time.Second
	resolver := newResolver(t, h, ResolverOptions{Timeout: 50 * time.Millisecond})
	_, err := resolver.Resolve(context.Background(), Request{Repository: "microsoft/phi-4"})
	var checkErr *Error
	if !errors.As(err, &checkErr) || checkErr.Code != CodeUnavailable || !checkErr.Timeout {
		t.Fatalf("expected a timeout, got %v", err)
	}
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("the resolver followed a redirect to another host with Authorization %q", r.Header.Get("Authorization"))
	}))
	defer elsewhere.Close()
	redirecting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	defer redirecting.Close()
	resolver, err = New(ResolverOptions{Endpoint: redirecting.URL})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = resolver.Resolve(context.Background(), Request{Repository: "microsoft/phi-4", Token: testToken, Scope: "tenant"}); !errors.As(err, &checkErr) || checkErr.Code != CodeUnavailable {
		t.Fatalf("expected the cross-host redirect to be refused, got %v", err)
	}
	if _, err = New(ResolverOptions{Endpoint: "ftp://huggingface.co"}); err == nil {
		t.Fatal("a non-HTTP endpoint was accepted")
	}
}

func TestPricedGPUsAndLoadBounds(t *testing.T) {
	now := time.Date(2026, 11, 17, 0, 0, 0, 0, time.UTC)
	prices := map[pricing.Request]pricing.Estimate{
		{Cloud: "runpod", Region: "global", GPU: "NVIDIA L40S", GPUCount: 1, Replicas: 1}:                                  {Hourly: 0.86, Currency: "USD", ObservedAt: now, StaleAfter: time.Hour},
		{Cloud: "runpod", Region: "global", GPU: "NVIDIA H100 80GB HBM3", GPUCount: 1, Replicas: 1}:                        {Hourly: 2.99, Currency: "USD", ObservedAt: now, StaleAfter: time.Hour},
		{Cloud: "runpod", Region: "global", GPU: "NVIDIA H200", GPUCount: 1, Replicas: 1}:                                  {Hourly: 3.99, Currency: "USD", ObservedAt: now.Add(-2 * time.Hour), StaleAfter: time.Hour},
		{Cloud: "aws", Region: "us-east-1", GPU: "NVIDIA L4", GPUCount: 1, Replicas: 1}:                                    {Hourly: 0.80, Currency: "USD", ObservedAt: now, StaleAfter: time.Hour},
		{Cloud: "runpod", Region: "global", GPU: "NVIDIA RTX PRO 6000 Blackwell Server Edition", GPUCount: 1, Replicas: 2}: {Hourly: 3.0, Currency: "USD", ObservedAt: now, StaleAfter: time.Hour},
	}
	got := PricedGPUs(prices, now)
	names := make([]string, 0, len(got))
	for _, gpu := range got {
		names = append(names, gpu.Name)
	}
	if strings.Join(names, ",") != "L4,L40S,H100" {
		t.Fatalf("unexpected priced GPUs: %v", names)
	}
	for _, test := range []struct{ context, concurrency, declared, wantContext, wantConcurrency int }{
		{0, 0, 0, DefaultContextTokens, DefaultConcurrency},
		{1, 5000, 0, MinContextTokens, MaxConcurrency},
		{32768, 4, 4096, 4096, 4},
	} {
		gotContext, gotConcurrency := NormalizeLoad(test.context, test.concurrency, test.declared)
		if gotContext != test.wantContext || gotConcurrency != test.wantConcurrency {
			t.Fatalf("NormalizeLoad(%d,%d,%d) = %d,%d", test.context, test.concurrency, test.declared, gotContext, gotConcurrency)
		}
	}
}

func TestParseConfigNestedAndInvalid(t *testing.T) {
	config, err := ParseConfig([]byte(`{"architectures":["Gemma3ForConditionalGeneration"],"model_type":"gemma3","text_config":{"num_hidden_layers":34,"hidden_size":2560,"num_attention_heads":8,"num_key_value_heads":4,"head_dim":256,"max_position_embeddings":131072},"torch_dtype":"bfloat16","auto_map":{}}`))
	if err != nil || config.Layers != 34 || config.KVHeads != 4 || config.ContextLength != 131072 || config.DType != "bfloat16" || config.CustomCode {
		t.Fatalf("nested config: %+v %v", config, err)
	}
	if config.KVCacheBytesPerToken() != 2*34*4*256*2 {
		t.Fatalf("unexpected KV size %d", config.KVCacheBytesPerToken())
	}
	for _, raw := range []string{`not json`, `{}`} {
		if _, err := ParseConfig([]byte(raw)); err == nil {
			t.Fatalf("%q parsed", raw)
		}
	}
	if (Config{Layers: 2}).KVCacheBytesPerToken() != 0 {
		t.Fatal("a config without heads produced a KV size")
	}
	if Parameters(14659507200) != "14.7B" || Parameters(335_000_000) != "335M" || Bytes(29_319_014_400) != "29.3 GB" || Bytes(66_000_000) != "66 MB" {
		t.Fatal("unexpected formatting")
	}
}

func checkNames(report Report) string {
	names := make([]string, 0, len(report.Checks))
	for _, item := range report.Checks {
		names = append(names, item.Name)
	}
	return strings.Join(names, ",")
}
