package gateway

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/admission"
	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/routes"
)

type allRecords struct {
	mu      sync.Mutex
	records []domain.InferenceRecord
}

func (a *allRecords) RecordRequest(_ context.Context, record domain.InferenceRecord) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.records = append(a.records, record)
	return nil
}

func (a *allRecords) only(t *testing.T) domain.InferenceRecord {
	t.Helper()
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.records) != 1 {
		t.Fatalf("records = %#v", a.records)
	}
	return a.records[0]
}

// upstreams answers each route by host and counts what each host received.
type upstreams struct {
	mu       sync.Mutex
	answer   map[string]func(*http.Request) (*http.Response, error)
	sent     map[string]int
	attempts map[string][]string
	models   map[string][]string
	keys     map[string][]string
}

func newUpstreams(answer map[string]func(*http.Request) (*http.Response, error)) *upstreams {
	return &upstreams{answer: answer, sent: map[string]int{}, attempts: map[string][]string{}, models: map[string][]string{}, keys: map[string][]string{}}
}

func (u *upstreams) client() *http.Client {
	return &http.Client{Transport: roundTripper(func(r *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(r.Body)
		u.mu.Lock()
		u.sent[r.URL.Host]++
		u.attempts[r.URL.Host] = append(u.attempts[r.URL.Host], r.Header.Get("X-InferCrane-Attempt"))
		u.keys[r.URL.Host] = append(u.keys[r.URL.Host], r.Header.Get("Authorization"))
		if strings.Contains(string(body), `"model":"`) {
			model := strings.SplitN(strings.SplitN(string(body), `"model":"`, 2)[1], `"`, 2)[0]
			u.models[r.URL.Host] = append(u.models[r.URL.Host], model)
		}
		answer := u.answer[r.URL.Host]
		u.mu.Unlock()
		if answer == nil {
			return nil, errors.New("dial tcp: connection refused")
		}
		return answer(r)
	})}
}

func (u *upstreams) count(host string) int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.sent[host]
}

func refused(*http.Request) (*http.Response, error) {
	return nil, errors.New("dial tcp 10.0.0.7:8000: connect: connection refused")
}

