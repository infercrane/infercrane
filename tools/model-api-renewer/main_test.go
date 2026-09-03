package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestRenewProfileSkipsWithoutSupplierTrafficOutsideWindow(t *testing.T) {
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	server := catalogServer(t, now.Add(7*time.Hour))
	defer server.Close()
	t.Setenv("INFERCRANE_MODEL_API_DEEPSEEK_CREDENTIAL_REFERENCE", "deepseek-secret-ref")
	cfg := testConfig(t, server.URL)
	commands := 0
	err := renewProfile(context.Background(), cfg, renewalProfiles[0], now, func(context.Context, string, ...string) error {
		commands++
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if commands != 0 {
		t.Fatalf("ran %d commands outside the renewal window", commands)
	}
}

func TestCurrentCatalogStateDecodesProductionDetailEnvelope(t *testing.T) {
	expiry := time.Date(2026, 9, 4, 16, 14, 25, 742732000, time.UTC)
	server := catalogServer(t, expiry)
	defer server.Close()
	state, err := currentCatalogState(context.Background(), testConfig(t, server.URL), "deepseek-v4-flash")
	if err != nil {
		t.Fatal(err)
	}
	if !state.Model.Callable || state.Model.EvidenceValidUntil == nil || !state.Model.EvidenceValidUntil.Equal(expiry) {
		t.Fatalf("production detail envelope decoded as %+v", state.Model)
	}
}

func TestRenewProfileUsesBoundedQualificationAndImmutableVersion(t *testing.T) {
	now := time.Date(2026, 9, 3, 12, 34, 56, 0, time.UTC)
	server := catalogServer(t, now.Add(5*time.Hour))
	defer server.Close()
	t.Setenv("INFERCRANE_MODEL_API_ZAI_CREDENTIAL_REFERENCE", "zai-secret-ref")
	cfg := testConfig(t, server.URL)
	type invocation struct {
		name string
		args []string
	}
	var calls []invocation
	err := renewProfile(context.Background(), cfg, renewalProfiles[1], now, func(_ context.Context, name string, args ...string) error {
		calls = append(calls, invocation{name: name, args: append([]string(nil), args...)})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 10 {
		t.Fatalf("ran %d commands, want qualifier + release + 8 renewal publications", len(calls))
	}
	if calls[0].name != "infercrane-model-api-qualifier" || !hasPair(calls[0].args, "--max-output-tokens", "512") || !hasPair(calls[0].args, "--samples-per-mode", "3") || !hasPair(calls[0].args, "--valid-for", "24h0m0s") {
		t.Fatalf("qualification is not bounded as required: %q %q", calls[0].name, calls[0].args)
	}
	wantVersion := fmt.Sprint(now.Unix())
	if !hasPair(calls[0].args, "--offer-version", wantVersion) || calls[1].name != "infercrane-model-api-mvp-release" || !hasPair(calls[1].args, "--release-version", wantVersion) {
		t.Fatalf("qualification and release do not share immutable version %s", wantVersion)
	}
	wantTypes := []string{"rate", "offer", "qualification", "target-binding", "plan", "product", "publication", "entitlement"}
	for index, want := range wantTypes {
		call := calls[index+2]
		if call.name != "infercrane" || len(call.args) < 4 || call.args[0] != "model-api" || call.args[1] != "publish" || call.args[2] != want {
			t.Fatalf("publication %d = %q %q, want %s", index, call.name, call.args, want)
		}
	}
	if !strings.HasPrefix(calls[0].args[slices.Index(calls[0].args, "--evidence-ref")+1], "fly-machine://machine-1/") {
		t.Fatal("evidence is not bound to the persistent Fly Machine")
	}
}

func TestConfigRequiresHTTPSAndFlyIdentity(t *testing.T) {
	cfg := testConfig(t, "http://control.example")
	if err := cfg.validate(); err == nil || !strings.Contains(err.Error(), "HTTPS") {
		t.Fatalf("HTTP control URL error=%v", err)
	}
	cfg.ControlURL = "https://control.example"
	cfg.MachineID = ""
	if err := cfg.validate(); err == nil || !strings.Contains(err.Error(), "FLY_MACHINE_ID") {
		t.Fatalf("missing machine identity error=%v", err)
	}
}

func catalogServer(t *testing.T, expiry time.Time) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer control-key" {
			t.Fatalf("missing control authentication")
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"model":{"id":"model","callable":true,"evidence_valid_until":%q},"access":{"authorized":true},"catalog_source":"durable_product_catalog"}`, expiry.UTC().Format(time.RFC3339Nano))
	}))
}

func testConfig(t *testing.T, controlURL string) config {
	t.Helper()
	return config{
		ControlURL: controlURL, APIKey: "control-key", OperatorWorkspace: "global", ServingPlan: "serving-plan",
		CustomerWorkspace: "customer-workspace", MachineID: "machine-1", StateDirectory: filepath.Join(t.TempDir(), "renewals"), RenewBefore: defaultRenewBefore,
	}
}

func hasPair(values []string, name, value string) bool {
	index := slices.Index(values, name)
	return index >= 0 && index+1 < len(values) && values[index+1] == value
}
