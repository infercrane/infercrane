// Package benchmark provides a reproducible OpenAI-compatible smoke/load runner.
package benchmark

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

type Config struct {
	Endpoint, APIKey, Model string
	Requests, Concurrency   int
	Timeout                 time.Duration
	Client                  *http.Client
}
type Result struct {
	Requests          int     `json:"requests"`
	Succeeded         int     `json:"succeeded"`
	Failed            int     `json:"failed"`
	DurationSeconds   float64 `json:"duration_seconds"`
	RequestsPerSecond float64 `json:"requests_per_second"`
	P50Milliseconds   float64 `json:"p50_milliseconds"`
	P95Milliseconds   float64 `json:"p95_milliseconds"`
}

func Run(ctx context.Context, cfg Config) (Result, error) {
	if cfg.Endpoint == "" || cfg.Model == "" || cfg.Requests < 1 || cfg.Concurrency < 1 {
		return Result{}, errors.New("endpoint, model, positive requests, and positive concurrency are required")
	}
	if cfg.Concurrency > cfg.Requests {
		cfg.Concurrency = cfg.Requests
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 2 * time.Minute
	}
	client := cfg.Client
	if client == nil {
		client = &http.Client{Timeout: cfg.Timeout}
	}
	jobs := make(chan struct{})
	latencies := make(chan float64, cfg.Requests)
	var succeeded, failed atomic.Int64
	var wg sync.WaitGroup
	started := time.Now()
	for range cfg.Concurrency {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range jobs {
				body, _ := json.Marshal(map[string]any{"model": cfg.Model, "messages": []map[string]string{{"role": "user", "content": "Reply with OK."}}, "max_tokens": 8})
				request, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.Endpoint+"/v1/chat/completions", bytes.NewReader(body))
				if err != nil {
					failed.Add(1)
					continue
				}
				request.Header.Set("Content-Type", "application/json")
				request.Header.Set("Authorization", "Bearer "+cfg.APIKey)
				before := time.Now()
				response, err := client.Do(request)
				latencies <- float64(time.Since(before)) / float64(time.Millisecond)
				if err != nil {
					failed.Add(1)
					continue
				}
				_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1<<20))
				response.Body.Close()
				if response.StatusCode >= 200 && response.StatusCode < 300 {
					succeeded.Add(1)
				} else {
					failed.Add(1)
				}
			}
		}()
	}
	go func() {
		defer close(jobs)
		for range cfg.Requests {
			select {
			case jobs <- struct{}{}:
			case <-ctx.Done():
				return
			}
		}
	}()
	wg.Wait()
	close(latencies)
	elapsed := time.Since(started)
	values := make([]float64, 0, cfg.Requests)
	for value := range latencies {
		values = append(values, value)
	}
	sort.Float64s(values)
	result := Result{Requests: cfg.Requests, Succeeded: int(succeeded.Load()), Failed: int(failed.Load()), DurationSeconds: elapsed.Seconds()}
	if elapsed > 0 {
		result.RequestsPerSecond = float64(result.Succeeded) / elapsed.Seconds()
	}
	if len(values) > 0 {
		result.P50Milliseconds = percentile(values, .50)
		result.P95Milliseconds = percentile(values, .95)
	}
	if ctx.Err() != nil {
		return result, ctx.Err()
	}
	return result, nil
}
func percentile(values []float64, p float64) float64 {
	index := int(float64(len(values)-1) * p)
	return values[index]
}