func answerStatus(status int) func(*http.Request) (*http.Response, error) {
	return func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"error":{"message":"from upstream"}}`))}, nil
	}
}

func answerOK(body string) func(*http.Request) (*http.Response, error) {
	return func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
	}
}

func endpointRoute(binding, host, model string) routes.Snapshot {
	return routes.Snapshot{TenantID: "global", Alias: "coder", EndpointID: "endpoint", ServingPlanID: "plan", BindingID: binding, DeploymentID: binding + "-deployment", RevisionID: binding + "-revision", RouterURL: "http://" + host, RouterProcessID: binding + "-g1", UpstreamModel: model}
}

func publishPlan(directory *routes.Directory, policy string, bound ...routes.Snapshot) {
	directory.PublishEndpoint(routes.EndpointRoute{TenantID: "global", Alias: "coder", RoutingPolicy: policy, Routes: bound})
}

func invokeCoder(handler http.Handler, body string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer secret")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

type fixedAdmission struct {
	retryBudget int
	timeout     time.Duration
}

func (fixedAdmission) Acquire(context.Context, admission.Request) (func(), error) {
	return func() {}, nil
}
func (a fixedAdmission) RetryBudget(string) int              { return a.retryBudget }
func (a fixedAdmission) RequestTimeout(string) time.Duration { return a.timeout }

func TestPrimaryFallbackFailsOverBufferedRequestBeforeAnyResponseByte(t *testing.T) {
	for _, tc := range []struct {
		name    string
		primary func(*http.Request) (*http.Response, error)
		reason  string
	}{
		{name: "connection refused", primary: refused, reason: "upstream_connection_failed"},
		{name: "bad gateway", primary: answerStatus(http.StatusBadGateway), reason: "upstream_502"},
		{name: "unavailable", primary: answerStatus(http.StatusServiceUnavailable), reason: "upstream_503"},
		{name: "gateway timeout", primary: answerStatus(http.StatusGatewayTimeout), reason: "upstream_504"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := newUpstreams(map[string]func(*http.Request) (*http.Response, error){
				"owned":  tc.primary,
				"closed": answerOK(`{"model":"closed-model","choices":[{"message":{"content":"served by fallback"}}],"usage":{"prompt_tokens":3,"completion_tokens":2}}`),
			})
			directory := routes.New()
			primary := endpointRoute("owned", "owned", "qwen3")
			fallback := endpointRoute("closed", "closed", "closed-model")
			fallback.DeploymentID, fallback.RevisionID, fallback.ComputeMode, fallback.UpstreamAPIKey = "", "", "external", "sealed-closed-key"
			publishPlan(directory, "primary-fallback", primary, fallback)
			records := &allRecords{}
			handler := (&Gateway{Routes: directory, APIKey: "secret", Client: upstream.client(), Recorder: records}).Handler()
			response := invokeCoder(handler, `{"model":"coder","messages":[{"role":"user","content":"hi"}]}`)
			if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "served by fallback") {
				t.Fatalf("response=%d %s", response.Code, response.Body.String())
			}
			if upstream.count("owned") != 1 || upstream.count("closed") != 1 {
				t.Fatalf("sent=%#v", upstream.sent)
			}
			if got := upstream.models["closed"]; len(got) != 1 || got[0] != "closed-model" {
				t.Fatalf("fallback received model %#v, want its own upstream model", got)
			}
			if got := upstream.keys["closed"]; len(got) != 1 || got[0] != "Bearer sealed-closed-key" {
				t.Fatalf("fallback credential=%#v", got)
			}
			if got := upstream.attempts["closed"]; len(got) != 1 || got[0] != "2" {
				t.Fatalf("fallback attempt header=%#v", got)
			}
			record := records.only(t)
			if record.BindingID != "closed" || record.DeploymentID != "" || record.StatusCode != http.StatusOK || record.FallbackReason != tc.reason || record.RetryCount != 0 || record.ErrorType != "" {
				t.Fatalf("record=%#v", record)
			}
			if record.InputTokens == nil || *record.InputTokens != 3 || record.ResponseModel != "closed-model" {
				t.Fatalf("record usage=%#v", record)
			}
		})
	}
}

func TestFailoverSpendsRetryBudgetOnlyOnTheFirstRoute(t *testing.T) {
	upstream := newUpstreams(map[string]func(*http.Request) (*http.Response, error){
		"owned":  answerStatus(http.StatusServiceUnavailable),
		"closed": answerStatus(http.StatusServiceUnavailable),
	})
	directory := routes.New()
	publishPlan(directory, "primary-fallback", endpointRoute("owned", "owned", "qwen3"), endpointRoute("closed", "closed", "closed-model"))
	records := &allRecords{}
	handler := (&Gateway{Routes: directory, APIKey: "secret", Client: upstream.client(), Recorder: records, AdmissionAuthorizer: fixedAdmission{retryBudget: 2}}).Handler()
	response := invokeCoder(handler, `{"model":"coder","messages":[]}`)
	if response.Code != http.StatusServiceUnavailable || strings.Contains(response.Body.String(), "from upstream") {
		t.Fatalf("response=%d %s", response.Code, response.Body.String())
	}
	if upstream.count("owned") != 3 || upstream.count("closed") != 1 {
		t.Fatalf("the fallback was retried or the first route was retried past its budget: %#v", upstream.sent)
	}
	if got := strings.Join(upstream.attempts["owned"], ",") + "|" + strings.Join(upstream.attempts["closed"], ","); got != "1,2,3|4" {
		t.Fatalf("attempt headers=%s", got)
	}
	record := records.only(t)
	if record.BindingID != "closed" || record.StatusCode != http.StatusServiceUnavailable || record.ErrorType != "upstream_unavailable" || record.FallbackReason != "upstream_503" || record.RetryCount != 2 {
		t.Fatalf("record=%#v", record)
	}
}

func TestFailoverIsBoundedToTwoFurtherRoutes(t *testing.T) {
	upstream := newUpstreams(map[string]func(*http.Request) (*http.Response, error){
		"one": refused, "two": refused, "three": refused, "four": answerOK(`{"choices":[]}`),
	})
	directory := routes.New()
	publishPlan(directory, "primary-fallback", endpointRoute("one", "one", "m"), endpointRoute("two", "two", "m"), endpointRoute("three", "three", "m"), endpointRoute("four", "four", "m"))
	records := &allRecords{}
	handler := (&Gateway{Routes: directory, APIKey: "secret", Client: upstream.client(), Recorder: records}).Handler()
	response := invokeCoder(handler, `{"model":"coder","messages":[]}`)
	if response.Code != http.StatusServiceUnavailable || upstream.count("four") != 0 {
		t.Fatalf("response=%d sent=%#v", response.Code, upstream.sent)
	}
	if upstream.count("one") != 1 || upstream.count("two") != 1 || upstream.count("three") != 1 {
		t.Fatalf("sent=%#v", upstream.sent)
	}
	if record := records.only(t); record.BindingID != "three" || record.FallbackReason != "upstream_connection_failed" {
		t.Fatalf("record=%#v", record)
	}
}

func TestFailoverLeavesStreamsOtherFailuresAndOtherPoliciesAlone(t *testing.T) {
	for _, tc := range []struct {
		name, policy, body string
		primary            func(*http.Request) (*http.Response, error)
		status             int
	}{
		{name: "stream", policy: "primary-fallback", body: `{"model":"coder","stream":true,"messages":[]}`, primary: refused, status: http.StatusServiceUnavailable},
		{name: "server error", policy: "primary-fallback", body: `{"model":"coder","messages":[]}`, primary: answerStatus(http.StatusInternalServerError), status: http.StatusInternalServerError},
		{name: "rate limited", policy: "primary-fallback", body: `{"model":"coder","messages":[]}`, primary: answerStatus(http.StatusTooManyRequests), status: http.StatusTooManyRequests},
		{name: "weighted", policy: "weighted", body: `{"model":"coder","messages":[]}`, primary: refused, status: http.StatusServiceUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := newUpstreams(map[string]func(*http.Request) (*http.Response, error){"owned": tc.primary, "closed": answerOK(`{"choices":[]}`)})
			directory := routes.New()
			primary, fallback := endpointRoute("owned", "owned", "qwen3"), endpointRoute("closed", "closed", "closed-model")
			primary.RoutingWeight, fallback.RoutingWeight = 1, 1
			publishPlan(directory, tc.policy, primary, fallback)
			records := &allRecords{}
			handler := (&Gateway{Routes: directory, APIKey: "secret", Client: upstream.client(), Recorder: records}).Handler()
			response := invokeCoder(handler, tc.body)
			if response.Code != tc.status || upstream.count("closed") != 0 {
				t.Fatalf("response=%d sent=%#v body=%s", response.Code, upstream.sent, response.Body.String())
			}
			if record := records.only(t); record.BindingID != "owned" || record.FallbackReason != "" {
				t.Fatalf("record=%#v", record)
			}
		})
	}
}

func TestFailoverStopsWhenTheCallerLeavesOrTheDeadlinePasses(t *testing.T) {
	t.Run("cancelled", func(t *testing.T) {
		started := make(chan struct{})
		upstream := newUpstreams(map[string]func(*http.Request) (*http.Response, error){
			"owned": func(r *http.Request) (*http.Response, error) {
				close(started)
				<-r.Context().Done()
				return nil, r.Context().Err()
			},
			"closed": answerOK(`{"choices":[]}`),
		})
		directory := routes.New()
		publishPlan(directory, "primary-fallback", endpointRoute("owned", "owned", "qwen3"), endpointRoute("closed", "closed", "closed-model"))
		handler := (&Gateway{Routes: directory, APIKey: "secret", Client: upstream.client()}).Handler()
		ctx, cancel := context.WithCancel(context.Background())
		request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"coder","messages":[]}`)).WithContext(ctx)
		request.Header.Set("Authorization", "Bearer secret")
		done := make(chan struct{})
		go func() {
			handler.ServeHTTP(httptest.NewRecorder(), request)
			close(done)
		}()
		<-started
		cancel()
		<-done
		if upstream.count("closed") != 0 {
			t.Fatalf("a cancelled request failed over: %#v", upstream.sent)
		}
	})
	t.Run("deadline", func(t *testing.T) {
		upstream := newUpstreams(map[string]func(*http.Request) (*http.Response, error){
			"owned": func(r *http.Request) (*http.Response, error) {
				<-r.Context().Done()
				return nil, r.Context().Err()
			},
			"closed": answerOK(`{"choices":[]}`),
		})
		directory := routes.New()
		publishPlan(directory, "primary-fallback", endpointRoute("owned", "owned", "qwen3"), endpointRoute("closed", "closed", "closed-model"))
		records := &allRecords{}
		handler := (&Gateway{Routes: directory, APIKey: "secret", Client: upstream.client(), Recorder: records, AdmissionAuthorizer: fixedAdmission{timeout: 50 * time.Millisecond}}).Handler()
		response := invokeCoder(handler, `{"model":"coder","messages":[]}`)
		if response.Code != http.StatusGatewayTimeout || upstream.count("closed") != 0 {
			t.Fatalf("response=%d sent=%#v", response.Code, upstream.sent)
		}
		if record := records.only(t); record.ErrorType != "request_deadline_exceeded" || record.FallbackReason != "" {
			t.Fatalf("record=%#v", record)
		}
	})
}

