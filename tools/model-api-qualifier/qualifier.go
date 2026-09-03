package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/infercrane/infercrane/internal/modelapisupply"
	"github.com/infercrane/infercrane/internal/supplieradapter"
)

const (
	qualificationSchema = "infercrane.supplier-qualification.raw/v1"
	qualificationProto  = "openai"
	qualificationPrompt = "Return exactly eight lowercase English words and no punctuation."
	qwen38Revision      = "017b9c7af6b5689d5dd426a76e0bc077eb5ca20a"
)

const (
	profileDeepSeekV4Flash = "deepseek-v4-flash"
	profileGLM52           = "glm-5.2"
	profileGLM53           = "glm-5.3"
	profileGLM53Flash      = "glm-5.3-flash"
	profileQwen38RunPod    = "qwen3.8-27b-runpod"
)

var runPodLBEndpointPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{2,127}$`)

type qualificationProfile struct {
	Name               string
	Supplier           string
	AdapterName        string
	SupplierModelID    string
	CredentialEnv      string
	ExpectedRevision   string
	RevisionAuthority  string
	RevisionCheck      string
	Scope              string
	DefaultBaseURL     string
	MaximumOutputToken int
	NewAdapter         func(*http.Client) supplieradapter.Adapter
}

func qualificationProfiles() map[string]qualificationProfile {
	return map[string]qualificationProfile{
		profileDeepSeekV4Flash: {
			Name: profileDeepSeekV4Flash, Supplier: supplieradapter.DeepSeekSupplier, AdapterName: supplieradapter.DeepSeekAdapterName,
			SupplierModelID: supplieradapter.DeepSeekV4FlashModelID, CredentialEnv: "DEEPSEEK_API_KEY",
			ExpectedRevision: supplieradapter.DeepSeekV4FlashRevision, RevisionAuthority: "https://api-docs.deepseek.com/quick_start/pricing/",
			RevisionCheck: "operator-pinned official model table; alias responses verified by the production adapter",
			Scope:         "deepseek-mvp:inventory+buffered-chat-completions+streaming-chat-completions", DefaultBaseURL: supplieradapter.DeepSeekBaseURL,
			MaximumOutputToken: 1024, NewAdapter: func(client *http.Client) supplieradapter.Adapter { return supplieradapter.NewDeepSeekAdapter(client) },
		},
		profileGLM52:      zaiQualificationProfile(profileGLM52, supplieradapter.ZAIGLM52ModelID, "https://docs.z.ai/guides/llm/glm-5.2"),
		profileGLM53:      zaiQualificationProfile(profileGLM53, supplieradapter.ZAIGLM53ModelID, "https://docs.z.ai/guides/llm/glm-5.3"),
		profileGLM53Flash: zaiQualificationProfile(profileGLM53Flash, supplieradapter.ZAIGLM53FlashModelID, "https://docs.z.ai/guides/vlm/glm-5.3-flash"),
		profileQwen38RunPod: {
			Name: profileQwen38RunPod, Supplier: supplieradapter.RunPodSupplier, AdapterName: supplieradapter.RunPodSGLangLBAdapterName,
			SupplierModelID: supplieradapter.RunPodQwen38SupplierModelID, CredentialEnv: "RUNPOD_API_KEY", ExpectedRevision: qwen38Revision,
			RevisionAuthority: "https://huggingface.co/Qwen/Qwen3.8-27B-FP8/commit/" + qwen38Revision,
			RevisionCheck:     "immutable Hugging Face checkpoint pinned by the RunPod SGLang deployment recipe; exact served model verified by the production adapter",
			Scope:             "runpod-sglang-mvp:inventory+buffered-chat-completions+streaming-chat-completions", MaximumOutputToken: supplieradapter.RunPodQwen38MaxOutputTokens,
			NewAdapter: func(client *http.Client) supplieradapter.Adapter {
				return supplieradapter.NewRunPodSGLangLBAdapter(client)
			},
		},
	}
}

func zaiQualificationProfile(name, modelID, authority string) qualificationProfile {
	return qualificationProfile{
		Name: name, Supplier: supplieradapter.ZAISupplier, AdapterName: supplieradapter.ZAIAdapterName,
		SupplierModelID: modelID, CredentialEnv: "ZAI_API_KEY", ExpectedRevision: modelID, RevisionAuthority: authority,
		RevisionCheck: "operator-pinned supplier API model identity; exact response identity verified by the production adapter",
		Scope:         "zai-mvp:authenticated-model-probe+buffered-chat-completions+streaming-chat-completions", DefaultBaseURL: supplieradapter.ZAIBaseURL,
		MaximumOutputToken: 1024, NewAdapter: func(client *http.Client) supplieradapter.Adapter { return supplieradapter.NewZAIAdapter(client) },
	}
}

func resolveQualificationProfile(name string) (qualificationProfile, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		name = profileDeepSeekV4Flash
	}
	profile, ok := qualificationProfiles()[name]
	if !ok {
		return qualificationProfile{}, fmt.Errorf("unsupported --profile %q", name)
	}
	return profile, nil
}

type qualifierConfig struct {
	Profile             string
	EndpointOrigin      string
	Region              string
	OfferID             string
	OfferVersion        int64
	QualificationID     string
	TupleKey            string
	ExpectedRevision    string
	EvidenceRef         string
	EvidenceOutput      string
	QualificationOutput string
	SamplesPerMode      int
	MaxOutputTokens     int
	RequestTimeout      time.Duration
	TotalTimeout        time.Duration
	ValidFor            time.Duration
	MaxStreamBytes      int64
	ConfirmLive         bool
}

func (c qualifierConfig) Validate() error {
	profile, err := resolveQualificationProfile(c.Profile)
	if err != nil {
		return err
	}
	required := map[string]string{
		"--offer-id": c.OfferID, "--qualification-id": c.QualificationID,
		"--tuple-key": c.TupleKey, "--expected-revision": c.ExpectedRevision,
		"--evidence-ref": c.EvidenceRef, "--evidence-output": c.EvidenceOutput,
		"--qualification-output": c.QualificationOutput,
	}
	for name, value := range required {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("%s is required", name)
		}
		if strings.ContainsAny(value, "\r\n\x00") {
			return fmt.Errorf("%s contains invalid characters", name)
		}
	}
	if c.OfferVersion <= 0 {
		return errors.New("--offer-version must be positive")
	}
	if c.ExpectedRevision != profile.ExpectedRevision {
		return fmt.Errorf("revision drift: profile %s qualifies only %s", profile.Name, profile.ExpectedRevision)
	}
	if c.SamplesPerMode < 1 || c.SamplesPerMode > 20 {
		return errors.New("--samples-per-mode must be between 1 and 20")
	}
	if c.MaxOutputTokens < 1 || c.MaxOutputTokens > profile.MaximumOutputToken {
		return fmt.Errorf("--max-output-tokens must be between 1 and %d for profile %s", profile.MaximumOutputToken, profile.Name)
	}
	if c.RequestTimeout <= 0 || c.RequestTimeout > 5*time.Minute {
		return errors.New("--request-timeout must be positive and at most 5m")
	}
	if c.TotalTimeout <= 0 || c.TotalTimeout > 30*time.Minute || c.TotalTimeout < c.RequestTimeout {
		return errors.New("--total-timeout must be between the request timeout and 30m")
	}
	if c.ValidFor <= 0 || c.ValidFor > 24*time.Hour {
		return errors.New("--valid-for must be positive and at most 24h")
	}
	if c.MaxStreamBytes < 1<<20 || c.MaxStreamBytes > 32<<20 {
		return errors.New("--max-stream-bytes must be between 1MiB and 32MiB")
	}
	if !c.ConfirmLive {
		return errors.New("--confirm-live is required; qualification makes billable supplier calls")
	}
	if profile.Name == profileQwen38RunPod {
		if err := validateRunPodLBOrigin(c.EndpointOrigin); err != nil {
			return err
		}
		if strings.TrimSpace(c.Region) == "" {
			return errors.New("--region is required for the RunPod profile")
		}
	} else if strings.TrimSpace(c.EndpointOrigin) != "" {
		return errors.New("--endpoint-origin is accepted only by the RunPod profile")
	}
	region := "global"
	if profile.Name == profileQwen38RunPod {
		region = strings.TrimSpace(c.Region)
	}
	expectedTuple := strings.Join([]string{profile.Supplier, profile.SupplierModelID, qualificationProto, region}, "|")
	if c.TupleKey != expectedTuple {
		return fmt.Errorf("--tuple-key must exactly match %q for profile %s", expectedTuple, profile.Name)
	}
	return nil
}

func validateRunPodLBOrigin(value string) error {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil || parsed.Scheme != "https" || parsed.User != nil || parsed.Port() != "" || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.RawPath != "" || strings.Trim(parsed.Path, "/") != "" {
		return errors.New("--endpoint-origin must be an exact HTTPS RunPod load-balanced endpoint origin")
	}
	host := strings.TrimSuffix(parsed.Hostname(), ".")
	if !strings.HasSuffix(host, ".api.runpod.ai") {
		return errors.New("--endpoint-origin must end in .api.runpod.ai")
	}
	endpointID := strings.TrimSuffix(host, ".api.runpod.ai")
	if !runPodLBEndpointPattern.MatchString(endpointID) || strings.Contains(endpointID, ".") {
		return errors.New("--endpoint-origin contains an invalid RunPod endpoint id")
	}
	return nil
}

type rawEvidence struct {
	SchemaVersion string             `json:"schema_version"`
	Status        string             `json:"status"`
	Supplier      string             `json:"supplier"`
	Adapter       string             `json:"adapter"`
	Protocol      string             `json:"protocol"`
	Region        string             `json:"region"`
	Model         modelIdentity      `json:"model"`
	Limits        runLimits          `json:"limits"`
	StartedAt     time.Time          `json:"started_at"`
	FinishedAt    time.Time          `json:"finished_at"`
	Inventory     inventoryEvidence  `json:"inventory"`
	Samples       []sampleEvidence   `json:"samples"`
	Summary       performanceSummary `json:"summary"`
}

type modelIdentity struct {
	SupplierModelID    string `json:"supplier_model_id"`
	ExpectedRevision   string `json:"expected_revision"`
	RevisionAuthority  string `json:"revision_authority"`
	RevisionCheck      string `json:"revision_check"`
	TargetOriginSHA256 string `json:"target_origin_sha256"`
}

type runLimits struct {
	SamplesPerMode   int    `json:"samples_per_mode"`
	RequestTimeoutMS int64  `json:"request_timeout_ms"`
	TotalTimeoutMS   int64  `json:"total_timeout_ms"`
	MaxOutputTokens  int    `json:"max_output_tokens"`
	MaxStreamBytes   int64  `json:"max_stream_bytes"`
	PromptSHA256     string `json:"prompt_sha256"`
}

type inventoryEvidence struct {
	ObservedAt     time.Time `json:"observed_at"`
	Access         string    `json:"access"`
	Availability   string    `json:"availability"`
	Health         string    `json:"health"`
	TargetPresent  bool      `json:"target_present"`
	ModelIDs       []string  `json:"model_ids"`
	ModelIDsSHA256 string    `json:"model_ids_sha256"`
}

type sampleEvidence struct {
	Mode                     string   `json:"mode"`
	Sequence                 int      `json:"sequence"`
	Succeeded                bool     `json:"succeeded"`
	ModelID                  string   `json:"model_id"`
	ResponseIDPresent        bool     `json:"response_id_present,omitempty"`
	SupplierRequestIDPresent bool     `json:"supplier_request_id_present"`
	InputTokens              int64    `json:"input_tokens"`
	OutputTokens             int64    `json:"output_tokens"`
	CachedInputTokens        *int64   `json:"cached_input_tokens,omitempty"`
	LatencyMS                float64  `json:"latency_ms"`
	TTFTMS                   *float64 `json:"ttft_ms,omitempty"`
	OutputTokensPerSecond    *float64 `json:"output_tokens_per_second,omitempty"`
	FinishReason             string   `json:"finish_reason"`
	WireSHA256               string   `json:"wire_sha256"`
	WireBytes                int64    `json:"wire_bytes"`
	TerminalDoneFrames       int      `json:"terminal_done_frames,omitempty"`
}

type performanceSummary struct {
	BufferedSamples  int     `json:"buffered_samples"`
	StreamingSamples int     `json:"streaming_samples"`
	TTFTP95MS        float64 `json:"ttft_p95_ms"`
	OutputTokensP5   float64 `json:"output_tokens_p5"`
}

type qualificationManifest struct {
	OfferID      string                               `json:"offer_id"`
	OfferVersion int64                                `json:"offer_version"`
	Evidence     modelapisupply.QualificationEvidence `json:"evidence"`
}

type memoryCredential struct {
	reference string
	value     []byte
}

func (r memoryCredential) Resolve(_ context.Context, reference string) ([]byte, error) {
	if reference != r.reference || len(r.value) == 0 {
		return nil, errors.New("supplier credential is unavailable")
	}
	return append([]byte(nil), r.value...), nil
}

type qualifier struct {
	client  *http.Client
	adapter supplieradapter.Adapter
	profile qualificationProfile
	target  supplieradapter.Target
	secret  memoryCredential
	now     func() time.Time
}

func runQualification(ctx context.Context, cfg qualifierConfig, client *http.Client, credential []byte) (rawEvidence, qualificationManifest, error) {
	if err := cfg.Validate(); err != nil {
		return rawEvidence{}, qualificationManifest{}, err
	}
	if client == nil || client.Transport == nil || len(credential) == 0 {
		return rawEvidence{}, qualificationManifest{}, errors.New("qualified HTTP client and credential are required")
	}
	profile, err := resolveQualificationProfile(cfg.Profile)
	if err != nil {
		return rawEvidence{}, qualificationManifest{}, err
	}
	target, err := profile.target(cfg)
	if err != nil {
		return rawEvidence{}, qualificationManifest{}, err
	}
	credentialReference := "env://" + profile.CredentialEnv
	q := qualifier{client: client, adapter: profile.NewAdapter(client), profile: profile, target: target,
		secret: memoryCredential{reference: credentialReference, value: credential}, now: time.Now}
	started := q.now().UTC().Truncate(time.Microsecond)
	raw := rawEvidence{
		SchemaVersion: qualificationSchema, Status: "passed", Supplier: profile.Supplier,
		Adapter: profile.AdapterName, Protocol: qualificationProto, Region: target.Region, StartedAt: started,
		Model: modelIdentity{SupplierModelID: profile.SupplierModelID, ExpectedRevision: cfg.ExpectedRevision,
			RevisionAuthority: profile.RevisionAuthority, RevisionCheck: profile.RevisionCheck, TargetOriginSHA256: digestBytes([]byte(target.BaseURL))},
		Limits: runLimits{SamplesPerMode: cfg.SamplesPerMode, RequestTimeoutMS: cfg.RequestTimeout.Milliseconds(),
			TotalTimeoutMS: cfg.TotalTimeout.Milliseconds(), MaxOutputTokens: cfg.MaxOutputTokens, MaxStreamBytes: cfg.MaxStreamBytes,
			PromptSHA256: digestBytes([]byte(qualificationPrompt))},
	}

	inventory, err := q.inventory(ctx, cfg)
	if err != nil {
		return rawEvidence{}, qualificationManifest{}, err
	}
	raw.Inventory = inventory
	for sequence := 1; sequence <= cfg.SamplesPerMode; sequence++ {
		sample, runErr := q.buffered(ctx, cfg, sequence, started)
		if runErr != nil {
			return rawEvidence{}, qualificationManifest{}, runErr
		}
		raw.Samples = append(raw.Samples, sample)
	}
	for sequence := 1; sequence <= cfg.SamplesPerMode; sequence++ {
		sample, runErr := q.streaming(ctx, cfg, sequence, started)
		if runErr != nil {
			return rawEvidence{}, qualificationManifest{}, runErr
		}
		raw.Samples = append(raw.Samples, sample)
	}
	raw.FinishedAt = q.now().UTC().Truncate(time.Microsecond)
	raw.Summary = summarize(raw.Samples)
	canonical, err := json.Marshal(raw)
	if err != nil {
		return rawEvidence{}, qualificationManifest{}, errors.New("raw evidence could not be encoded")
	}
	ttft, throughput := raw.Summary.TTFTP95MS, raw.Summary.OutputTokensP5
	manifest := qualificationManifest{
		OfferID: cfg.OfferID, OfferVersion: cfg.OfferVersion,
		Evidence: modelapisupply.QualificationEvidence{
			ID: cfg.QualificationID, State: modelapisupply.QualificationQualified, TupleKey: cfg.TupleKey,
			Protocol: qualificationProto, Region: target.Region, Capabilities: []string{"chat-completions", "streaming"},
			Scope: profile.Scope + ";revision=" + cfg.ExpectedRevision + ";target_origin_sha256=" + raw.Model.TargetOriginSHA256, EvidenceRef: cfg.EvidenceRef,
			EvidenceDigest: "sha256:" + digestBytes(canonical), ObservedAt: raw.FinishedAt,
			ValidUntil: raw.FinishedAt.Add(cfg.ValidFor).UTC().Truncate(time.Microsecond), SampleCount: len(raw.Samples),
			TTFTP95MS: &ttft, OutputTokensP5: &throughput,
		},
	}
	if err = manifest.Evidence.Validate(); err != nil {
		return rawEvidence{}, qualificationManifest{}, errors.New("qualification manifest failed validation")
	}
	return raw, manifest, nil
}

func (p qualificationProfile) target(cfg qualifierConfig) (supplieradapter.Target, error) {
	baseURL := p.DefaultBaseURL
	region := "global"
	if p.Name == profileQwen38RunPod {
		baseURL = strings.TrimSpace(cfg.EndpointOrigin)
		region = strings.TrimSpace(cfg.Region)
	}
	if strings.TrimSpace(baseURL) == "" || strings.TrimSpace(region) == "" {
		return supplieradapter.Target{}, errors.New("qualification target is incomplete")
	}
	return supplieradapter.Target{Supplier: p.Supplier, BaseURL: baseURL, SupplierModelID: p.SupplierModelID,
		Region: region, CredentialReference: "env://" + p.CredentialEnv}, nil
}

func (q qualifier) inventory(ctx context.Context, cfg qualifierConfig) (inventoryEvidence, error) {
	callCtx, cancel := context.WithTimeout(ctx, cfg.RequestTimeout)
	defer cancel()
	observation, err := q.adapter.Probe(callCtx, q.target, q.secret)
	if err != nil {
		return inventoryEvidence{}, safeStageError("inventory probe", err)
	}
	ids := make([]string, 0, len(observation.Inventory))
	targetCount := 0
	for _, item := range observation.Inventory {
		id := strings.TrimSpace(item.SupplierModelID)
		if id == "" || !item.Available {
			return inventoryEvidence{}, errors.New("inventory probe failed closed on an invalid model item")
		}
		ids = append(ids, id)
		if id == q.profile.SupplierModelID {
			targetCount++
		}
	}
	sort.Strings(ids)
	if observation.Access != "authorized" || observation.Availability != "available" || observation.Health != "healthy" || targetCount != 1 {
		return inventoryEvidence{}, errors.New("inventory probe did not prove one exact available target model")
	}
	encoded, _ := json.Marshal(ids)
	return inventoryEvidence{ObservedAt: observation.ObservedAt.UTC().Truncate(time.Microsecond), Access: observation.Access,
		Availability: observation.Availability, Health: observation.Health, TargetPresent: true, ModelIDs: ids,
		ModelIDsSHA256: digestBytes(encoded)}, nil
}

func (q qualifier) qualificationRequest(cfg qualifierConfig, id string, stream bool) supplieradapter.Request {
	maximum := cfg.MaxOutputTokens
	temperature := 0.0
	return supplieradapter.Request{
		ID: id, Operation: supplieradapter.OperationChatCompletions, ModelID: "qualification-target",
		Messages:        []supplieradapter.Message{{Role: "user", Content: []supplieradapter.ContentPart{{Type: "text", Text: qualificationPrompt}}}},
		MaxOutputTokens: &maximum, Temperature: &temperature, Stream: stream,
	}
}

func (q qualifier) buffered(ctx context.Context, cfg qualifierConfig, sequence int, runStarted time.Time) (sampleEvidence, error) {
	callCtx, cancel := context.WithTimeout(ctx, cfg.RequestTimeout)
	defer cancel()
	requestID := fmt.Sprintf("ic-qual-%d-b-%02d", runStarted.UnixMicro(), sequence)
	request, err := q.adapter.BuildRequest(callCtx, q.target, q.qualificationRequest(cfg, requestID, false), q.secret)
	if err != nil {
		return sampleEvidence{}, safeStageError("buffered request construction", err)
	}
	started := q.now()
	response, err := q.client.Do(request)
	if err != nil {
		if response != nil && response.Body != nil {
			response.Body.Close()
		}
		return sampleEvidence{}, errors.New("buffered supplier transport failed")
	}
	audit := newHashingReadCloser(response.Body)
	response.Body = audit
	decoded, err := q.adapter.DecodeResponse(callCtx, response)
	finished := q.now()
	if err != nil {
		return sampleEvidence{}, safeStageError("buffered response contract", err)
	}
	input, output, err := requireCompleteUsage(decoded.Usage)
	if err != nil || decoded.ModelID != q.profile.SupplierModelID || decoded.ID == "" || len(decoded.Choices) != 1 || decoded.Choices[0].FinishReason == "" {
		return sampleEvidence{}, errors.New("buffered response failed exact identity, usage, or terminal contract")
	}
	return sampleEvidence{Mode: "buffered", Sequence: sequence, Succeeded: true, ModelID: decoded.ModelID,
		ResponseIDPresent: true, SupplierRequestIDPresent: decoded.SupplierRequestID != "", InputTokens: input, OutputTokens: output,
		CachedInputTokens: decoded.Usage.CachedInput, LatencyMS: durationMS(finished.Sub(started)), FinishReason: decoded.Choices[0].FinishReason,
		WireSHA256: audit.Digest(), WireBytes: audit.BytesRead()}, nil
}

func (q qualifier) streaming(ctx context.Context, cfg qualifierConfig, sequence int, runStarted time.Time) (sampleEvidence, error) {
	callCtx, cancel := context.WithTimeout(ctx, cfg.RequestTimeout)
	defer cancel()
	requestID := fmt.Sprintf("ic-qual-%d-s-%02d", runStarted.UnixMicro(), sequence)
	request, err := q.adapter.BuildRequest(callCtx, q.target, q.qualificationRequest(cfg, requestID, true), q.secret)
	if err != nil {
		return sampleEvidence{}, safeStageError("streaming request construction", err)
	}
	started := q.now()
	response, err := q.client.Do(request)
	if err != nil {
		if response != nil && response.Body != nil {
			response.Body.Close()
		}
		return sampleEvidence{}, errors.New("streaming supplier transport failed")
	}
	wire := newSSEWireAudit(response.Body, cfg.MaxStreamBytes)
	response.Body = wire
	stream, err := q.adapter.OpenStream(callCtx, response)
	if err != nil {
		return sampleEvidence{}, safeStageError("streaming response contract", err)
	}
	defer stream.Close()
	var firstContent time.Time
	finishReason := ""
	finishCount, usageCount := 0, 0
	var usage supplieradapter.Usage
	supplierRequestIDPresent := false
	for {
		event, nextErr := stream.Next(callCtx)
		if errors.Is(nextErr, io.EOF) {
			break
		}
		if nextErr != nil {
			return sampleEvidence{}, safeStageError("streaming SSE contract", nextErr)
		}
		supplierRequestIDPresent = supplierRequestIDPresent || event.SupplierRequestID != ""
		if event.TextDelta != "" && firstContent.IsZero() {
			firstContent = q.now()
		}
		if event.FinishReason != "" {
			finishCount++
			finishReason = event.FinishReason
		}
		if event.Usage != nil {
			usageCount++
			usage = *event.Usage
		}
	}
	finished := q.now()
	if err = wire.Validate(); err != nil {
		return sampleEvidence{}, errors.New("streaming wire contract was not terminal")
	}
	input, output, usageErr := requireCompleteUsage(usage)
	if usageErr != nil || firstContent.IsZero() || finishCount != 1 || usageCount != 1 || finishReason == "" {
		return sampleEvidence{}, errors.New("streaming response failed exact content, usage, or terminal contract")
	}
	ttft := durationMS(firstContent.Sub(started))
	generationSeconds := finished.Sub(firstContent).Seconds()
	if generationSeconds <= 0 {
		generationSeconds = 1e-9
	}
	throughput := float64(output) / generationSeconds
	return sampleEvidence{Mode: "streaming", Sequence: sequence, Succeeded: true, ModelID: q.profile.SupplierModelID,
		SupplierRequestIDPresent: supplierRequestIDPresent, InputTokens: input, OutputTokens: output, CachedInputTokens: usage.CachedInput,
		LatencyMS: durationMS(finished.Sub(started)), TTFTMS: &ttft, OutputTokensPerSecond: &throughput, FinishReason: finishReason,
		WireSHA256: wire.Digest(), WireBytes: wire.BytesRead(), TerminalDoneFrames: wire.DoneFrames()}, nil
}

func requireCompleteUsage(usage supplieradapter.Usage) (int64, int64, error) {
	if err := usage.Validate(); err != nil || usage.State != supplieradapter.UsageComplete || usage.InputTokens == nil || usage.OutputTokens == nil || *usage.InputTokens <= 0 || *usage.OutputTokens <= 0 {
		return 0, 0, errors.New("complete positive supplier usage is required")
	}
	return *usage.InputTokens, *usage.OutputTokens, nil
}

func summarize(samples []sampleEvidence) performanceSummary {
	var ttft, throughput []float64
	buffered, streaming := 0, 0
	for _, sample := range samples {
		if sample.Mode == "buffered" {
			buffered++
			continue
		}
		streaming++
		if sample.TTFTMS != nil {
			ttft = append(ttft, *sample.TTFTMS)
		}
		if sample.OutputTokensPerSecond != nil {
			throughput = append(throughput, *sample.OutputTokensPerSecond)
		}
	}
	return performanceSummary{BufferedSamples: buffered, StreamingSamples: streaming,
		TTFTP95MS: nearestRank(ttft, .95), OutputTokensP5: nearestRank(throughput, .05)}
}

func nearestRank(values []float64, percentile float64) float64 {
	if len(values) == 0 {
		return 0
	}
	copyValues := append([]float64(nil), values...)
	sort.Float64s(copyValues)
	index := int(math.Ceil(percentile*float64(len(copyValues)))) - 1
	if index < 0 {
		index = 0
	}
	if index >= len(copyValues) {
		index = len(copyValues) - 1
	}
	return copyValues[index]
}

func safeStageError(stage string, err error) error {
	var normalized *supplieradapter.Error
	if errors.As(err, &normalized) {
		return fmt.Errorf("%s failed: %s", stage, normalized.Error())
	}
	return fmt.Errorf("%s failed", stage)
}

func durationMS(value time.Duration) float64 {
	if value <= 0 {
		return .000001
	}
	return float64(value) / float64(time.Millisecond)
}

func digestBytes(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}

type hashingReadCloser struct {
	body io.ReadCloser
	hash hash.Hash
	read int64
}

func newHashingReadCloser(body io.ReadCloser) *hashingReadCloser {
	return &hashingReadCloser{body: body, hash: sha256.New()}
}

func (r *hashingReadCloser) Read(value []byte) (int, error) {
	n, err := r.body.Read(value)
	if n > 0 {
		_, _ = r.hash.Write(value[:n])
		r.read += int64(n)
	}
	return n, err
}

func (r *hashingReadCloser) Close() error     { return r.body.Close() }
func (r *hashingReadCloser) Digest() string   { return hex.EncodeToString(r.hash.Sum(nil)) }
func (r *hashingReadCloser) BytesRead() int64 { return r.read }

func writeArtifacts(cfg qualifierConfig, raw rawEvidence, manifest qualificationManifest) error {
	if err := writeJSONExclusive(cfg.EvidenceOutput, raw); err != nil {
		return fmt.Errorf("write evidence artifact: %w", err)
	}
	if err := writeJSONExclusive(cfg.QualificationOutput, manifest); err != nil {
		return fmt.Errorf("write qualification artifact: %w", err)
	}
	return nil
}

func writeJSONExclusive(path string, value any) error {
	encoded, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return errors.New("artifact could not be encoded")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return errors.New("artifact path is invalid")
	}
	if err = os.MkdirAll(filepath.Dir(absolute), 0o755); err != nil {
		return errors.New("artifact directory could not be created")
	}
	file, err := os.OpenFile(absolute, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return errors.New("artifact already exists or cannot be created")
	}
	defer file.Close()
	if _, err = file.Write(append(encoded, '\n')); err != nil {
		return errors.New("artifact could not be written")
	}
	return file.Sync()
}
