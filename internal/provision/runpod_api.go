package provision

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// defaultRunPodRESTURL is RunPod's REST API v2 base. REST v1
// (https://rest.runpod.io/v1) was retired on 2026-11-15 and its host no
// longer resolves. INFERCRANE_RUNPOD_REST_URL overrides this base, for
// example to route through the acceptance fault proxy.
const defaultRunPodRESTURL = "https://api.runpod.io/v2"

// RunPod v2 list operations return at most 1000 items per page. The page cap
// bounds a misbehaving cursor; 100 pages is far above any account InferCrane
// operates and fails closed instead of looping.
const (
	runPodPageLimit = 1000
	runPodMaxPages  = 100
)

type runPodRequester func(ctx context.Context, method, path string, body, output any) error

type runPodPagination struct {
	NextCursor  *string `json:"nextCursor"`
	HasNextPage bool    `json:"hasNextPage"`
}

// runPodListAll walks a RunPod v2 cursor-paginated list. Every v2 list wraps
// its items under a resource key ("pods", "endpoints", "networkVolumes") next
// to a required pagination block. A response that omits either is rejected so
// an inventory read can never report a partial list as complete.
func runPodListAll[T any](ctx context.Context, request runPodRequester, path, key string) ([]T, error) {
	var items []T
	cursor := ""
	for page := 0; page < runPodMaxPages; page++ {
		query := url.Values{"limit": {fmt.Sprint(runPodPageLimit)}}
		if cursor != "" {
			query.Set("cursor", cursor)
		}
		var envelope map[string]json.RawMessage
		if err := request(ctx, http.MethodGet, path+"?"+query.Encode(), nil, &envelope); err != nil {
			return nil, err
		}
		rawItems, ok := envelope[key]
		if !ok {
			return nil, fmt.Errorf("RunPod list %s response omitted %q", path, key)
		}
		var pageItems []T
		if err := json.Unmarshal(rawItems, &pageItems); err != nil {
			return nil, fmt.Errorf("decode RunPod list %s: %w", path, err)
		}
		var pagination runPodPagination
		rawPagination, ok := envelope["pagination"]
		if !ok {
			return nil, fmt.Errorf("RunPod list %s response omitted pagination", path)
		}
		if err := json.Unmarshal(rawPagination, &pagination); err != nil {
			return nil, fmt.Errorf("decode RunPod list %s pagination: %w", path, err)
		}
		items = append(items, pageItems...)
		if !pagination.HasNextPage {
			return items, nil
		}
		if pagination.NextCursor == nil || strings.TrimSpace(*pagination.NextCursor) == "" || *pagination.NextCursor == cursor {
			return nil, fmt.Errorf("RunPod list %s reported more pages without a new cursor", path)
		}
		cursor = *pagination.NextCursor
	}
	return nil, fmt.Errorf("RunPod list %s exceeded %d pages", path, runPodMaxPages)
}

// runPodProblemText extracts the human-readable part of a RunPod v2 RFC 9457
// problem response for classification. Non-problem bodies are returned as-is.
func runPodProblemText(payload []byte) string {
	var problem struct {
		Title  string   `json:"title"`
		Detail string   `json:"detail"`
		Errors []string `json:"errors"`
	}
	if json.Unmarshal(payload, &problem) != nil || (problem.Detail == "" && problem.Title == "" && len(problem.Errors) == 0) {
		return string(payload)
	}
	return strings.Join(append([]string{problem.Title, problem.Detail}, problem.Errors...), " ")
}

func runPodBaseURL(configured string) string {
	base := strings.TrimRight(strings.TrimSpace(configured), "/")
	if base == "" {
		return defaultRunPodRESTURL
	}
	return base
}