func TestManagedExternalFallbackReservesBeforeTransmission(t *testing.T) {
	managed := func() routes.Snapshot {
		route := endpointRoute("managed", "supplier", "supplier/model")
		route.DeploymentID, route.RevisionID, route.ComputeMode, route.Provider, route.ExternalPolicyID, route.UpstreamAPIKey = "", "", "external", "modal", "policy", "supplier-key"
		return route
	}
	t.Run("reserved and settled", func(t *testing.T) {
		upstream := newUpstreams(map[string]func(*http.Request) (*http.Response, error){"owned": refused, "supplier": answerOK(`{"usage":{"prompt_tokens":5,"completion_tokens":4}}`)})
		directory := routes.New()
		publishPlan(directory, "primary-fallback", endpointRoute("owned", "owned", "qwen3"), managed())
		billing := &captureManagedBilling{}
		handler := (&Gateway{Routes: directory, APIKey: "secret", Client: upstream.client(), ManagedBilling: billing, ExternalAuthorizer: &oneRequestBudget{remaining: 1}}).Handler()
		response := invokeCoder(handler, `{"model":"coder","messages":[]}`)
		if response.Code != http.StatusOK || upstream.count("supplier") != 1 || billing.authorized != 1 || billing.settled != 1 || billing.released != 0 {
			t.Fatalf("response=%d sent=%#v billing=%+v", response.Code, upstream.sent, billing)
		}
		if billing.settlement.OutputTokens == nil || *billing.settlement.OutputTokens != 4 {
			t.Fatalf("settlement=%+v", billing.settlement)
		}
	})
	t.Run("never left once sent", func(t *testing.T) {
		upstream := newUpstreams(map[string]func(*http.Request) (*http.Response, error){"supplier": answerStatus(http.StatusServiceUnavailable), "owned": answerOK(`{"choices":[]}`)})
		directory := routes.New()
		publishPlan(directory, "primary-fallback", managed(), endpointRoute("owned", "owned", "qwen3"))
		billing := &captureManagedBilling{}
		records := &allRecords{}
		handler := (&Gateway{Routes: directory, APIKey: "secret", Client: upstream.client(), Recorder: records, ManagedBilling: billing, ExternalAuthorizer: &oneRequestBudget{remaining: 2}}).Handler()
		response := invokeCoder(handler, `{"model":"coder","messages":[]}`)
		if response.Code != http.StatusServiceUnavailable || upstream.count("supplier") != 1 || upstream.count("owned") != 0 || billing.released != 1 {
			t.Fatalf("response=%d sent=%#v billing=%+v", response.Code, upstream.sent, billing)
		}
		if record := records.only(t); record.BindingID != "managed" || record.FallbackReason != "" {
			t.Fatalf("record=%#v", record)
		}
	})
	t.Run("budget exhausted", func(t *testing.T) {
		upstream := newUpstreams(map[string]func(*http.Request) (*http.Response, error){"owned": refused, "supplier": answerOK(`{"choices":[]}`)})
		directory := routes.New()
		publishPlan(directory, "primary-fallback", endpointRoute("owned", "owned", "qwen3"), managed())
		billing := &captureManagedBilling{}
		records := &allRecords{}
		handler := (&Gateway{Routes: directory, APIKey: "secret", Client: upstream.client(), Recorder: records, ManagedBilling: billing, ExternalAuthorizer: &oneRequestBudget{}}).Handler()
		response := invokeCoder(handler, `{"model":"coder","messages":[]}`)
		if response.Code != http.StatusServiceUnavailable || upstream.count("supplier") != 0 || billing.authorized != 1 || billing.released != 1 {
			t.Fatalf("response=%d sent=%#v billing=%+v", response.Code, upstream.sent, billing)
		}
		if record := records.only(t); record.BindingID != "owned" || record.ErrorType != "upstream_unavailable" || record.FallbackReason != "" {
			t.Fatalf("record=%#v", record)
		}
	})
}

