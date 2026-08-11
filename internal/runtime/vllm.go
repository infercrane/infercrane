package runtime

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"
)

// OpenAI inspects the narrow HTTP readiness and model-identity contract shared
// by qualified runtimes. The VLLM alias preserves source compatibility.
type OpenAI struct {
	Client *http.Client
	APIKey string
}

type VLLM = OpenAI

var defaultClient = &http.Client{Transport: &http.Transport{MaxIdleConns: 256, MaxIdleConnsPerHost: 32, IdleConnTimeout: 90 * time.Second}}

func (v OpenAI) Inspect(ctx context.Context, baseURL string) (bool, map[string]struct{}) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	client := v.Client
	if client == nil {
		client = defaultClient
	}
	do := func(path string) (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(baseURL, "/")+path, nil)
		if err != nil {
			return nil, err
		}
		if v.APIKey != "" {
			req.Header.Set("Authorization", "Bearer "+v.APIKey)
		}
		return client.Do(req)
	}
	health, err := do("/health")
	if err != nil {
		return false, nil
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(health.Body, 4096))
	health.Body.Close()
	if health.StatusCode != http.StatusOK {
		return false, nil
	}
	models, err := do("/v1/models")
	if err != nil {
		return false, nil
	}
	defer models.Body.Close()
	if models.StatusCode != http.StatusOK {
		return false, nil
	}
	var body struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if json.NewDecoder(io.LimitReader(models.Body, 1<<20)).Decode(&body) != nil {
		return false, nil
	}
	ids := make(map[string]struct{}, len(body.Data))
	for _, item := range body.Data {
		if item.ID != "" {
			ids[item.ID] = struct{}{}
		}
	}
	return true, ids
}
