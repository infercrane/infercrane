package agentprotocol

import (
	"strings"
	"testing"
	"time"
)

func validSync() SyncRequest {
	ttft := 12.5
	tokens := 40
	return SyncRequest{
		Protocol: Version, AgentVersion: "1.0.0", OS: "linux", Arch: "amd64", SentAt: time.Now(),
		Hardware: Hardware{State: "unavailable", Accelerators: []Accelerator{}, Limitations: []string{"none"}},
		Servers:  []Server{{Key: "127.0.0.1-8000", Engine: EngineVLLM, Address: "http://127.0.0.1:8000", Models: []string{"Qwen/Qwen3-8B"}, Health: HealthHealthy}},
		Metrics:  []MetricRelay{{ServerKey: "127.0.0.1-8000", Current: Exposition{AgeMS: 10, Text: "vllm:num_requests_running 1\n"}}},
		Traffic: []TrafficShape{{ID: "abcdefgh12345678", ServerKey: "127.0.0.1-8000", StartedAt: time.Now(), Operation: "chat", StatusCode: 200, LatencyMS: 30, TTFTMS: &ttft, InputTokens: &tokens,
			SharedPrefixHash: strings.Repeat("a", 64)}},
	}
}

func TestSyncValidationBoundsEveryField(t *testing.T) {
	if err := validSync().Validate(time.Now()); err != nil {
		t.Fatalf("valid sync: %v", err)
	}
	cases := map[string]func(*SyncRequest){
		"protocol":         func(r *SyncRequest) { r.Protocol = "infercrane.agent/v0" },
		"agent version":    func(r *SyncRequest) { r.AgentVersion = "" },
		"control char":     func(r *SyncRequest) { r.OS = "linux\n" },
		"hardware state":   func(r *SyncRequest) { r.Hardware.State = "great" },
		"vendor":           func(r *SyncRequest) { r.Hardware.Accelerators = []Accelerator{{Vendor: "acme", Count: 1}} },
		"server key":       func(r *SyncRequest) { r.Servers[0].Key = "Has Space" },
		"duplicate server": func(r *SyncRequest) { r.Servers = append(r.Servers, r.Servers[0]) },
		"engine":           func(r *SyncRequest) { r.Servers[0].Engine = "tgi" },
		"health":           func(r *SyncRequest) { r.Servers[0].Health = "fine" },
		"too many models": func(r *SyncRequest) {
			r.Servers[0].Models = make([]string, MaxModelsPerServer+1)
		},
		"exposition size": func(r *SyncRequest) { r.Metrics[0].Current.Text = strings.Repeat("x", MaxExpositionBytes+1) },
		"exposition age":  func(r *SyncRequest) { r.Metrics[0].Current.AgeMS = -1 },
		"shape id":        func(r *SyncRequest) { r.Traffic[0].ID = "short" },
		"operation":       func(r *SyncRequest) { r.Traffic[0].Operation = "images" },
		"status":          func(r *SyncRequest) { r.Traffic[0].StatusCode = 42 },
		"latency":         func(r *SyncRequest) { r.Traffic[0].LatencyMS = -1 },
		"hash":            func(r *SyncRequest) { r.Traffic[0].SharedPrefixHash = "not-a-hash" },
		"too much traffic": func(r *SyncRequest) {
			r.Traffic = make([]TrafficShape, MaxTrafficShapes+1)
		},
		"gateway key": func(r *SyncRequest) { r.Gateway = &Gateway{Listen: "127.0.0.1:4000", ServerKey: "BAD KEY"} },
	}
	for name, mutate := range cases {
		request := validSync()
		mutate(&request)
		if err := request.Validate(time.Now()); err == nil {
			t.Errorf("%s: invalid request accepted", name)
		}
	}
}

func TestRegisterValidation(t *testing.T) {
	valid := RegisterServerRequest{ServerKey: "127.0.0.1-11434", Endpoint: "llama3.2-3b", Model: "llama3.2:3b", Engine: EngineOllama, Address: "http://127.0.0.1:11434"}
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*RegisterServerRequest){
		func(r *RegisterServerRequest) { r.Endpoint = "Llama 3" },
		func(r *RegisterServerRequest) { r.Model = "" },
		func(r *RegisterServerRequest) { r.Engine = "triton" },
		func(r *RegisterServerRequest) { r.Address = "" },
	} {
		request := valid
		mutate(&request)
		if request.Validate() == nil {
			t.Fatalf("invalid registration accepted: %#v", request)
		}
	}
}
