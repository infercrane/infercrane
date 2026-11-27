package specswitch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Controller puts an engine to sleep, wakes it, and checks that it answers.
type Controller interface {
	Sleep(ctx context.Context, engine Engine) error
	Wake(ctx context.Context, engine Engine) error
	// Ready returns nil once the engine serves its model: it lists the model
	// and writes one token.
	Ready(ctx context.Context, engine Engine) error
}

// VLLM controls vLLM engines through their sleep-mode routes. The engine must
// run with --enable-sleep-mode and VLLM_SERVER_DEV_MODE=1, which serves
// /sleep, /wake_up and /is_sleeping (vLLM 0.34.0, Python and Rust front ends).
// Level 1 sleep moves the weights to host memory and discards the cache.
type VLLM struct {
	Client *http.Client
	// Poll is how often a sleep or wake is checked; zero means 200 ms.
	Poll time.Duration
}

// maxEngineReply bounds what the switch reads from an engine's control routes.
const maxEngineReply = 1 << 20

func (v VLLM) client() *http.Client {
	if v.Client != nil {
		return v.Client
	}
	return &http.Client{Timeout: 5 * time.Minute}
}

func (v VLLM) poll() time.Duration {
	if v.Poll > 0 {
		return v.Poll
	}
	return 200 * time.Millisecond
}

// Sleep puts the engine to level-1 sleep. mode=wait lets requests the engine
// still holds finish instead of aborting them; the switch drains first, so
// none should remain.
func (v VLLM) Sleep(ctx context.Context, engine Engine) error {
	if _, err := v.call(ctx, http.MethodPost, engine.URL+"/sleep?level=1&mode=wait", nil); err != nil {
		return fmt.Errorf("sleep %s: %w", engine.Name, err)
	}
	return v.waitSleeping(ctx, engine, true)
}

// Wake wakes every part of the engine: weights and cache.
func (v VLLM) Wake(ctx context.Context, engine Engine) error {
	_, _, err := v.WakeInParts(ctx, engine)
	return err
}

// WakeInParts wakes the engine in vLLM's two deferrable parts and times each:
// first the weights (with them, every part that is not deferrable: runtime
// buffers and the CUDA graphs' memory, copied back from host memory; the
// graphs are not captured again), then the KV cache (mapped afresh, nothing
// copied).
func (v VLLM) WakeInParts(ctx context.Context, engine Engine) (weights, cache time.Duration, err error) {
	start := time.Now()
	if _, err = v.call(ctx, http.MethodPost, engine.URL+"/wake_up?tags=weights", nil); err != nil {
		return 0, 0, fmt.Errorf("wake %s weights: %w", engine.Name, err)
	}
	weights = time.Since(start)
	start = time.Now()
	if _, err = v.call(ctx, http.MethodPost, engine.URL+"/wake_up?tags=kv_cache", nil); err != nil {
		return weights, 0, fmt.Errorf("wake %s cache: %w", engine.Name, err)
	}
	err = v.waitSleeping(ctx, engine, false)
	return weights, time.Since(start), err
}

func (v VLLM) waitSleeping(ctx context.Context, engine Engine, want bool) error {
	for {
		body, err := v.call(ctx, http.MethodGet, engine.URL+"/is_sleeping", nil)
		if err == nil {
			var state struct {
				IsSleeping *bool `json:"is_sleeping"`
			}
			if json.Unmarshal(body, &state) == nil && state.IsSleeping != nil && *state.IsSleeping == want {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%s did not report is_sleeping=%t: %w", engine.Name, want, ctx.Err())
		case <-time.After(v.poll()):
		}
	}
}

// Ready lists the engine's model and asks it for one token.
func (v VLLM) Ready(ctx context.Context, engine Engine) error {
	body, err := v.call(ctx, http.MethodGet, engine.URL+"/v1/models", nil)
	if err != nil {
		return fmt.Errorf("models %s: %w", engine.Name, err)
	}
	var models struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err = json.Unmarshal(body, &models); err != nil || len(models.Data) == 0 || models.Data[0].ID == "" {
		return fmt.Errorf("models %s: no model listed", engine.Name)
	}
	request, _ := json.Marshal(map[string]any{"model": models.Data[0].ID, "prompt": "Hello", "max_tokens": 1, "temperature": 0})
	if _, err = v.call(ctx, http.MethodPost, engine.URL+"/v1/completions", request); err != nil {
		return fmt.Errorf("one token from %s: %w", engine.Name, err)
	}
	return nil
}

func (v VLLM) call(ctx context.Context, method, target string, body []byte) ([]byte, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	request, err := http.NewRequestWithContext(ctx, method, target, reader)
	if err != nil {
		return nil, err
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := v.client().Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	reply, err := io.ReadAll(io.LimitReader(response.Body, maxEngineReply))
	if err != nil {
		return nil, err
	}
	if response.StatusCode/100 != 2 {
		return nil, fmt.Errorf("status %d: %s", response.StatusCode, bytes.TrimSpace(reply[:min(len(reply), 300)]))
	}
	return reply, nil
}

// WaitHealthy polls the engine's /health until it answers 200 or ctx ends.
func (v VLLM) WaitHealthy(ctx context.Context, engine Engine) error {
	for {
		if _, err := v.call(ctx, http.MethodGet, engine.URL+"/health", nil); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return errors.Join(fmt.Errorf("%s never answered /health", engine.Name), ctx.Err())
		case <-time.After(v.poll() * 5):
		}
	}
}
