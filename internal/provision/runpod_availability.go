package provision

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/infercrane/infercrane/internal/provideridentity"
)

const defaultRunPodGraphQLURL = "https://api.runpod.io/graphql"

// RunPodAvailability is an optional, read-only provider capability. Keeping it
// separate from SkyPilot lets other providers register their own signal without
// changing durable lifecycle code.
type RunPodAvailability struct {
	APIKey, BaseURL string
	Client          *http.Client
}

func (r RunPodAvailability) ProbeLaunch(ctx context.Context, request LaunchProbeRequest) (LaunchProbeEvidence, error) {
	now := time.Now().UTC()
	evidence := LaunchProbeEvidence{
		Provider: request.Provider, Region: request.Region, GPU: request.GPU, GPUCount: request.GPUCount,
		ConnectionState: "connection-required", AvailabilityState: "unknown", QuotaState: "unknown", Deployability: "unknown",
		Source: "runpod.gpuTypes.lowestPrice", ObservedAt: now, ExpiresAt: now.Add(30 * time.Second),
		Message:     "RunPod credentials are not configured",
		Limitations: []string{"provider stock is advisory and not a reservation", "quota is not exposed by this read-only signal"},
	}
	if evidence.Provider == "" {
		evidence.Provider = "runpod"
	}
	if strings.TrimSpace(r.APIKey) == "" {
		return evidence, nil
	}
	evidence.ConnectionState = "configured"
	availability, err := r.Availability(ctx, AvailabilityRequest{Cloud: evidence.Provider, Region: request.Region, GPU: request.GPU, Count: request.GPUCount})
	if err != nil {
		evidence.Message = "RunPod stock could not be observed; launchability remains unknown"
		return evidence, err
	}
	evidence.AvailabilityState = availability.State
	evidence.Message = availability.Message
	switch availability.State {
	case "unavailable":
		evidence.Deployability = "unavailable"
	default:
		// Positive stock is advisory. RunPod does not expose account quota in
		// this signal, so deployability must remain unknown until a launch is
		// accepted. A proven absence of stock can still fail closed above.
		evidence.Deployability = "unknown"
	}
	return evidence, nil
}

func (r RunPodAvailability) Availability(ctx context.Context, request AvailabilityRequest) (Availability, error) {
	if strings.TrimSpace(r.APIKey) == "" {
		return Availability{State: "unknown", Message: "Provider availability was not checked because credentials are not configured"}, nil
	}
	if request.Count < 1 {
		request.Count = 1
	}
	queryText := fmt.Sprintf("query { gpuTypes { id displayName lowestPrice(input: {gpuCount: %d, secureCloud: true}) { stockStatus availableGpuCounts } } }", request.Count)
	encoded, _ := json.Marshal(map[string]string{"query": queryText})
	endpoint := strings.TrimRight(r.BaseURL, "/")
	if endpoint == "" {
		endpoint = defaultRunPodGraphQLURL
	}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(encoded))
	if err != nil {
		return Availability{}, fmt.Errorf("create provider availability request: %w", err)
	}
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set("Authorization", "Bearer "+r.APIKey)
	client := r.Client
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	response, err := client.Do(httpRequest)
	if err != nil {
		// Do not persist provider-specific transport internals in durable events.
		return Availability{}, errors.New("provider availability request failed")
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return Availability{}, fmt.Errorf("read provider availability: %w", err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return Availability{}, fmt.Errorf("provider availability returned HTTP %d", response.StatusCode)
	}
	var payload struct {
		Data struct {
			GPUTypes []struct {
				ID, DisplayName string
				LowestPrice     *struct {
					StockStatus        string
					AvailableGPUCounts []int
				}
			}
		}
		Errors []struct{ Message string }
	}
	if err = json.Unmarshal(data, &payload); err != nil {
		return Availability{}, fmt.Errorf("decode provider availability: %w", err)
	}
	if len(payload.Errors) > 0 {
		return Availability{}, errors.New("provider availability query was rejected: " + safeRunPodDiagnostic(payload.Errors[0].Message, r.APIKey))
	}
	matches := make([]string, 0)
	best := "none"
	for _, gpu := range payload.Data.GPUTypes {
		if !provideridentity.MatchesGPU("runpod", gpu.ID, request.GPU) {
			continue
		}
		stock := "None"
		if gpu.LowestPrice != nil && gpu.LowestPrice.StockStatus != "" {
			stock = gpu.LowestPrice.StockStatus
		}
		label := gpu.DisplayName
		if strings.TrimSpace(label) == "" {
			label = gpu.ID
		}
		matches = append(matches, label+":"+stock)
		best = betterStock(best, strings.ToLower(stock))
	}
	if len(matches) == 0 {
		return Availability{State: "unknown", Message: fmt.Sprintf("Provider did not report a stock signal for GPU %s", request.GPU)}, nil
	}
	state, message := "available", fmt.Sprintf("Provider reports %s stock for %s", best, request.GPU)
	switch best {
	case "none":
		state, message = "unavailable", fmt.Sprintf("Provider reports no current secure capacity for %s", request.GPU)
	case "low":
		state, message = "constrained", fmt.Sprintf("Provider reports low current secure capacity for %s; placement may be delayed", request.GPU)
	}
	if request.Region != "" {
		message += "; the signal is global and does not guarantee region " + request.Region
	}
	return Availability{State: state, Message: message, Details: safeRunPodDiagnostic(strings.Join(matches, ","), r.APIKey)}, nil
}

func betterStock(current, candidate string) string {
	rank := map[string]int{"none": 0, "low": 1, "medium": 2, "high": 3}
	if rank[candidate] > rank[current] {
		return candidate
	}
	return current
}
