package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/infercrane/infercrane/internal/openrouterprovider"
)

type gate struct {
	Name   string `json:"name"`
	Passed bool   `json:"passed"`
	Detail string `json:"detail,omitempty"`
}

type streamResult struct {
	Status           int     `json:"status"`
	TTFTMilliseconds float64 `json:"ttft_ms"`
	SawDone          bool    `json:"saw_done"`
	SawFinishReason  bool    `json:"saw_finish_reason"`
	PromptTokens     int     `json:"prompt_tokens"`
	CompletionTokens int     `json:"completion_tokens"`
}

type boundaryResult struct {
	TargetPromptTokens     int          `json:"target_prompt_tokens"`
	TargetCompletionTokens int          `json:"target_completion_tokens"`
	ContentRepetitions     int          `json:"content_repetitions"`
	Stream                 streamResult `json:"stream"`
}

type receipt struct {
	SchemaVersion string          `json:"schema_version"`
	CreatedAt     time.Time       `json:"created_at"`
	Endpoint      string          `json:"endpoint"`
	Model         string          `json:"model"`
	Gates         []gate          `json:"gates"`
	Stream        streamResult    `json:"stream"`
	Boundary      *boundaryResult `json:"boundary,omitempty"`
	Load          map[string]any  `json:"load"`
	Passed        bool            `json:"passed"`
}

type qualifier struct {
	baseURL string
	apiKey  string
	model   string
	client  *http.Client
}

func main() {
	baseURL := flag.String("url", "", "HTTPS base URL for the InferCrane provider gateway")
	model := flag.String("model", "qwen/qwen3.8-27b", "OpenRouter model id")
	keyFile := flag.String("api-key-file", "", "path containing the provider API key")
	requests := flag.Int("requests", 32, "number of bounded load requests")
	concurrency := flag.Int("concurrency", 8, "bounded load concurrency")
	output := flag.String("output", "", "optional JSON receipt path")
	boundaryInput := flag.Int("boundary-input-tokens", 0, "optional exact prompt-token boundary to qualify")
	boundaryOutput := flag.Int("boundary-output-tokens", 0, "optional exact completion-token boundary to qualify")
	flag.Parse()
	if *baseURL == "" || *keyFile == "" || *requests < 1 || *concurrency < 1 {
		fatal(errors.New("--url, --api-key-file, positive --requests, and positive --concurrency are required"))
	}
	if (*boundaryInput == 0) != (*boundaryOutput == 0) || *boundaryInput < 0 || *boundaryOutput < 0 {
		fatal(errors.New("--boundary-input-tokens and --boundary-output-tokens must both be positive or both omitted"))
	}
	parsed, err := url.Parse(*baseURL)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		fatal(errors.New("--url must be an absolute HTTPS URL without credentials, query, or fragment"))
	}
	key, err := os.ReadFile(*keyFile)
	if err != nil || strings.TrimSpace(string(key)) == "" {
		fatal(fmt.Errorf("read provider API key: %w", err))
	}
	q := qualifier{
		baseURL: strings.TrimRight(*baseURL, "/"), apiKey: strings.TrimSpace(string(key)), model: *model,
		client: &http.Client{Timeout: 5 * time.Minute, Transport: &http.Transport{MaxIdleConns: 128, MaxIdleConnsPerHost: 64, IdleConnTimeout: 90 * time.Second}},
	}
	result, err := q.run(context.Background(), *requests, *concurrency, *boundaryInput, *boundaryOutput)
	if err != nil {
		fatal(err)
	}
	body, _ := json.MarshalIndent(result, "", "  ")
	body = append(body, '\n')
	if *output != "" {
		if err = os.MkdirAll(filepath.Dir(*output), 0o755); err != nil {
			fatal(err)
		}
		if err = os.WriteFile(*output, body, 0o600); err != nil {
			fatal(err)
		}
	}
	_, _ = os.Stdout.Write(body)
	if !result.Passed {
		os.Exit(1)
	}
}

