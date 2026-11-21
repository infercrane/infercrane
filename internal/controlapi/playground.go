package controlapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/openaicompat"
	"github.com/infercrane/infercrane/internal/optimizationcampaign"
)

// PlaygroundInference serves one chat completion for a principal the control
// API has already authenticated. The gateway implements it with its own
// inference path, so a prompt sent from the dashboard is routed, admitted,
// metered and recorded exactly like one sent with an API key.
type PlaygroundInference interface {
	ServeChatAs(http.ResponseWriter, *http.Request, domain.Principal)
}

const (
	candidatePromptBodyLimit     = 1 << 20
	candidatePromptResponseLimit = 8 << 20
	candidatePromptTimeout       = 5 * time.Minute
	candidatePromptsPerTenant    = 2
)

// CandidatePrompts sends single prompts straight to a campaign's decided
// candidate and bounds how many run at once for one organization.
type CandidatePrompts struct {
	// Client overrides the HTTP client; nil uses one that never follows
	// redirects or a proxy.
	Client *http.Client

	mu     sync.Mutex
	active map[string]int
}

func NewCandidatePrompts() *CandidatePrompts {
	return &CandidatePrompts{active: map[string]int{}}
}

func (p *CandidatePrompts) acquire(tenant string) (func(), bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.active == nil {
		p.active = map[string]int{}
	}
	if p.active[tenant] >= candidatePromptsPerTenant {
		return nil, false
	}
	p.active[tenant]++
	return func() {
		p.mu.Lock()
		defer p.mu.Unlock()
		if p.active[tenant]--; p.active[tenant] <= 0 {
			delete(p.active, tenant)
		}
	}, true
}

func (p *CandidatePrompts) client() *http.Client {
	if p.Client != nil {
		return p.Client
	}
	return candidatePromptClient
}

var candidatePromptClient = &http.Client{
	Transport: &http.Transport{
		Proxy:                 nil,
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 3 * time.Minute,
		MaxIdleConnsPerHost:   2,
	},
	CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("redirects are not followed") },
}

type candidatePromptStore interface {
	OptimizationCampaign(context.Context, string, string) (domain.OptimizationCampaign, error)
	optimizationcampaign.CandidateChatStore
}

// playgroundChat lets a control-plane principal, such as a signed-in dashboard
// member, send one chat completion to an endpoint it may call. Authentication
// happens here, in the control plane; the gateway then serves the request on
// its normal path.
func (a API) playgroundChat(w http.ResponseWriter, r *http.Request) {
	if a.Inference == nil {
		writeError(w, http.StatusNotImplemented, "capability_unavailable", "playground inference is not configured")
		return
	}
	a.Inference.ServeChatAs(w, r, r.Context().Value(identityKey{}).(domain.Principal))
}

