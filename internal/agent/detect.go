// Package agent is the InferCrane agent: one outbound-only process that runs
// beside a customer's model server. It finds local OpenAI-compatible servers,
// reports their health, engine, version, models, and the host's hardware,
// relays filtered engine metrics, and records content-free request shapes
// when traffic passes through its local gateway. It never opens a port that
// the control plane connects to, and prompts and completions never leave the
// machine.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/infercrane/infercrane/internal/agentprotocol"
	"github.com/infercrane/infercrane/internal/metrics"
)

// DefaultPorts are the loopback ports common local servers listen on:
// vLLM 8000, SGLang 30000, llama.cpp 8080, Ollama 11434, and LM Studio 1234.
// A port only says where to look; the engine is identified from what the
// server returns.
var DefaultPorts = []int{8000, 30000, 8080, 11434, 1234}

const (
	probeTimeout  = 2 * time.Second
	maxProbeBytes = 1 << 20
)

// ServerConfig is a server the user named explicitly.
type ServerConfig struct {
	URL string
	// AuthHeader is sent as Authorization on every local request, for a
	// server started with an API key. It never leaves the machine.
	AuthHeader string
}

// Detected is one local server and what the agent could read from it.
type Detected struct {
	agentprotocol.Server
	BaseURL    string
	authHeader string
	// Exposition is the filtered metrics text from the probe, when the
	// server exposes engine metrics.
	Exposition string
}

// Detector probes local servers. HTTP is a client that must not use a proxy:
// local traffic stays local.
type Detector struct {
	HTTP *http.Client
}

func (d Detector) client() *http.Client {
	if d.HTTP != nil {
		return d.HTTP
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	return &http.Client{Timeout: probeTimeout, Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// Candidates returns the explicit servers, or the default loopback ports
// when autodetect is on, without duplicates.
func Candidates(explicit []ServerConfig, autodetect bool, defaultAuth string) []ServerConfig {
	seen := map[string]struct{}{}
	out := []ServerConfig{}
	add := func(config ServerConfig) {
		base, err := NormalizeBaseURL(config.URL)
		if err != nil {
			return
		}
		if _, ok := seen[base]; ok {
			return
		}
		seen[base] = struct{}{}
		config.URL = base
		out = append(out, config)
	}
	for _, config := range explicit {
		add(config)
	}
	if autodetect {
		for _, port := range DefaultPorts {
			add(ServerConfig{URL: fmt.Sprintf("http://127.0.0.1:%d", port), AuthHeader: defaultAuth})
		}
	}
	return out
}

// NormalizeBaseURL turns a server or OpenAI SDK base URL into the server
// root the agent probes.
func NormalizeBaseURL(raw string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errors.New("server URL must be an absolute http(s) URL without credentials, query, or fragment")
	}
	parsed.Path = strings.TrimSuffix(strings.TrimRight(parsed.Path, "/"), "/v1")
	return strings.TrimRight(parsed.String(), "/"), nil
}

// ServerKey is the stable, agent-local identity of a server: its host and
// port. It does not depend on the engine, which may only become known later.
func ServerKey(baseURL string) string {
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return ""
	}
	host, port := parsed.Hostname(), parsed.Port()
	if port == "" {
		port = "80"
		if parsed.Scheme == "https" {
			port = "443"
		}
	}
	if ip := net.ParseIP(host); ip != nil && strings.Contains(host, ":") {
		host = "ip6-" + strings.ReplaceAll(host, ":", ".")
	}
	var b strings.Builder
	for _, r := range strings.ToLower(host) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	key := strings.Trim(b.String(), "-.") + "-" + port
	if len(key) > 128 {
		key = key[len(key)-128:]
	}
	return strings.TrimLeft(key, "-.")
}

// Probe reads one server. found is false when nothing answers on a default
// port; an explicit server that does not answer is reported unhealthy.
func (d Detector) Probe(ctx context.Context, config ServerConfig, explicit bool) (Detected, bool) {
	base := config.URL
	out := Detected{BaseURL: base, authHeader: config.AuthHeader}
	out.Key = ServerKey(base)
	out.Address = base
	out.Engine = agentprotocol.EngineOpenAICompatible
	out.Models = []string{}
	out.Authenticated = config.AuthHeader != ""
	models, status, err := d.models(ctx, base, config.AuthHeader)
	if err != nil {
		if !explicit {
			return Detected{}, false
		}
		out.Health, out.HealthDetail = agentprotocol.HealthUnhealthy, boundedDetail(err.Error())
		out.MetricsDetail = "the server did not answer"
		return out, true
	}
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		out.Health = agentprotocol.HealthUnhealthy
		out.HealthDetail = fmt.Sprintf("GET /v1/models returned HTTP %d; pass the server's API key with --auth-header-env or --token-env", status)
	case status != http.StatusOK:
		if !explicit && status == http.StatusNotFound {
			return Detected{}, false
		}
		out.Health = agentprotocol.HealthUnhealthy
		out.HealthDetail = fmt.Sprintf("GET /v1/models returned HTTP %d", status)
	case len(models) == 0:
		out.Health, out.HealthDetail = agentprotocol.HealthUnhealthy, "GET /v1/models listed no models"
	default:
		out.Health = agentprotocol.HealthHealthy
		out.Models = models
	}
	d.identify(ctx, &out)
	return out, true
}