func TestFailoverPinsTheFallbackGenerationUntilTheRequestEnds(t *testing.T) {
	reached := make(chan struct{})
	release := make(chan struct{})
	upstream := newUpstreams(map[string]func(*http.Request) (*http.Response, error){
		"owned": refused,
		"closed": func(*http.Request) (*http.Response, error) {
			close(reached)
			<-release
			return answerOK(`{"choices":[]}`)(nil)
		},
	})
	directory := routes.New()
	primary, fallback := endpointRoute("owned", "owned", "qwen3"), endpointRoute("closed", "closed", "closed-model")
	publishPlan(directory, "primary-fallback", primary, fallback)
	handler := (&Gateway{Routes: directory, APIKey: "secret", Client: upstream.client()}).Handler()
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- invokeCoder(handler, `{"model":"coder","messages":[]}`) }()
	select {
	case <-reached:
	case response := <-done:
		t.Fatalf("request ended without reaching the fallback: %d %s", response.Code, response.Body.String())
	}
	publishPlan(directory, "primary-fallback", primary)
	if pending := directory.RetiringInFlight(fallback.DeploymentID); pending != 1 {
		t.Fatalf("withdrawn fallback generation is not fenced while serving: pending=%d", pending)
	}
	close(release)
	if response := <-done; response.Code != http.StatusOK {
		t.Fatalf("response=%d %s", response.Code, response.Body.String())
	}
	if pending := directory.RetiringInFlight(fallback.DeploymentID); pending != 0 {
		t.Fatalf("fallback generation stayed pinned: pending=%d", pending)
	}
}

