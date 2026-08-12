package controlapi

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDiscoverEndpointSelectsSingleModelAndConservativelyClassifiesRuntime(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		w.Header().Set("Server", "unrelated-gateway")
		_, _ = w.Write([]byte(`{"data":[{"id":"acme/coder"}]}`))
	}))
	defer server.Close()

	got, err := discoverEndpoint(context.Background(), server.Client(), server.URL+"/v1", "", "auto")
	if err != nil {
		t.Fatal(err)
	}
	if got.Model != "acme/coder" || got.Runtime != "openai-compatible" || got.Connector != "openai-compatible" || got.Health != "reachable" {
		t.Fatalf("discovery = %+v", got)
	}
}

func TestDiscoverEndpointRequiresSelectionForMultipleModels(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"second"},{"id":"first"}]}`))
	}))
	defer server.Close()

	_, err := discoverEndpoint(context.Background(), server.Client(), server.URL, "", "auto")
	if err == nil || !strings.Contains(err.Error(), "select one with --model") || !strings.Contains(err.Error(), "first, second") {
		t.Fatalf("error = %v", err)
	}
}

func TestDiscoverEndpointNamesLiteLLMOnlyFromEvidence(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"coder"}]}`))
	}))
	defer server.Close()

	got, err := discoverEndpoint(context.Background(), server.Client(), server.URL, "coder", "litellm")
	if err != nil {
		t.Fatal(err)
	}
	if got.Runtime != "litellm" || got.Connector != "litellm" {
		t.Fatalf("discovery = %+v", got)
	}
}

func TestDiscoverEndpointDoesNotFollowRedirectOrAcceptUnknownModel(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("redirect followed")
	}))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer redirect.Close()
	if _, err := discoverEndpoint(context.Background(), redirect.Client(), redirect.URL, "coder", "auto"); err == nil || !strings.Contains(err.Error(), "HTTP 302") {
		t.Fatalf("redirect error = %v", err)
	}

	models := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"other"}]}`))
	}))
	defer models.Close()
	if _, err := discoverEndpoint(context.Background(), models.Client(), models.URL, "coder", "auto"); err == nil || !strings.Contains(err.Error(), "not present") {
		t.Fatalf("model error = %v", err)
	}
}

func TestDiscoveryRejectsCloudMetadataAddressClasses(t *testing.T) {
	for _, raw := range []string{"169.254.169.254", "fe80::1", "224.0.0.1", "0.0.0.0"} {
		if !unsafeDiscoveryIP(net.ParseIP(raw)) {
			t.Fatalf("address %s was accepted", raw)
		}
	}
	for _, raw := range []string{"127.0.0.1", "10.0.0.4", "192.168.1.3"} {
		if unsafeDiscoveryIP(net.ParseIP(raw)) {
			t.Fatalf("internal inference address %s was rejected", raw)
		}
	}
}