// optimizationCandidateChat sends one chat completion to the exact revision a
// campaign decided on, addressed like the campaign's own measurement and
// answer check. It bypasses the gateway on purpose: the candidate has no
// published route before it is rolled out. Nothing is recorded as a request
// trace, and the server-side credential never reaches the caller.
func (a API) optimizationCandidateChat(w http.ResponseWriter, r *http.Request) {
	store, ok := a.Store.(candidatePromptStore)
	if !ok || a.CandidatePrompts == nil {
		writeError(w, http.StatusNotImplemented, "capability_unavailable", "setup prompts are not configured")
		return
	}
	actor := r.Context().Value(identityKey{}).(domain.Principal)
	campaign, err := store.OptimizationCampaign(r.Context(), actor.TenantID, r.PathValue("id"))
	if errors.Is(err, domain.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "optimization run was not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "optimization run could not be loaded")
		return
	}
	var candidate domain.OptimizationCandidateRun
	for _, row := range campaign.Candidates {
		if row.ID == r.PathValue("candidate_id") {
			candidate = row
			break
		}
	}
	if candidate.ID == "" {
		writeError(w, http.StatusNotFound, "not_found", "setup was not found")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, candidatePromptBodyLimit))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, "request_too_large", "setup prompts are limited to 1 MiB")
			return
		}
		writeError(w, http.StatusBadRequest, "invalid_request", "request body could not be read")
		return
	}
	var payload map[string]any
	if json.Unmarshal(body, &payload) != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "request body must be one JSON object")
		return
	}
	if messages, _ := payload["messages"].([]any); len(messages) == 0 {
		writeError(w, http.StatusBadRequest, "invalid_request", "messages must contain at least one message")
		return
	}
	backends := make(map[string]optimizationcampaign.BenchmarkBackend, len(a.Backends))
	for name, backend := range a.Backends {
		backends[name] = optimizationcampaign.BenchmarkBackend{APIKey: backend.APIKey, APIKeyEnv: backend.APIKeyEnv, WorkerMaster: backend.WorkerMaster}
	}
	target, err := optimizationcampaign.CandidateChatTarget(r.Context(), store, backends, candidate)
	switch {
	case errors.Is(err, optimizationcampaign.ErrCandidateUndecided):
		writeError(w, http.StatusConflict, "candidate_not_decided", "a setup can be prompted once the run has decided, so a prompt never runs beside a measurement")
		return
	case errors.Is(err, optimizationcampaign.ErrCandidateStopped):
		writeError(w, http.StatusConflict, "candidate_not_running", "this setup is no longer running")
		return
	case errors.Is(err, optimizationcampaign.ErrCandidateUnreachable):
		w.Header().Set("Retry-After", "5")
		writeError(w, http.StatusServiceUnavailable, "candidate_unavailable", "the setup has no healthy replica to answer right now")
		return
	case err != nil:
		writeError(w, http.StatusInternalServerError, "internal", "setup could not be resolved")
		return
	}
	url, err := openaicompat.Endpoint(target.Endpoint, "chat/completions")
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "candidate_unavailable", "the setup's address is not usable")
		return
	}
	release, ok := a.CandidatePrompts.acquire(actor.TenantID)
	if !ok {
		w.Header().Set("Retry-After", "1")
		writeError(w, http.StatusTooManyRequests, "rate_limited", fmt.Sprintf("at most %d candidate prompts run at once", candidatePromptsPerTenant))
		return
	}
	defer release()

	// The candidate serves its own model identity; the caller's value is ignored.
	payload["model"] = target.Model
	encoded, err := json.Marshal(payload)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "request body could not be encoded")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), candidatePromptTimeout)
	defer cancel()
	upstream, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(encoded))
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "candidate_unavailable", "the setup's address is not usable")
		return
	}
	upstream.Header.Set("Content-Type", "application/json")
	upstream.Header.Set("Accept-Encoding", "identity")
	if accept := r.Header.Get("Accept"); accept != "" {
		upstream.Header.Set("Accept", accept)
	}
	upstream.Header.Set("Authorization", "Bearer "+target.APIKey)
	response, err := a.CandidatePrompts.client().Do(upstream)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			writeError(w, http.StatusGatewayTimeout, "candidate_timeout", "the setup did not answer in time")
			return
		}
		writeError(w, http.StatusBadGateway, "candidate_upstream_error", "the setup could not be reached")
		return
	}
	defer response.Body.Close()
	w.Header().Set("X-InferCrane-Deployment", target.DeploymentName)
	w.Header().Set("X-InferCrane-Revision", target.RevisionID)
	if response.StatusCode != http.StatusOK {
		// The runtime's own error text can name internals; only its status is kept.
		status, code := http.StatusBadGateway, "candidate_upstream_error"
		switch {
		case response.StatusCode == http.StatusTooManyRequests:
			status, code = http.StatusTooManyRequests, "rate_limited"
			if seconds, parseErr := strconv.Atoi(response.Header.Get("Retry-After")); parseErr == nil && seconds > 0 && seconds <= 300 {
				w.Header().Set("Retry-After", strconv.Itoa(seconds))
			} else {
				w.Header().Set("Retry-After", "1")
			}
		case response.StatusCode >= 400 && response.StatusCode < 500:
			status, code = http.StatusUnprocessableEntity, "candidate_rejected_request"
		}
		writeError(w, status, code, fmt.Sprintf("the candidate's server answered HTTP %d", response.StatusCode))
		return
	}
	contentType := response.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/json"
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	buffer := make([]byte, 16<<10)
	remaining := candidatePromptResponseLimit
	for remaining > 0 {
		limit := len(buffer)
		if remaining < limit {
			limit = remaining
		}
		read, readErr := response.Body.Read(buffer[:limit])
		if read > 0 {
			remaining -= read
			if _, writeErr := w.Write(buffer[:read]); writeErr != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if readErr != nil {
			return
		}
	}
}
