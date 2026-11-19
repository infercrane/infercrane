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

// Target is one exact revision's directly addressed OpenAI-compatible server.
type Target struct {
	Endpoint, APIKey, Model, RevisionID string
}

// Completer returns one greedy answer for one prompt.
type Completer interface {
	Complete(context.Context, Target, EvalSet, Prompt) (string, error)
}

// HTTPCompleter calls POST /v1/chat/completions with greedy decoding. It never
// follows redirects or uses a proxy, and it bounds every response.
type HTTPCompleter struct {
	Client *http.Client
}

func (c HTTPCompleter) Complete(ctx context.Context, target Target, set EvalSet, prompt Prompt) (string, error) {
	url, err := openaicompat.Endpoint(target.Endpoint, "chat/completions")
	if err != nil {
		return "", err
	}
	messages := make([]map[string]string, 0, 2)
	if prompt.System != "" {
		messages = append(messages, map[string]string{"role": "system", "content": prompt.System})
	}
	messages = append(messages, map[string]string{"role": "user", "content": prompt.Prompt})
	body, err := json.Marshal(map[string]any{
		"model": target.Model, "messages": messages, "temperature": 0, "top_p": 1,
		"seed": Seed, "max_tokens": set.MaxTokens, "n": 1, "stream": false,
		// Hybrid reasoning models (Qwen3, GLM) answer directly when their chat
		// template supports this switch; templates without it ignore it. A
		// reasoning trace rarely finishes inside the check's token limit.
		"chat_template_kwargs": map[string]any{"enable_thinking": false},
	})
	if err != nil {
		return "", err
	}
	requestCtx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(requestCtx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	request.Header.Set("Content-Type", "application/json")
	if target.APIKey != "" {
		request.Header.Set("Authorization", "Bearer "+target.APIKey)
	}
	response, err := c.client().Do(request)
	if err != nil {
		return "", fmt.Errorf("prompt %s: %w", prompt.ID, err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		return "", fmt.Errorf("prompt %s: read answer: %w", prompt.ID, err)
	}
	if len(data) > maxResponseBytes {
		return "", fmt.Errorf("prompt %s: answer exceeds 1 MiB", prompt.ID)
	}
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("prompt %s: server returned HTTP %d", prompt.ID, response.StatusCode)
	}
	var decoded struct {
		Choices []struct {
			Message struct {
				Content *string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err = json.Unmarshal(data, &decoded); err != nil || len(decoded.Choices) == 0 {
		return "", fmt.Errorf("prompt %s: server returned no answer", prompt.ID)
	}
	if decoded.Choices[0].Message.Content == nil {
		// A server that splits out reasoning returns no content when the
		// reasoning did not finish inside the token limit. That is an answer
		// with no text, which Compare reports, not a transient failure.
		return "", nil
	}
	return strings.TrimSpace(*decoded.Choices[0].Message.Content), nil
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
