package autoscale

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

type staticTargets []string

func (s staticTargets) AutoscalingTargetURLs(context.Context, string) ([]string, error) {
	return s, nil
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
