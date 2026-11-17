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

func TestSyncValidationBoundsTheEnvelopeAndEachItem(t *testing.T) {
	request := validSync()
	if err := request.Validate(time.Now()); err != nil {
		t.Fatalf("valid sync: %v", err)
	}
	if err := request.Servers[0].Validate(); err != nil {
		t.Fatal(err)
	}
	if err := request.Metrics[0].Validate(); err != nil {
		t.Fatal(err)
	}
	if err := request.Traffic[0].Validate(); err != nil {
		t.Fatal(err)
	}
	envelope := map[string]func(*SyncRequest){
		"protocol":         func(r *SyncRequest) { r.Protocol = "infercrane.agent/v0" },
		"agent version":    func(r *SyncRequest) { r.AgentVersion = "" },
		"control char":     func(r *SyncRequest) { r.OS = "linux\n" },
		"hardware state":   func(r *SyncRequest) { r.Hardware.State = "great" },
		"vendor":           func(r *SyncRequest) { r.Hardware.Accelerators = []Accelerator{{Vendor: "acme", Count: 1}} },
		"too many servers": func(r *SyncRequest) { r.Servers = make([]Server, MaxServers+1) },
		"too many traffic": func(r *SyncRequest) { r.Traffic = make([]TrafficShape, MaxTrafficShapes+1) },
		"gateway key":      func(r *SyncRequest) { r.Gateway = &Gateway{Listen: "127.0.0.1:4000", ServerKey: "BAD KEY"} },
	}
	for name, mutate := range envelope {
		request := validSync()
		mutate(&request)
		if err := request.Validate(time.Now()); err == nil {
			t.Errorf("envelope %s: invalid request accepted", name)
		}
	}
	server := map[string]func(*Server){
		"key":    func(s *Server) { s.Key = "Has Space" },
		"engine": func(s *Server) { s.Engine = "tgi" },
		"health": func(s *Server) { s.Health = "fine" },
		"models": func(s *Server) { s.Models = make([]string, MaxModelsPerServer+1) },
		"model":  func(s *Server) { s.Models = []string{"qwen\tx"} },
	}
	for name, mutate := range server {
		item := validSync().Servers[0]
		mutate(&item)
		if item.Validate() == nil {
			t.Errorf("server %s: invalid item accepted", name)
		}
	}
	for name, mutate := range map[string]func(*MetricRelay){
		"size": func(r *MetricRelay) { r.Current.Text = strings.Repeat("x", MaxExpositionBytes+1) },
		"age":  func(r *MetricRelay) { r.Current.AgeMS = -1 },
		"key":  func(r *MetricRelay) { r.ServerKey = "" },
	} {
		item := validSync().Metrics[0]
		mutate(&item)
		if item.Validate() == nil {
			t.Errorf("metrics %s: invalid item accepted", name)
		}
	}
	for name, mutate := range map[string]func(*TrafficShape){
		"id":        func(s *TrafficShape) { s.ID = "short" },
		"operation": func(s *TrafficShape) { s.Operation = "images" },
		"status":    func(s *TrafficShape) { s.StatusCode = 42 },
		"latency":   func(s *TrafficShape) { s.LatencyMS = -1 },
		"hash":      func(s *TrafficShape) { s.SharedPrefixHash = "not-a-hash" },
		"model":     func(s *TrafficShape) { s.RequestModel = "qwen\nx" },
	} {
		item := validSync().Traffic[0]
		mutate(&item)
		if item.Validate() == nil {
			t.Errorf("traffic %s: invalid item accepted", name)
		}
	}
}

func TestCleanTextAlwaysPassesValidation(t *testing.T) {
	for _, value := range []string{strings.Repeat("a", 255) + "é", "tab\there\n", strings.Repeat("日本", 100), "\x7fdel", "ok"} {
		cleaned := CleanText(value, MaxText)
		if err := boundedText("field", cleaned, false); err != nil || len(cleaned) > MaxText {
			t.Fatalf("CleanText(%q)=%q: %v", value, cleaned, err)
		}
	}
	if CleanText("  qwen\tx  ", MaxText) != "qwenx" {
		t.Fatal("control characters were not removed")
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