func (q qualifier) run(ctx context.Context, requests, concurrency, boundaryInput, boundaryOutput int) (receipt, error) {
	result := receipt{SchemaVersion: "infercrane.dev/openrouter-provider-qualification/v1", CreatedAt: time.Now().UTC(), Endpoint: q.baseURL, Model: q.model}
	catalogGate := q.catalogGate(ctx)
	result.Gates = append(result.Gates, catalogGate)

	buffered, status, err := q.postJSON(ctx, map[string]any{"model": q.model, "messages": []map[string]string{{"role": "user", "content": "Reply with ready."}}, "temperature": 0, "max_tokens": 16})
	bufferedPassed := err == nil && status == http.StatusOK && positiveUsage(buffered) && finishReason(buffered) != ""
	result.Gates = append(result.Gates, gate{Name: "buffered_usage_and_finish", Passed: bufferedPassed, Detail: statusDetail(status, err)})

	stream, err := q.stream(ctx)
	result.Stream = stream
	result.Gates = append(result.Gates,
		gate{Name: "stream_done", Passed: err == nil && stream.Status == http.StatusOK && stream.SawDone, Detail: statusDetail(stream.Status, err)},
		gate{Name: "stream_finish_reason", Passed: err == nil && stream.SawFinishReason},
		gate{Name: "stream_usage", Passed: err == nil && stream.PromptTokens > 0 && stream.CompletionTokens > 0},
	)

	structured, status, err := q.postJSON(ctx, map[string]any{
		"model": q.model, "messages": []map[string]string{{"role": "user", "content": "Return status ready as JSON."}}, "temperature": 0, "max_tokens": 64,
		"response_format": map[string]any{"type": "json_schema", "json_schema": map[string]any{"name": "status", "strict": true, "schema": map[string]any{"type": "object", "additionalProperties": false, "required": []string{"status"}, "properties": map[string]any{"status": map[string]any{"const": "ready"}}}}},
	})
	result.Gates = append(result.Gates, gate{Name: "structured_output", Passed: status == http.StatusOK && structuredReady(structured), Detail: statusDetail(status, err)})

	tool, status, err := q.postJSON(ctx, map[string]any{
		"model": q.model, "messages": []map[string]string{{"role": "user", "content": "Read src/cache.py."}}, "temperature": 0, "max_tokens": 96,
		"tools":       []map[string]any{{"type": "function", "function": map[string]any{"name": "read_file", "description": "Read a file", "parameters": map[string]any{"type": "object", "properties": map[string]any{"path": map[string]string{"type": "string"}}, "required": []string{"path"}}}}},
		"tool_choice": map[string]any{"type": "function", "function": map[string]string{"name": "read_file"}},
	})
	result.Gates = append(result.Gates, gate{Name: "forced_tool_call", Passed: status == http.StatusOK && expectedToolCall(tool), Detail: statusDetail(status, err)})

	reasoning, status, err := q.postJSON(ctx, map[string]any{
		"model": q.model, "messages": []map[string]string{{"role": "user", "content": "Think through 17 plus 25, then give the integer."}}, "temperature": 0, "max_tokens": 256,
		"reasoning": map[string]any{"effort": "xhigh"}, "include_reasoning": true,
	})
	result.Gates = append(result.Gates, gate{Name: "reasoning_output", Passed: status == http.StatusOK && reasoningReady(reasoning), Detail: statusDetail(status, err)})

	_, status, err = q.postJSON(ctx, map[string]any{"model": "infercrane/unknown-model", "messages": []map[string]string{{"role": "user", "content": "hello"}}, "max_tokens": 1})
	result.Gates = append(result.Gates, gate{Name: "unknown_model_rejected", Passed: err == nil && status >= 400 && status < 500, Detail: statusDetail(status, err)})
	status, err = q.postRaw(ctx, []byte("{not-json"))
	result.Gates = append(result.Gates, gate{Name: "malformed_json_rejected", Passed: err == nil && status >= 400 && status < 500, Detail: statusDetail(status, err)})

	load := q.load(ctx, requests, concurrency)
	result.Load = load
	serverErrors, _ := load["server_errors"].(int)
	other, _ := load["other_statuses"].(int)
	result.Gates = append(result.Gates, gate{Name: "bounded_load_no_server_errors", Passed: serverErrors == 0 && other == 0})

	if boundaryInput > 0 {
		boundary, err := q.boundary(ctx, boundaryInput, boundaryOutput)
		result.Boundary = &boundary
		passed := err == nil && boundary.Stream.Status == http.StatusOK && boundary.Stream.SawDone && boundary.Stream.SawFinishReason && boundary.Stream.PromptTokens == boundaryInput && boundary.Stream.CompletionTokens == boundaryOutput
		result.Gates = append(result.Gates, gate{Name: "advertised_token_boundary", Passed: passed, Detail: statusDetail(boundary.Stream.Status, err)})
	}

	recovery, status, err := q.postJSON(ctx, map[string]any{"model": q.model, "messages": []map[string]string{{"role": "user", "content": "Reply recovered."}}, "temperature": 0, "max_tokens": 16})
	result.Gates = append(result.Gates, gate{Name: "post_load_recovery", Passed: err == nil && status == http.StatusOK && positiveUsage(recovery), Detail: statusDetail(status, err)})
	result.Passed = true
	for _, check := range result.Gates {
		result.Passed = result.Passed && check.Passed
	}
	return result, nil
}

