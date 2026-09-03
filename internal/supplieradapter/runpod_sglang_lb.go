package supplieradapter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	RunPodSGLangLBAdapterName    = "runpod-sglang-openai-lb"
	RunPodQwen38SupplierModelID  = "Qwen/Qwen3.8-27B-FP8"
	RunPodQwen38MaxOutputTokens  = 512
	RunPodQwen38QualifiedContext = 18_432
)

// RunPodSGLangLBAdapter is the direct, load-balanced RunPod contract used by
// the provisional Qwen3.8 H100 recipe. It is intentionally separate from the
// queue-based vLLM adapter: the URL shape, health model, streaming behavior,
// and deployment evidence are different contracts.
type RunPodSGLangLBAdapter struct {
	client *http.Client
	now    func() time.Time
}

var _ Adapter = (*RunPodSGLangLBAdapter)(nil)

func NewRunPodSGLangLBAdapter(client *http.Client) *RunPodSGLangLBAdapter {
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	strictClient := *client
	strictClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &RunPodSGLangLBAdapter{client: &strictClient, now: time.Now}
}

func (*RunPodSGLangLBAdapter) Name() string { return RunPodSGLangLBAdapterName }

func (a *RunPodSGLangLBAdapter) BuildRequest(ctx context.Context, target Target, request Request, credentials CredentialResolver) (*http.Request, error) {
	if err := validateRunPodSGLangLBTarget(target); err != nil {
		return nil, runPodInvalidRequest(err.Error())
	}
	if err := validateRunPodSGLangLBRequest(request); err != nil {
		return nil, runPodInvalidRequest(err.Error())
	}
	if credentials == nil {
		return nil, runPodInternalBeforeTransmission("supplier credential resolver is unavailable", nil)
	}
	credential, err := credentials.Resolve(ctx, target.CredentialReference)
	if err != nil {
		return nil, runPodInternalBeforeTransmission("supplier credential could not be resolved", err)
	}
	defer clear(credential)
	credentialValue := bytes.TrimSpace(credential)
	if len(credentialValue) == 0 || !runPodSafeHeaderValue(string(credentialValue)) {
		return nil, runPodInternalBeforeTransmission("supplier credential is invalid", nil)
	}

	type wireMessage struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	payload := struct {
		Model         string          `json:"model"`
		Messages      []wireMessage   `json:"messages"`
		MaxTokens     int             `json:"max_tokens"`
		Temperature   *float64        `json:"temperature,omitempty"`
		Stream        bool            `json:"stream"`
		StreamOptions map[string]bool `json:"stream_options,omitempty"`
	}{
		Model: target.SupplierModelID, Messages: make([]wireMessage, 0, len(request.Messages)),
		MaxTokens: *request.MaxOutputTokens, Temperature: request.Temperature, Stream: request.Stream,
	}
	if request.Stream {
		payload.StreamOptions = map[string]bool{"include_usage": true}
	}
	for _, message := range request.Messages {
		payload.Messages = append(payload.Messages, wireMessage{Role: message.Role, Content: message.Content[0].Text})
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, runPodInternalBeforeTransmission("supplier request could not be encoded", err)
	}
	if len(body) > RunPodVLLMMaxRequestBytes {
		return nil, runPodInvalidRequest("normalized request exceeds the RunPod SGLang MVP byte limit")
	}

	requestContext := context.WithValue(ctx, runPodExpectedModelKey{}, target.SupplierModelID)
	endpoint := strings.TrimRight(target.BaseURL, "/") + "/v1/chat/completions"
	upstream, err := http.NewRequestWithContext(requestContext, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, runPodInternalBeforeTransmission("supplier request could not be constructed", err)
	}
	upstream.Header.Set("Authorization", "Bearer "+string(credentialValue))
	upstream.Header.Set("Content-Type", "application/json")
	if request.Stream {
		upstream.Header.Set("Accept", "text/event-stream")
	} else {
		upstream.Header.Set("Accept", "application/json")
	}
	upstream.Header.Set("X-Request-ID", request.ID)
	upstream.Header.Set("User-Agent", "InferCrane/1 supplier-adapter")
	return upstream, nil
}

// Response normalization is the same pinned OpenAI subset already qualified
// for SGLang. Target validation remains separate so queue and load-balanced
// endpoint identities cannot be confused.
func (a *RunPodSGLangLBAdapter) DecodeResponse(ctx context.Context, response *http.Response) (Response, error) {
	return NewRunPodVLLMAdapter(a.client).DecodeResponse(ctx, response)
}

func (a *RunPodSGLangLBAdapter) OpenStream(ctx context.Context, response *http.Response) (Stream, error) {
	return NewRunPodVLLMAdapter(a.client).OpenStream(ctx, response)
}

