package autoscale

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

type staticTargets []string

func (s staticTargets) AutoscalingTargets(context.Context, string) ([]Target, error) {
	targets := make([]Target, 0, len(s))
	for _, url := range s {
		targets = append(targets, Target{URL: url})
	}
	return targets, nil
}

type fixedTargets []Target

func (s fixedTargets) AutoscalingTargets(context.Context, string) ([]Target, error) { return s, nil }

func TestEngineSignalsSendEachWorkerOnlyItsOwnCredential(t *testing.T) {
	seen := map[string]string{}
	handler := func(name string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			seen[name] = r.Header.Get("Authorization")
			_, _ = w.Write([]byte("vllm:num_requests_running 1\nvllm:num_requests_waiting 0\n"))
		}
	}
	scoped, shared := httptest.NewServer(handler("scoped")), httptest.NewServer(handler("shared"))
	defer scoped.Close()
	defer shared.Close()
	credential := func(deploymentID string, target Target) string {
		if target.ProviderDetails == "scoped" {
			return "key-of-" + target.TenantID + "-" + deploymentID
		}
		return "shared-key"
	}
	targets := fixedTargets{{URL: scoped.URL, TenantID: "tenant-a", ProviderDetails: "scoped"}, {URL: shared.URL, TenantID: "tenant-a"}}
	signals, err := (VLLMSignals{Targets: targets, Credential: credential}).Signals(context.Background(), "deployment-a")
	if err != nil || signals.Running != 2 {
		t.Fatalf("signals=%#v err=%v", signals, err)
	}
	if seen["scoped"] != "Bearer key-of-tenant-a-deployment-a" || seen["shared"] != "Bearer shared-key" {
		t.Fatalf("workers were sent %#v", seen)
	}
}

func TestEngineSignalsDoNotCallAWorkerWithoutACredential(t *testing.T) {
	called := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		_, _ = w.Write([]byte("vllm:num_requests_running 1\nvllm:num_requests_waiting 0\n"))
	}))
	defer server.Close()
	none := func(string, Target) string { return "" }
	if _, err := (VLLMSignals{Targets: fixedTargets{{URL: server.URL, ComputeConnectionID: "connection-a"}}, Credential: none}).Signals(context.Background(), "deployment-a"); err == nil {
		t.Fatal("a worker without a credential was scraped")
	}
	if called {
		t.Fatal("a request reached a worker that has no credential to be sent")
	}
}

func TestVLLMSignalsAggregatesRuntimeQueueGauges(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("vllm:num_requests_running 2\nvllm:num_requests_waiting 3\n"))
	}))
	defer server.Close()
	signals, err := (VLLMSignals{Targets: staticTargets{server.URL, server.URL}}).Signals(context.Background(), "deployment")
	if err != nil || signals.Running != 4 || signals.Waiting != 6 {
		t.Fatalf("signals=%#v err=%v", signals, err)
	}
}

func TestEngineSignalsReadSGLangAndLlamaCPPQueueGauges(t *testing.T) {
	for _, test := range []struct {
		name, payload    string
		running, waiting float64
	}{
		{"sglang", "sglang:num_running_reqs{tp_rank=\"0\",priority=\"\"} 5\nsglang:num_running_reqs{tp_rank=\"0\",priority=\"1\"} 5\nsglang:num_queue_reqs{tp_rank=\"0\",priority=\"\"} 9\n", 5, 9},
		{"llamacpp", "llamacpp:requests_processing 2\nllamacpp:requests_deferred 1\n", 2, 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(test.payload))
			}))
			defer server.Close()
			signals, err := (VLLMSignals{Targets: staticTargets{server.URL}}).Signals(context.Background(), "deployment")
			if err != nil || signals.Running != test.running || signals.Waiting != test.waiting {
				t.Fatalf("signals=%#v err=%v", signals, err)
			}
		})
	}
}

func TestVLLMSignalsRejectsUnavailableEvidence(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("unrelated_metric 1\n"))
	}))
	defer server.Close()
	if _, err := (VLLMSignals{Targets: staticTargets{server.URL}}).Signals(context.Background(), "deployment"); err == nil {
		t.Fatal("missing queue gauges must not be interpreted as zero")
	}
	partial := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("sglang:num_running_reqs 3\n"))
	}))
	defer partial.Close()
	if _, err := (VLLMSignals{Targets: staticTargets{partial.URL}}).Signals(context.Background(), "deployment"); err == nil {
		t.Fatal("a missing waiting gauge must not be interpreted as zero")
	}
}