func (d Detector) get(ctx context.Context, base, path, auth string) (int, []byte, http.Header, error) {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, base+path, nil)
	if err != nil {
		return 0, nil, nil, err
	}
	if auth != "" {
		request.Header.Set("Authorization", auth)
	}
	response, err := d.client().Do(request)
	if err != nil {
		var netErr net.Error
		if errors.As(err, &netErr) && netErr.Timeout() {
			return 0, nil, nil, fmt.Errorf("%s did not answer within %s", base, probeTimeout)
		}
		return 0, nil, nil, fmt.Errorf("could not reach %s", base)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxProbeBytes+1))
	if err != nil {
		return response.StatusCode, nil, response.Header, err
	}
	if len(body) > maxProbeBytes {
		return response.StatusCode, nil, response.Header, fmt.Errorf("%s%s returned more than %d bytes", base, path, maxProbeBytes)
	}
	return response.StatusCode, body, response.Header, nil
}

func (d Detector) getIf(ctx context.Context, condition bool, base, path, auth string) (int, []byte, http.Header, error) {
	if !condition {
		return 0, nil, nil, errors.New("skipped")
	}
	return d.get(ctx, base, path, auth)
}

func (d Detector) models(ctx context.Context, base, auth string) ([]string, int, error) {
	status, body, _, err := d.get(ctx, base, "/v1/models", auth)
	if err != nil {
		return nil, 0, err
	}
	if status != http.StatusOK {
		return nil, status, nil
	}
	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if json.Unmarshal(body, &payload) != nil {
		return nil, http.StatusBadGateway, nil
	}
	models := make([]string, 0, len(payload.Data))
	for _, item := range payload.Data {
		if id := strings.TrimSpace(item.ID); id != "" && len(id) <= 256 && len(models) < agentprotocol.MaxModelsPerServer {
			models = append(models, id)
		}
	}
	sort.Strings(models)
	return models, status, nil
}

// identify reads engine, version, active model, and metrics. Each source is
// optional; what cannot be read stays empty and is explained.
func (d Detector) identify(ctx context.Context, out *Detected) {
	auth := out.authHeader
	if status, body, _, err := d.get(ctx, out.BaseURL, "/metrics", auth); err == nil && status == http.StatusOK {
		if filtered, engine, filterErr := metrics.FilterEngineExposition(string(body)); filterErr == nil {
			out.MetricsAvailable = true
			out.Exposition = filtered
			out.Engine = map[string]string{metrics.EngineVLLM: agentprotocol.EngineVLLM, metrics.EngineSGLang: agentprotocol.EngineSGLang, metrics.EngineLlamaCPP: agentprotocol.EngineLlamaCPP}[engine]
		} else {
			out.MetricsDetail = "GET /metrics answered without vLLM, SGLang, or llama.cpp engine metrics"
		}
	}
	// Ollama: /api/version and /api/ps (loaded models).
	if status, body, _, err := d.getIf(ctx, out.Engine == agentprotocol.EngineOpenAICompatible, out.BaseURL, "/api/version", auth); err == nil && status == http.StatusOK {
		var version struct {
			Version string `json:"version"`
		}
		if json.Unmarshal(body, &version) == nil && version.Version != "" {
			out.Engine, out.EngineVersion = agentprotocol.EngineOllama, boundedDetail(version.Version)
			if status, body, _, err = d.get(ctx, out.BaseURL, "/api/ps", auth); err == nil && status == http.StatusOK {
				var loaded struct {
					Models []struct {
						Name  string `json:"name"`
						Model string `json:"model"`
					} `json:"models"`
				}
				if json.Unmarshal(body, &loaded) == nil && len(loaded.Models) > 0 {
					out.ActiveModel = boundedDetail(firstNonEmpty(loaded.Models[0].Model, loaded.Models[0].Name))
				}
			}
		}
	}
	// LM Studio: /api/v0/models marks the loaded model.
	if out.Engine == agentprotocol.EngineOpenAICompatible {
		if status, body, _, err := d.get(ctx, out.BaseURL, "/api/v0/models", auth); err == nil && status == http.StatusOK {
			var listing struct {
				Data []struct {
					ID    string `json:"id"`
					State string `json:"state"`
					Type  string `json:"type"`
				} `json:"data"`
			}
			if json.Unmarshal(body, &listing) == nil && len(listing.Data) > 0 {
				out.Engine = agentprotocol.EngineLMStudio
				for _, item := range listing.Data {
					if item.State == "loaded" && item.Type != "embeddings" {
						out.ActiveModel = boundedDetail(item.ID)
						break
					}
				}
			}
		}
	}
	// vLLM: /version.
	if out.Engine == agentprotocol.EngineVLLM || out.Engine == agentprotocol.EngineOpenAICompatible {
		if status, body, _, err := d.get(ctx, out.BaseURL, "/version", auth); err == nil && status == http.StatusOK {
			var version struct {
				Version string `json:"version"`
			}
			if json.Unmarshal(body, &version) == nil && version.Version != "" {
				out.EngineVersion = boundedDetail(version.Version)
				if out.Engine == agentprotocol.EngineOpenAICompatible {
					out.Engine = agentprotocol.EngineVLLM
				}
			}
		}
	}
	// SGLang: /server_info (newer) or /get_server_info.
	if out.Engine == agentprotocol.EngineSGLang || out.Engine == agentprotocol.EngineOpenAICompatible {
		for _, path := range []string{"/server_info", "/get_server_info"} {
			status, body, _, err := d.get(ctx, out.BaseURL, path, auth)
			if err != nil || status != http.StatusOK {
				continue
			}
			var info struct {
				Version   string `json:"version"`
				ModelPath string `json:"model_path"`
			}
			if json.Unmarshal(body, &info) == nil && (info.Version != "" || info.ModelPath != "") {
				out.Engine = agentprotocol.EngineSGLang
				out.EngineVersion = boundedDetail(info.Version)
				break
			}
		}
	}
	// llama.cpp: /props carries the build.
	if out.Engine == agentprotocol.EngineLlamaCPP || out.Engine == agentprotocol.EngineOpenAICompatible {
		if status, body, _, err := d.get(ctx, out.BaseURL, "/props", auth); err == nil && status == http.StatusOK {
			var props struct {
				BuildInfo string `json:"build_info"`
				ModelPath string `json:"model_path"`
			}
			if json.Unmarshal(body, &props) == nil && (props.BuildInfo != "" || props.ModelPath != "") {
				out.Engine = agentprotocol.EngineLlamaCPP
				out.EngineVersion = boundedDetail(props.BuildInfo)
			}
		}
	}
	if !out.MetricsAvailable && out.MetricsDetail == "" {
		out.MetricsDetail = metricsUnavailable(out.Engine)
	}
}

