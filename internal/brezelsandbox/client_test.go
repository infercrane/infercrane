package brezelsandbox

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/sandboxprovider"
)

func TestClientMapsApprovedTemplateAndPreservesLifecycle(t *testing.T) {
	revision := "envr_" + strings.Repeat("a", 24)
	now := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer token" || r.Header.Get("X-Project-ID") != "project-a" {
			t.Fatalf("missing Brezel authorization headers")
		}
		if r.Method != http.MethodPost || r.URL.Path != "/v1/sandboxes" || r.Header.Get("Idempotency-Key") != "create-1" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		var body struct {
			EnvironmentRevision string          `json:"environment_revision"`
			Lifecycle           brezelLifecycle `json:"lifecycle"`
			Network             brezelNetwork   `json:"network"`
			WorkspaceMounts     []struct {
				WorkspaceID string `json:"workspace_id"`
				Path        string `json:"path"`
			} `json:"workspace_mounts"`
			ConnectorRevisions []string `json:"connector_revisions"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body.EnvironmentRevision != revision || body.Lifecycle.ExpiresAfterSeconds != 900 || body.Lifecycle.StandbyAfterSeconds != 300 || !body.Lifecycle.AutoResume || body.Network.AllowInternet || len(body.WorkspaceMounts) != 1 || body.WorkspaceMounts[0].WorkspaceID != "workspace-1" || body.WorkspaceMounts[0].Path != "/workspace" || len(body.ConnectorRevisions) != 1 || body.ConnectorRevisions[0] != "connr_"+strings.Repeat("b", 24) {
			t.Fatalf("unexpected create body: %#v", body)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"resource":  map[string]any{"id": "sandbox-1", "environment_revision": revision, "state": "running", "lifecycle": body.Lifecycle, "network": body.Network, "created_at": now, "updated_at": now, "expires_at": now.Add(15 * time.Minute)},
			"operation": map[string]any{"id": "operation-1", "kind": "create_sandbox", "resource_id": "sandbox-1", "state": "succeeded", "created_at": now, "updated_at": now},
		})
	}))
	defer server.Close()

	client, err := New(Config{BaseURL: server.URL, Token: "token", ProjectID: "project-a", AllowedTenant: "tenant-a", Templates: map[string]string{"python-agent": revision}, DefaultTemplate: "python-agent", Client: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	mutation, err := client.Create(context.Background(), "tenant-a", "create-1", sandboxprovider.CreateRequest{WorkspaceID: "workspace-1", ConnectorRevision: "connr_" + strings.Repeat("b", 24), ExpiresAfterSeconds: 900, StandbyAfterSeconds: 300, AutoResume: true, NetworkMode: "offline"})
	if err != nil {
		t.Fatal(err)
	}
	if mutation.Resource.ID != "sandbox-1" || mutation.Resource.TemplateID != "python-agent" || mutation.Resource.Provider != "brezel" || mutation.Resource.Network.Mode != "offline" || mutation.Operation.State != "succeeded" {
		t.Fatalf("unexpected mutation: %#v", mutation)
	}
}

func TestClientRejectsUnapprovedScopeBeforeCallingBrezel(t *testing.T) {
	revision := "envr_" + strings.Repeat("b", 24)
	client, err := New(Config{BaseURL: "http://127.0.0.1:1", Token: "token", ProjectID: "project-a", AllowedTenant: "tenant-a", Templates: map[string]string{"base": revision}, DefaultTemplate: "base"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = client.Create(context.Background(), "tenant-b", "create-1", sandboxprovider.CreateRequest{}); !errors.Is(err, sandboxprovider.ErrForbidden) {
		t.Fatalf("cross-tenant error = %v", err)
	}
	if _, err = client.Create(context.Background(), "tenant-a", "create-2", sandboxprovider.CreateRequest{TemplateID: "unknown"}); !errors.Is(err, sandboxprovider.ErrInvalid) {
		t.Fatalf("unknown template error = %v", err)
	}
	if _, err = client.Create(context.Background(), "tenant-a", "create-3", sandboxprovider.CreateRequest{NetworkMode: "internet"}); !errors.Is(err, sandboxprovider.ErrInvalid) {
		t.Fatalf("network error = %v", err)
	}
}

func TestCapabilitiesDoNotClaimPTYOrGPU(t *testing.T) {
	revision := "envr_" + strings.Repeat("c", 24)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"runtime": "microvm", "qualification": "unverified", "qualification_note": "run conformance",
			"implemented": map[string]bool{"hostile_code_isolation": true, "deny_by_default_egress": true, "filesystem_checkpoint": true, "command_streaming": true, "file_read_write": true, "authenticated_ports": true, "durable_workspaces": true},
		})
	}))
	defer server.Close()
	client, err := New(Config{BaseURL: server.URL, Token: "token", ProjectID: "project-a", AllowedTenant: "tenant-a", Templates: map[string]string{"base": revision}, DefaultTemplate: "base", Client: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	capabilities, err := client.Capabilities(context.Background(), "tenant-a")
	if err != nil {
		t.Fatal(err)
	}
	if capabilities.State != "ready" || capabilities.Assurance != "private-tenant-preview" || !capabilities.Features.HTTPPreview || capabilities.Features.InteractivePTY || capabilities.Features.GPU {
		t.Fatalf("unsafe capability projection: %#v", capabilities)
	}
}

func TestCommandEventsAreDeliveredBeforeTheStreamCompletes(t *testing.T) {
	revision := "envr_" + strings.Repeat("d", 24)
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/sandboxes/sandbox-1/commands" || r.Method != http.MethodPost {
			t.Fatalf("unexpected command request %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		_ = json.NewEncoder(w).Encode(map[string]any{"execution_id": "exec-1", "type": "stdout", "data": []byte("first")})
		w.(http.Flusher).Flush()
		<-release
		code := int32(0)
		_ = json.NewEncoder(w).Encode(map[string]any{"execution_id": "exec-1", "type": "exited", "exit_code": code, "exited": true})
	}))
	defer server.Close()
	client, err := New(Config{BaseURL: server.URL, Token: "token", ProjectID: "project-a", AllowedTenant: "tenant-a", Templates: map[string]string{"base": revision}, DefaultTemplate: "base", Client: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	first := make(chan sandboxprovider.CommandEvent, 1)
	result := make(chan error, 1)
	go func() {
		_, runErr := client.RunCommand(context.Background(), "tenant-a", "sandbox-1", sandboxprovider.CommandRequest{Argv: []string{"true"}}, func(event sandboxprovider.CommandEvent) error {
			if event.Type == "stdout" {
				first <- event
			}
			return nil
		})
		result <- runErr
	}()
	select {
	case event := <-first:
		if string(event.Data) != "first" {
			t.Fatalf("first chunk = %q", event.Data)
		}
	case <-time.After(time.Second):
		t.Fatal("first command chunk was buffered until stream completion")
	}
	close(release)
	if err = <-result; err != nil {
		t.Fatal(err)
	}
}

func TestProviderCredentialsNeverFollowRedirects(t *testing.T) {
	revision := "envr_" + strings.Repeat("e", 24)
	credentialReachedSink := false
	sink := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		credentialReachedSink = r.Header.Get("Authorization") != ""
	}))
	defer sink.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, sink.URL, http.StatusFound)
	}))
	defer server.Close()
	client, err := New(Config{BaseURL: server.URL, Token: "token", ProjectID: "project-a", AllowedTenant: "tenant-a", Templates: map[string]string{"base": revision}, DefaultTemplate: "base", Client: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.ProxyPreview(context.Background(), "tenant-a", "/p/opaque/", sandboxprovider.PreviewRequest{Method: http.MethodGet})
	if response.Body != nil {
		response.Body.Close()
	}
	if err == nil || credentialReachedSink {
		t.Fatalf("redirect err=%v credential_reached_sink=%v", err, credentialReachedSink)
	}
}