func (q qualifier) catalogGate(ctx context.Context) gate {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, q.baseURL+"/openrouter/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+q.apiKey)
	response, err := q.client.Do(req)
	if err != nil {
		return gate{Name: "provider_models_schema_2_5", Detail: err.Error()}
	}
	defer response.Body.Close()
	var catalog openrouterprovider.Catalog
	err = json.NewDecoder(io.LimitReader(response.Body, 4<<20)).Decode(&catalog)
	if err == nil {
		err = catalog.Validate()
	}
	found := false
	for _, model := range catalog.Data {
		found = found || model.ID == q.model
	}
	return gate{Name: "provider_models_schema_2_5", Passed: response.StatusCode == http.StatusOK && err == nil && found, Detail: statusDetail(response.StatusCode, err)}
}

func (q qualifier) postJSON(ctx context.Context, payload map[string]any) (map[string]any, int, error) {
	body, _ := json.Marshal(payload)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, q.baseURL+"/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+q.apiKey)
	req.Header.Set("Content-Type", "application/json")
	response, err := q.client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer response.Body.Close()
	var value map[string]any
	err = json.NewDecoder(io.LimitReader(response.Body, 8<<20)).Decode(&value)
	return value, response.StatusCode, err
}

func (q qualifier) postRaw(ctx context.Context, body []byte) (int, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, q.baseURL+"/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+q.apiKey)
	req.Header.Set("Content-Type", "application/json")
	response, err := q.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1<<20))
	return response.StatusCode, nil
}

func (q qualifier) stream(ctx context.Context) (streamResult, error) {
	return q.streamPrompt(ctx, "Reply with ready.", 16)
}

func (q qualifier) streamPrompt(ctx context.Context, content string, maxTokens int) (streamResult, error) {
	return q.streamPayload(ctx, map[string]any{"model": q.model, "messages": []map[string]string{{"role": "user", "content": content}}, "temperature": 0, "max_tokens": maxTokens, "stream": true, "stream_options": map[string]bool{"include_usage": true}})
}

