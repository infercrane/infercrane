package qualitycheck

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/infercrane/infercrane/internal/openaicompat"
)

const (
	// Seed is sent with every request. Greedy decoding already fixes the
	// output; the seed keeps servers that sample anyway reproducible.
	Seed             = 17
	maxResponseBytes = 1 << 20
	requestTimeout   = 3 * time.Minute
)

// Target is one directly addressed OpenAI-compatible server. SubjectID names
// exactly what answers: a deployment revision ID, or binding:<id> for a closed
// API behind an endpoint's external binding. Dialect says how it reads a
// chat request.
type Target struct {
	Endpoint, APIKey, Model, SubjectID string
	Dialect                            Dialect
	// Tokenizer names what the server reads text with: "model:ID@REVISION"
	// when it serves that exact model, or the sha256 of the model's
	// tokenizer files (modelcheck.TokenizerDigest). Empty is unknown. A
	// divergence compares token IDs, so it is read only between two targets
	// whose Tokenizer is set and equal.
	Tokenizer string
}

// Dialect is how a target reads a chat completion request.
type Dialect string

const (
	// DialectModelServer is a model server InferCrane runs (vLLM, SGLang,
	// llama.cpp): max_tokens, and chat_template_kwargs to turn hybrid
	// reasoning off.
	DialectModelServer Dialect = ""
	// DialectOpenAI is OpenAI's API: max_completion_tokens, and no
	// chat_template_kwargs, which it rejects.
	DialectOpenAI Dialect = "openai"
	// DialectOpenAICompatible is another closed OpenAI-compatible API:
	// max_tokens, and no chat_template_kwargs.
	DialectOpenAICompatible Dialect = "openai-compatible"
)

// BindingSubject is the SubjectID of a closed API behind an external binding.
func BindingSubject(bindingID string) string { return "binding:" + bindingID }

// Completer returns one greedy answer for one prompt: its text and any tool
// calls.
type Completer interface {
	Complete(context.Context, Target, EvalSet, Prompt) (Answer, error)
}

// HTTPCompleter calls POST /v1/chat/completions with greedy decoding. It never
// follows redirects or uses a proxy, and it bounds every response.
type HTTPCompleter struct {
	Client *http.Client
}

func (c HTTPCompleter) Complete(ctx context.Context, target Target, set EvalSet, prompt Prompt) (Answer, error) {
	url, err := openaicompat.Endpoint(target.Endpoint, "chat/completions")
	if err != nil {
		return Answer{}, err
	}
	body, err := json.Marshal(requestBody(target, set, prompt))
	if err != nil {
		return Answer{}, err
	}
	requestCtx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	httpRequest, err := http.NewRequestWithContext(requestCtx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return Answer{}, err
	}
	httpRequest.Header.Set("Content-Type", "application/json")
	if target.APIKey != "" {
		httpRequest.Header.Set("Authorization", "Bearer "+target.APIKey)
	}
	response, err := c.client().Do(httpRequest)
	if err != nil {
		return Answer{}, fmt.Errorf("prompt %s: %w", prompt.ID, err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		return Answer{}, fmt.Errorf("prompt %s: read answer: %w", prompt.ID, err)
	}
	if len(data) > maxResponseBytes {
		return Answer{}, fmt.Errorf("prompt %s: answer exceeds 1 MiB", prompt.ID)
	}
	if response.StatusCode == http.StatusBadRequest && prompt.HasTools() {
		// vLLM refuses tools unless it was started with
		// --enable-auto-tool-choice and a --tool-call-parser.
		return Answer{}, fmt.Errorf("prompt %s: server returned HTTP 400 to a request with tools; the server may not accept tool calls", prompt.ID)
	}
	if response.StatusCode != http.StatusOK {
		return Answer{}, fmt.Errorf("prompt %s: server returned HTTP %d", prompt.ID, response.StatusCode)
	}
	var decoded struct {
		Choices []struct {
			Message struct {
				Content   *string `json:"content"`
				ToolCalls []struct {
					ID       string `json:"id"`
					Type     string `json:"type"`
					Function struct {
						Name      string          `json:"name"`
						Arguments json.RawMessage `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err = json.Unmarshal(data, &decoded); err != nil || len(decoded.Choices) == 0 {
		return Answer{}, fmt.Errorf("prompt %s: server returned no answer", prompt.ID)
	}
	message := decoded.Choices[0].Message
	var answer Answer
	// A server that splits out reasoning returns no content when the
	// reasoning did not finish inside the token limit. That is an answer with
	// no text, which Compare reports, not a transient failure.
	if message.Content != nil {
		answer.Content = strings.TrimSpace(*message.Content)
	}
	for _, made := range message.ToolCalls {
		answer.ToolCalls = append(answer.ToolCalls, ToolCall{ID: made.ID, Type: made.Type, Function: ToolCallFunction{Name: made.Function.Name, Arguments: argumentText(made.Function.Arguments)}})
	}
	return answer, nil
}

// argumentText returns a call's arguments as the JSON text the model wrote.
// The OpenAI format sends them as a string; a server that sends a JSON value
// instead is read as that value's text.
func argumentText(raw json.RawMessage) string {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	return string(raw)
}

func requestBody(target Target, set EvalSet, prompt Prompt) map[string]any {
	body := map[string]any{
		"model": target.Model, "messages": prompt.requestMessages(), "temperature": 0, "top_p": 1,
		"seed": Seed, "n": 1, "stream": false,
	}
	if prompt.HasTools() {
		body["tools"] = prompt.Tools
		if prompt.ToolChoice != nil {
			body["tool_choice"] = prompt.ToolChoice
		}
	}
	switch target.Dialect {
	case DialectOpenAI:
		body["max_completion_tokens"] = set.MaxTokens
	case DialectOpenAICompatible:
		body["max_tokens"] = set.MaxTokens
	default:
		body["max_tokens"] = set.MaxTokens
		// Hybrid reasoning models (Qwen3, GLM) answer directly when their chat
		// template supports this switch; templates without it ignore it. A
		// reasoning trace rarely finishes inside the check's token limit.
		body["chat_template_kwargs"] = map[string]any{"enable_thinking": false}
	}
	return body
}

func (c HTTPCompleter) client() *http.Client {
	if c.Client != nil {
		return c.Client
	}
	return defaultClient
}

var defaultClient = &http.Client{
	Transport: &http.Transport{
		Proxy:                 nil,
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: requestTimeout,
		MaxIdleConnsPerHost:   2,
	},
	CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("redirects are not followed") },
}