func (a *RunPodSGLangLBAdapter) Probe(ctx context.Context, target Target, credentials CredentialResolver) (Observation, error) {
	if err := validateRunPodSGLangLBTarget(target); err != nil {
		return Observation{}, runPodInvalidRequest(err.Error())
	}
	if credentials == nil {
		return Observation{}, runPodInternalBeforeTransmission("supplier credential resolver is unavailable", nil)
	}
	credential, err := credentials.Resolve(ctx, target.CredentialReference)
	if err != nil {
		return Observation{}, runPodInternalBeforeTransmission("supplier credential could not be resolved", err)
	}
	defer clear(credential)
	credentialValue := bytes.TrimSpace(credential)
	if len(credentialValue) == 0 || !runPodSafeHeaderValue(string(credentialValue)) {
		return Observation{}, runPodInternalBeforeTransmission("supplier credential is invalid", nil)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(target.BaseURL, "/")+"/v1/models", nil)
	if err != nil {
		return Observation{}, runPodInternalBeforeTransmission("supplier probe could not be constructed", err)
	}
	request.Header.Set("Authorization", "Bearer "+string(credentialValue))
	request.Header.Set("Accept", "application/json")
	response, err := a.client.Do(request)
	if err != nil {
		return Observation{}, &Error{Code: ErrorTransport, Message: "supplier probe transport failed", Retry: RetrySameOffer, Billing: BillingAmbiguous, Cause: err}
	}
	if response == nil || response.Body == nil {
		return Observation{}, runPodProtocolFailure("supplier inventory response body is missing", "", nil)
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		defer response.Body.Close()
		return Observation{}, normalizeRunPodHTTPError(response.StatusCode, response.Header, runPodSupplierRequestID(response.Header))
	}
	defer response.Body.Close()
	body, err := runPodReadBounded(response.Body, 1<<20)
	if err != nil {
		return Observation{}, runPodProtocolFailure("supplier inventory response exceeded the safe byte limit", runPodSupplierRequestID(response.Header), err)
	}
	var inventory struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err = json.Unmarshal(body, &inventory); err != nil {
		return Observation{}, runPodProtocolFailure("supplier inventory response was malformed", runPodSupplierRequestID(response.Header), err)
	}
	items := make([]InventoryItem, 0, len(inventory.Data))
	found := 0
	for _, item := range inventory.Data {
		id := strings.TrimSpace(item.ID)
		if id == "" || !runPodSafeModelID(id) {
			continue
		}
		if id == target.SupplierModelID {
			found++
		}
		items = append(items, InventoryItem{SupplierModelID: id, Region: target.Region, Available: true})
	}
	availability := "unavailable"
	if found == 1 {
		availability = "available"
	}
	now := time.Now
	if a.now != nil {
		now = a.now
	}
	return Observation{Access: "authorized", Availability: availability, Health: "healthy", ObservedAt: now().UTC(), Inventory: items}, nil
}

func validateRunPodSGLangLBTarget(target Target) error {
	if target.Supplier != RunPodSupplier || target.SupplierModelID != RunPodQwen38SupplierModelID {
		return errors.New("target is not the qualified RunPod Qwen3.8 SGLang contract")
	}
	parsed, err := url.Parse(strings.TrimSpace(target.BaseURL))
	if err != nil || parsed.Scheme != "https" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.RawPath != "" || strings.Trim(parsed.Path, "/") != "" {
		return errors.New("target must use an exact RunPod load-balanced endpoint origin")
	}
	host := strings.TrimSuffix(parsed.Hostname(), ".")
	if parsed.Port() != "" || !strings.HasSuffix(host, ".api.runpod.ai") {
		return errors.New("target must use https://{endpoint_id}.api.runpod.ai")
	}
	endpointID := strings.TrimSuffix(host, ".api.runpod.ai")
	if !runPodEndpointIDPattern.MatchString(endpointID) || strings.Contains(endpointID, ".") {
		return errors.New("target RunPod endpoint id is invalid")
	}
	if strings.TrimSpace(target.Region) == "" || !runPodSafeHeaderValue(target.Region) {
		return errors.New("target region is required")
	}
	if strings.TrimSpace(target.CredentialReference) == "" {
		return errors.New("target credential reference is required")
	}
	return nil
}

func validateRunPodSGLangLBRequest(request Request) error {
	if err := validateRunPodVLLMRequest(request); err != nil {
		return err
	}
	if request.MaxOutputTokens == nil || *request.MaxOutputTokens > RunPodQwen38MaxOutputTokens {
		return fmt.Errorf("max output tokens must be set and cannot exceed %d for the provisional Qwen3.8 recipe", RunPodQwen38MaxOutputTokens)
	}
	return nil
}