func (q qualifier) streamPayload(ctx context.Context, value map[string]any) (streamResult, error) {
	payload, _ := json.Marshal(value)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, q.baseURL+"/v1/chat/completions", bytes.NewReader(payload))
	req.Header.Set("Authorization", "Bearer "+q.apiKey)
	req.Header.Set("Content-Type", "application/json")
	started := time.Now()
	response, err := q.client.Do(req)
	if err != nil {
		return streamResult{}, err
	}
	defer response.Body.Close()
	result := streamResult{Status: response.StatusCode}
	if response.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, response.Body)
		return result, nil
	}
	firstContent := time.Time{}
	scanner := bufio.NewScanner(response.Body)
	scanner.Buffer(make([]byte, 64<<10), 2<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		raw := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if raw == "[DONE]" {
			result.SawDone = true
			continue
		}
		if raw == "" {
			continue
		}
		var event map[string]any
		if json.Unmarshal([]byte(raw), &event) != nil {
			continue
		}
		if usage, ok := event["usage"].(map[string]any); ok {
			result.PromptTokens = integer(usage["prompt_tokens"])
			result.CompletionTokens = integer(usage["completion_tokens"])
		}
		for _, choice := range slice(event["choices"]) {
			if choice["finish_reason"] != nil {
				result.SawFinishReason = true
			}
			if delta, ok := choice["delta"].(map[string]any); ok && deltaHasOutput(delta) && firstContent.IsZero() {
				firstContent = time.Now()
			}
		}
	}
	if !firstContent.IsZero() {
		result.TTFTMilliseconds = float64(firstContent.Sub(started).Microseconds()) / 1000
	}
	return result, scanner.Err()
}

func (q qualifier) boundary(ctx context.Context, promptTokens, completionTokens int) (boundaryResult, error) {
	// Qwen tokenizes the repeated leading-space atom as one token. Calibrate the
	// fixed chat-template overhead against the deployed tokenizer so the final
	// request exercises the exact advertised input boundary rather than an
	// approximation based on bytes.
	repetitions := promptTokens - 64
	if repetitions < 1 {
		repetitions = 1
	}
	calibration, status, err := q.postJSON(ctx, map[string]any{
		"model": q.model, "messages": []map[string]string{{"role": "user", "content": strings.Repeat(" x", repetitions)}},
		"temperature": 0, "max_tokens": 1,
	})
	if err != nil || status != http.StatusOK {
		return boundaryResult{TargetPromptTokens: promptTokens, TargetCompletionTokens: completionTokens, ContentRepetitions: repetitions, Stream: streamResult{Status: status}}, err
	}
	usage, _ := calibration["usage"].(map[string]any)
	repetitions += promptTokens - integer(usage["prompt_tokens"])
	if repetitions < 1 {
		return boundaryResult{}, errors.New("tokenizer calibration produced an invalid repetition count")
	}
	stream, err := q.streamPayload(ctx, map[string]any{
		"model": q.model, "messages": []map[string]string{{"role": "user", "content": strings.Repeat(" x", repetitions)}},
		"temperature": 0, "max_tokens": completionTokens, "ignore_eos": true,
		"stream": true, "stream_options": map[string]bool{"include_usage": true},
	})
	return boundaryResult{TargetPromptTokens: promptTokens, TargetCompletionTokens: completionTokens, ContentRepetitions: repetitions, Stream: stream}, err
}

func deltaHasOutput(delta map[string]any) bool {
	for _, key := range []string{"content", "reasoning", "reasoning_content"} {
		if value, exists := delta[key]; exists && value != nil && strings.TrimSpace(fmt.Sprint(value)) != "" {
			return true
		}
	}
	return len(slice(delta["tool_calls"])) > 0
}

