package controlapi

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/optimizationcampaign"
)

type capturePlaygroundInference struct {
	mu         sync.Mutex
	principals []domain.Principal
	bodies     []string
}

func (c *capturePlaygroundInference) ServeChatAs(w http.ResponseWriter, r *http.Request, principal domain.Principal) {
	body, _ := io.ReadAll(r.Body)
	c.mu.Lock()
	c.principals, c.bodies = append(c.principals, principal), append(c.bodies, string(body))
	c.mu.Unlock()
	w.Header().Set("X-Request-ID", "req_playground")
	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
}

func playgroundRequest(handler http.Handler, path, token, body string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func errorCode(t *testing.T, response *httptest.ResponseRecorder) string {
	t.Helper()
	var decoded struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("response is not an error envelope: %d %s", response.Code, response.Body.String())
	}
	return decoded.Error.Code
}

func TestPlaygroundChatAuthenticatesInTheControlPlaneAndServesAsThatPrincipal(t *testing.T) {
	const path = "/api/v1/playground/chat/completions"
	const body = `{"model":"support","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	store := &fakeStore{principal: domain.Principal{ID: "user_1", TenantID: "tenant-a", Role: "viewer", Kind: "human"}}
	inference := &capturePlaygroundInference{}
	handler := (API{Store: store, Authenticator: store, Inference: inference}).Handler()

	if response := playgroundRequest(handler, path, "", body); response.Code != http.StatusUnauthorized || errorCode(t, response) != "unauthenticated" {
		t.Fatalf("anonymous response=%d %s", response.Code, response.Body.String())
	}
	response := playgroundRequest(handler, path, "member-session", body)
	if response.Code != http.StatusOK || response.Header().Get("X-Request-ID") != "req_playground" || response.Header().Get("Cache-Control") != "no-store" || !strings.Contains(response.Body.String(), "[DONE]") {
		t.Fatalf("member response=%d headers=%v body=%s", response.Code, response.Header(), response.Body.String())
	}
	if len(inference.principals) != 1 || inference.principals[0].ID != "user_1" || inference.principals[0].TenantID != "tenant-a" || inference.bodies[0] != body {
		t.Fatalf("served principals=%+v bodies=%q", inference.principals, inference.bodies)
	}

	// The control API's own rules still apply before anything is served.
	store.principal = domain.Principal{ID: "token_1", TenantID: "tenant-a", Role: "viewer", Kind: "inference_token", EndpointNames: []string{"support"}}
	if response = playgroundRequest(handler, path, "endpoint-token", body); response.Code != http.StatusForbidden {
		t.Fatalf("endpoint-scoped credential response=%d %s", response.Code, response.Body.String())
	}
	store.principal = domain.Principal{ID: "key_1", TenantID: "tenant-a", Role: "operator", Scopes: []string{"deploy"}}
	if response = playgroundRequest(handler, path, "deploy-only", body); response.Code != http.StatusForbidden {
		t.Fatalf("principal without read response=%d %s", response.Code, response.Body.String())
	}
	if len(inference.principals) != 1 {
		t.Fatalf("a refused principal was served: %+v", inference.principals)
	}

	store.principal = domain.Principal{ID: "user_1", TenantID: "tenant-a", Role: "viewer", Kind: "human"}
	unconfigured := (API{Store: store, Authenticator: store}).Handler()
	if response = playgroundRequest(unconfigured, path, "member-session", body); response.Code != http.StatusNotImplemented || errorCode(t, response) != "capability_unavailable" {
		t.Fatalf("unconfigured response=%d %s", response.Code, response.Body.String())
	}
}

type candidateChatFixture struct {
	store   *fakeReportStore
	handler http.Handler
	prompts *CandidatePrompts
}

func newCandidateChatFixture(t *testing.T, state string, upstream http.HandlerFunc) candidateChatFixture {
	t.Helper()
	server := httptest.NewServer(upstream)
	t.Cleanup(server.Close)
	candidate := domain.OptimizationCandidateRun{ID: "candidate-1", TenantID: "tenant-a", CampaignID: "campaign-1", State: state, DeploymentName: "support-opt-1", RevisionID: "rev-2", DeploymentSpecJSON: `{"name":"support","model":{"id":"acme/model"},"runtime":{"engine":"vllm"}}`}
	base := &fakeStore{
		principal: domain.Principal{ID: "user_1", TenantID: "tenant-a", Role: "viewer", Kind: "human"},
		resolved:  domain.ResolvedDeployment{Deployment: domain.Deployment{ID: "dep-1", Name: "support-opt-1"}},
		replicas:  []domain.Replica{{TenantID: "tenant-a", DeploymentID: "dep-1", RevisionID: "rev-2", Endpoint: server.URL, Provider: "runpod-pods", Health: "healthy", LifecycleState: "active"}},
	}
	store := &fakeReportStore{fakeStore: base, campaign: domain.OptimizationCampaign{ID: "campaign-1", TenantID: "tenant-a", Intent: optimizationcampaign.IntentNewEndpoint, Candidates: []domain.OptimizationCandidateRun{candidate}}}
	prompts := NewCandidatePrompts()
	api := API{Store: store, Authenticator: store, CandidatePrompts: prompts, Backends: map[string]BackendMetadata{"runpod-pods": {APIKey: "worker-key"}}}
	return candidateChatFixture{store: store, handler: api.Handler(), prompts: prompts}
}

const candidateChatPath = "/api/v1/optimization/campaigns/campaign-1/candidates/candidate-1/chat/completions"

func TestCandidateChatReachesTheDecidedRevisionWithTheServerSideCredential(t *testing.T) {
	var authorization, received string
	fixture := newCandidateChatFixture(t, optimizationcampaign.CandidateQualified, func(w http.ResponseWriter, r *http.Request) {
		authorization = r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		received = string(body)
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("upstream path=%s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n")
	})
	response := playgroundRequest(fixture.handler, candidateChatPath, "member-session", `{"model":"anything","stream":true,"messages":[{"role":"user","content":"hello"}],"tools":[{"type":"function","function":{"name":"weather"}}]}`)
	if response.Code != http.StatusOK || response.Header().Get("Content-Type") != "text/event-stream" || !strings.HasSuffix(response.Body.String(), "data: [DONE]\n\n") {
		t.Fatalf("response=%d headers=%v body=%s", response.Code, response.Header(), response.Body.String())
	}
	if response.Header().Get("X-InferCrane-Deployment") != "support-opt-1" || response.Header().Get("X-InferCrane-Revision") != "rev-2" {
		t.Fatalf("identity headers=%v", response.Header())
	}
	if authorization != "Bearer worker-key" {
		t.Fatalf("upstream authorization=%q", authorization)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(received), &payload); err != nil || payload["model"] != "acme/model" || payload["tools"] == nil || payload["stream"] != true {
		t.Fatalf("upstream payload=%s", received)
	}
	if strings.Contains(response.Body.String()+strings.Join(response.Header().Values("Authorization"), ""), "worker-key") {
		t.Fatal("the server-side credential reached the caller")
	}
}

func TestCandidateChatIsTenantScopedAndRefusesUndecidedOrStoppedCandidates(t *testing.T) {
	calls := 0
	upstream := func(w http.ResponseWriter, _ *http.Request) {
		calls++
		_, _ = io.WriteString(w, `{}`)
	}
	const body = `{"messages":[{"role":"user","content":"hello"}]}`

	decided := newCandidateChatFixture(t, optimizationcampaign.CandidateQualified, upstream)
	decided.store.principal = domain.Principal{ID: "user_2", TenantID: "tenant-b", Role: "admin", Kind: "human"}
	if response := playgroundRequest(decided.handler, candidateChatPath, "other-tenant", body); response.Code != http.StatusNotFound {
		t.Fatalf("another tenant's run response=%d %s", response.Code, response.Body.String())
	}
	decided.store.principal = domain.Principal{ID: "user_1", TenantID: "tenant-a", Role: "viewer", Kind: "human"}
	if response := playgroundRequest(decided.handler, strings.Replace(candidateChatPath, "candidate-1", "candidate-9", 1), "member-session", body); response.Code != http.StatusNotFound {
		t.Fatalf("unknown candidate response=%d %s", response.Code, response.Body.String())
	}
	if response := playgroundRequest(decided.handler, candidateChatPath, "", body); response.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous response=%d", response.Code)
	}
	if response := playgroundRequest(decided.handler, candidateChatPath, "member-session", `{"messages":[]}`); response.Code != http.StatusBadRequest {
		t.Fatalf("empty messages response=%d %s", response.Code, response.Body.String())
	}
	if response := playgroundRequest(decided.handler, candidateChatPath, "member-session", `{"messages":[{"role":"user","content":"`+strings.Repeat("x", candidatePromptBodyLimit)+`"}]}`); response.Code != http.StatusRequestEntityTooLarge || errorCode(t, response) != "request_too_large" {
		t.Fatalf("oversized response=%d", response.Code)
	}

	for state, want := range map[string]string{
		optimizationcampaign.CandidateMeasuring:  "candidate_not_decided",
		optimizationcampaign.CandidateValidating: "candidate_not_decided",
		optimizationcampaign.CandidateRanked:     "candidate_not_decided",
		optimizationcampaign.CandidateRejected:   "candidate_not_running",
		optimizationcampaign.CandidateCleaned:    "candidate_not_running",
	} {
		fixture := newCandidateChatFixture(t, state, upstream)
		response := playgroundRequest(fixture.handler, candidateChatPath, "member-session", body)
		if response.Code != http.StatusConflict || errorCode(t, response) != want {
			t.Errorf("%s: response=%d %s", state, response.Code, response.Body.String())
		}
	}

	unreachable := newCandidateChatFixture(t, optimizationcampaign.CandidateQualified, upstream)
	unreachable.store.replicas[0].Health = "unhealthy"
	response := playgroundRequest(unreachable.handler, candidateChatPath, "member-session", body)
	if response.Code != http.StatusServiceUnavailable || errorCode(t, response) != "candidate_unavailable" || response.Header().Get("Retry-After") != "5" {
		t.Fatalf("unreachable response=%d headers=%v body=%s", response.Code, response.Header(), response.Body.String())
	}
	if calls != 0 {
		t.Fatalf("a refused prompt reached a runtime %d times", calls)
	}
}

func TestCandidateChatKeepsOnlyTheStatusOfARuntimeError(t *testing.T) {
	for upstreamStatus, want := range map[int]struct {
		status int
		code   string
		retry  string
	}{
		http.StatusBadRequest:          {http.StatusUnprocessableEntity, "candidate_rejected_request", ""},
		http.StatusTooManyRequests:     {http.StatusTooManyRequests, "rate_limited", "7"},
		http.StatusInternalServerError: {http.StatusBadGateway, "candidate_upstream_error", ""},
	} {
		fixture := newCandidateChatFixture(t, optimizationcampaign.CandidateGuardPassed, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Retry-After", "7")
			w.WriteHeader(upstreamStatus)
			_, _ = io.WriteString(w, `{"error":"internal host gpu-7.cluster.local refused"}`)
		})
		response := playgroundRequest(fixture.handler, candidateChatPath, "member-session", `{"messages":[{"role":"user","content":"hello"}]}`)
		if response.Code != want.status || errorCode(t, response) != want.code || response.Header().Get("Retry-After") != want.retry {
			t.Errorf("upstream %d: response=%d headers=%v body=%s", upstreamStatus, response.Code, response.Header(), response.Body.String())
		}
		if strings.Contains(response.Body.String(), "cluster.local") {
			t.Errorf("upstream %d: runtime error text reached the caller: %s", upstreamStatus, response.Body.String())
		}
	}
}

func TestCandidateChatBoundsConcurrentPromptsPerOrganization(t *testing.T) {
	entered, release := make(chan struct{}, candidatePromptsPerTenant), make(chan struct{})
	fixture := newCandidateChatFixture(t, optimizationcampaign.CandidateQualified, func(w http.ResponseWriter, _ *http.Request) {
		entered <- struct{}{}
		<-release
		_, _ = io.WriteString(w, `{"choices":[]}`)
	})
	const body = `{"messages":[{"role":"user","content":"hello"}]}`
	var running sync.WaitGroup
	for range candidatePromptsPerTenant {
		running.Add(1)
		go func() {
			defer running.Done()
			if response := playgroundRequest(fixture.handler, candidateChatPath, "member-session", body); response.Code != http.StatusOK {
				t.Errorf("held prompt response=%d %s", response.Code, response.Body.String())
			}
		}()
	}
	for range candidatePromptsPerTenant {
		<-entered
	}
	limited := playgroundRequest(fixture.handler, candidateChatPath, "member-session", body)
	if limited.Code != http.StatusTooManyRequests || errorCode(t, limited) != "rate_limited" || limited.Header().Get("Retry-After") != "1" {
		t.Fatalf("limited response=%d headers=%v body=%s", limited.Code, limited.Header(), limited.Body.String())
	}
	// Another organization is not held back by this one's prompts.
	if _, ok := fixture.prompts.acquire("tenant-b"); !ok {
		t.Fatal("one organization's prompts blocked another's")
	}
	close(release)
	running.Wait()
	if response := playgroundRequest(fixture.handler, candidateChatPath, "member-session", body); response.Code != http.StatusOK {
		t.Fatalf("after release response=%d %s", response.Code, response.Body.String())
	}
}