func TestConcurrentRequestsAllFailOverToTheFallback(t *testing.T) {
	upstream := newUpstreams(map[string]func(*http.Request) (*http.Response, error){"owned": refused, "closed": answerOK(`{"choices":[]}`)})
	directory := routes.New()
	publishPlan(directory, "primary-fallback", endpointRoute("owned", "owned", "qwen3"), endpointRoute("closed", "closed", "closed-model"))
	records := &allRecords{}
	handler := (&Gateway{Routes: directory, APIKey: "secret", Client: upstream.client(), Recorder: records}).Handler()
	var wait sync.WaitGroup
	for range 45 {
		wait.Go(func() {
			if response := invokeCoder(handler, `{"model":"coder","messages":[]}`); response.Code != http.StatusOK {
				t.Errorf("response=%d %s", response.Code, response.Body.String())
			}
		})
	}
	wait.Wait()
	records.mu.Lock()
	defer records.mu.Unlock()
	if len(records.records) != 45 || upstream.count("closed") != 45 || upstream.count("owned") != 45 {
		t.Fatalf("records=%d sent=%#v", len(records.records), upstream.sent)
	}
	for _, record := range records.records {
		if record.BindingID != "closed" || record.FallbackReason != "upstream_connection_failed" {
			t.Fatalf("record=%#v", record)
		}
	}
}