func (q qualifier) load(ctx context.Context, requests, concurrency int) map[string]any {
	semaphore := make(chan struct{}, concurrency)
	statuses := make(chan int, requests)
	latencies := make(chan float64, requests)
	var wait sync.WaitGroup
	for index := 0; index < requests; index++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			semaphore <- struct{}{}
			defer func() { <-semaphore }()
			started := time.Now()
			result, err := q.streamPrompt(ctx, fmt.Sprintf("Reply with request %d.", index), 32)
			status := result.Status
			if err != nil {
				status = 0
			}
			statuses <- status
			latencies <- float64(time.Since(started).Microseconds()) / 1000
		}(index)
	}
	wait.Wait()
	close(statuses)
	close(latencies)
	counts := map[int]int{}
	for status := range statuses {
		counts[status]++
	}
	values := make([]float64, 0, requests)
	for latency := range latencies {
		values = append(values, latency)
	}
	sort.Float64s(values)
	serverErrors, other := 0, 0
	for status, count := range counts {
		if status >= 500 || status == 0 {
			serverErrors += count
		} else if status != http.StatusOK && status != http.StatusTooManyRequests {
			other += count
		}
	}
	return map[string]any{"requests": requests, "concurrency": concurrency, "status_counts": counts, "server_errors": serverErrors, "other_statuses": other, "latency_p50_ms": percentile(values, 0.50), "latency_p95_ms": percentile(values, 0.95)}
}

func positiveUsage(value map[string]any) bool {
	usage, _ := value["usage"].(map[string]any)
	return integer(usage["prompt_tokens"]) > 0 && integer(usage["completion_tokens"]) > 0
}

func finishReason(value map[string]any) string {
	choices := slice(value["choices"])
	if len(choices) == 0 {
		return ""
	}
	finish, ok := choices[0]["finish_reason"]
	if !ok || finish == nil {
		return ""
	}
	return fmt.Sprint(finish)
}

func structuredReady(value map[string]any) bool {
	choices := slice(value["choices"])
	if len(choices) == 0 {
		return false
	}
	message, _ := choices[0]["message"].(map[string]any)
	var decoded map[string]any
	return json.Unmarshal([]byte(fmt.Sprint(message["content"])), &decoded) == nil && decoded["status"] == "ready"
}

func expectedToolCall(value map[string]any) bool {
	choices := slice(value["choices"])
	if len(choices) == 0 {
		return false
	}
	message, _ := choices[0]["message"].(map[string]any)
	calls := slice(message["tool_calls"])
	if len(calls) == 0 {
		return false
	}
	function, _ := calls[0]["function"].(map[string]any)
	var arguments map[string]any
	return function["name"] == "read_file" && json.Unmarshal([]byte(fmt.Sprint(function["arguments"])), &arguments) == nil && arguments["path"] == "src/cache.py"
}

func reasoningReady(value map[string]any) bool {
	choices := slice(value["choices"])
	if len(choices) == 0 {
		return false
	}
	message, _ := choices[0]["message"].(map[string]any)
	reasoning := strings.TrimSpace(fmt.Sprint(message["reasoning_content"]))
	if reasoning == "" || reasoning == "<nil>" {
		reasoning = strings.TrimSpace(fmt.Sprint(message["reasoning"]))
	}
	content := strings.TrimSpace(fmt.Sprint(message["content"]))
	answer := regexp.MustCompile(`(^|\D)42(\D|$)`)
	return reasoning != "" && reasoning != "<nil>" && answer.MatchString(content)
}

func slice(value any) []map[string]any {
	raw, _ := value.([]any)
	rows := make([]map[string]any, 0, len(raw))
	for _, entry := range raw {
		if row, ok := entry.(map[string]any); ok {
			rows = append(rows, row)
		}
	}
	return rows
}

func integer(value any) int {
	number, _ := value.(float64)
	return int(number)
}

func percentile(values []float64, quantile float64) float64 {
	if len(values) == 0 {
		return 0
	}
	position := int(float64(len(values)-1) * quantile)
	return values[position]
}

func statusDetail(status int, err error) string {
	if err != nil {
		return err.Error()
	}
	return fmt.Sprintf("HTTP %d", status)
}

func fatal(err error) {
	_, _ = fmt.Fprintln(os.Stderr, "openrouter-provider-qualifier:", err)
	os.Exit(1)
}
