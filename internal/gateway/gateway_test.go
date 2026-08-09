package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/routes"
)

type fakeAuthenticator struct{ principal domain.Principal }

func (f fakeAuthenticator) AuthenticatePrincipal(context.Context, string) (domain.Principal, error) {
	return f.principal, nil
}

type roundTripper func(*http.Request) (*http.Response, error)

func (fn roundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return fn(r) }

func TestCompletionRewritesAlias(t *testing.T) {
	client := &http.Client{Transport: roundTripper(func(r *http.Request) (*http.Response, error) {
		if !validTraceParent.MatchString(r.Header.Get("traceparent")) {
			t.Errorf("invalid traceparent %q", r.Header.Get("traceparent"))
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["model"] != "upstream-model" {
			t.Errorf("model = %v", body["model"])
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("data: [DONE]\n\n"))}, nil
	})}
	directory := routes.New()
	directory.Put(routes.Snapshot{DeploymentID: "d1", Alias: "alias", UpstreamModel: "upstream-model", RouterURL: "http://router"})
	handler := (&Gateway{Routes: directory, APIKey: "secret", Client: client}).Handler()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"alias","messages":[]}`))
	req.Header.Set("Authorization", "Bearer secret")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	if recorder.Code != 200 {
		t.Fatalf("status = %d", recorder.Code)
	}
	if !validTraceParent.MatchString(recorder.Header().Get("traceparent")) {
		t.Fatalf("response traceparent=%q", recorder.Header().Get("traceparent"))
	}
}

func TestAuthentication(t *testing.T) {
	handler := (&Gateway{Routes: routes.New(), APIKey: "secret"}).Handler()
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d", recorder.Code)
	}
}

func TestModelsAreTenantScoped(t *testing.T) {
	directory := routes.New()
	directory.Put(routes.Snapshot{TenantID: "tenant-a", Alias: "shared"})
	directory.Put(routes.Snapshot{TenantID: "tenant-b", Alias: "private"})
	handler := (&Gateway{Routes: directory, Authenticator: fakeAuthenticator{principal: domain.Principal{ID: "p", TenantID: "tenant-a", Role: "viewer"}}}).Handler()
	request := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	request.Header.Set("Authorization", "Bearer scoped")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != 200 || !strings.Contains(response.Body.String(), "shared") || strings.Contains(response.Body.String(), "private") {
		t.Fatalf("response=%d %s", response.Code, response.Body.String())
	}
}

func TestActiveStreamKeepsSelectedRouterAcrossGenerationPublish(t *testing.T) {
	selected := make(chan string, 1)
	release := make(chan struct{})
	client := &http.Client{Transport: roundTripper(func(r *http.Request) (*http.Response, error) {
		selected <- r.URL.Host
		<-release
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("data: first\n\ndata: [DONE]\n\n"))}, nil
	})}
	directory := routes.New()
	directory.Put(routes.Snapshot{DeploymentID: "d1", Alias: "alias", UpstreamModel: "model", RouterURL: "http://router-g1", RouterProcessID: "d1-g1"})
	handler := (&Gateway{Routes: directory, APIKey: "secret", Client: client}).Handler()
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"alias","stream":true,"messages":[]}`))
	request.Header.Set("Authorization", "Bearer secret")
	response := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		handler.ServeHTTP(response, request)
		close(done)
	}()
	if host := <-selected; host != "router-g1" {
		t.Fatalf("selected router=%s", host)
	}
	directory.Put(routes.Snapshot{DeploymentID: "d1", Alias: "alias", UpstreamModel: "model", RouterURL: "http://router-g2", RouterProcessID: "d1-g2"})
	close(release)
	<-done
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "[DONE]") {
		t.Fatalf("response=%d %s", response.Code, response.Body.String())
	}
}