func metricsUnavailable(engine string) string {
	switch engine {
	case agentprotocol.EngineOllama:
		return "Ollama does not expose engine metrics; queue, cache, and latency internals are unknown"
	case agentprotocol.EngineLMStudio:
		return "LM Studio does not expose engine metrics; queue, cache, and latency internals are unknown"
	case agentprotocol.EngineSGLang:
		return "SGLang serves metrics only when started with --enable-metrics"
	case agentprotocol.EngineLlamaCPP:
		return "llama.cpp serves metrics only when started with --metrics"
	case agentprotocol.EngineVLLM:
		return "GET /metrics did not return vLLM engine metrics"
	}
	return "no engine metrics were found at /metrics"
}

// SelectModel picks the model to observe: the one asked for, the model the
// server has loaded, or the only one it lists. It refuses to guess.
func SelectModel(server Detected, requested string) (string, error) {
	if requested != "" {
		for _, model := range server.Models {
			if model == requested {
				return model, nil
			}
		}
		return "", fmt.Errorf("%s does not list model %q", server.Address, requested)
	}
	if server.ActiveModel != "" {
		for _, model := range server.Models {
			if model == server.ActiveModel || strings.TrimSuffix(model, ":latest") == strings.TrimSuffix(server.ActiveModel, ":latest") {
				return model, nil
			}
		}
	}
	if len(server.Models) == 1 {
		return server.Models[0], nil
	}
	if len(server.Models) == 0 {
		return "", fmt.Errorf("%s lists no models", server.Address)
	}
	return "", fmt.Errorf("%s lists %d models and none is loaded; choose one with --model", server.Address, len(server.Models))
}

// EndpointName derives a valid endpoint name from a model identity, for
// example "Qwen/Qwen3-8B" becomes "qwen3-8b" and "llama3.2:3b" becomes
// "llama3.2-3b".
func EndpointName(model string) string {
	if index := strings.LastIndex(model, "/"); index >= 0 {
		model = model[index+1:]
	}
	model = strings.TrimSuffix(strings.ToLower(model), ":latest")
	var b strings.Builder
	for _, r := range model {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	name := strings.Trim(b.String(), "-._")
	for strings.Contains(name, "--") {
		name = strings.ReplaceAll(name, "--", "-")
	}
	if len(name) > 64 {
		name = strings.Trim(name[:64], "-._")
	}
	if name == "" {
		name = "local-model"
	}
	return name
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func boundedDetail(value string) string {
	value = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, strings.TrimSpace(value))
	if len(value) > 256 {
		value = value[:256]
	}
	return value
}
