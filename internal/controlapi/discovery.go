package controlapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/infercrane/infercrane/internal/openaicompat"
)

type endpointDiscovery struct {
	Runtime   string   `json:"runtime"`
	Connector string   `json:"connector"`
	Model     string   `json:"model"`
	Models    []string `json:"models"`
	Health    string   `json:"health"`
	Evidence  []string `json:"evidence"`
}

func discoverEndpoint(ctx context.Context, supplied *http.Client, baseURL, requestedModel, connector string) (endpointDiscovery, error) {
	if connector != "auto" && connector != "vllm" && connector != "litellm" && connector != "openai-compatible" {
		return endpointDiscovery{}, errors.New("connector must be auto, vllm, litellm, or openai-compatible")
	}
	modelsURL, err := openaicompat.Endpoint(baseURL, "models")
	if err != nil {
		return endpointDiscovery{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	client, err := restrictedDiscoveryClient(supplied)
	if err != nil {
		return endpointDiscovery{}, err
	}
	// Endpoint discovery is the intended network boundary. restrictedDiscoveryClient
	// removes ambient authority and validates every resolved address before dialing.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, modelsURL, nil)
	if err != nil {
		return endpointDiscovery{}, err
	}
	// The URL is an authenticated user's explicit inference endpoint. The
	// restricted client below disables proxies, redirects, cookies, and dials
	// only the IP addresses that passed the discovery address policy.
	// lgtm[go/request-forgery]
	resp, err := client.Do(req)
	if err != nil {
		return endpointDiscovery{}, fmt.Errorf("could not reach %s: %w", modelsURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return endpointDiscovery{}, fmt.Errorf("%s returned HTTP %d; connect currently requires an endpoint whose model list is readable by the control plane", modelsURL, resp.StatusCode)
	}
	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	decoder := json.NewDecoder(io.LimitReader(resp.Body, 1<<20))
	if err = decoder.Decode(&payload); err != nil {
		return endpointDiscovery{}, errors.New("model discovery returned invalid OpenAI-compatible JSON")
	}
	models := make([]string, 0, len(payload.Data))
	for _, item := range payload.Data {
		if id := strings.TrimSpace(item.ID); id != "" {
			models = append(models, id)
		}
	}
	sort.Strings(models)
	if len(models) == 0 {
		return endpointDiscovery{}, errors.New("model discovery returned no model identities")
	}
	model := requestedModel
	if model == "" {
		if len(models) != 1 {
			return endpointDiscovery{}, fmt.Errorf("endpoint exposes %d models (%s); select one with --model", len(models), strings.Join(models, ", "))
		}
		model = models[0]
	} else if !containsString(models, model) {
		return endpointDiscovery{}, fmt.Errorf("requested model %q is not present in /v1/models", model)
	}
	runtimeName := "openai-compatible"
	detectedConnector := connector
	if connector == "vllm" {
		runtimeName = "vllm"
	} else if connector == "litellm" {
		runtimeName = "litellm"
	} else if connector == "auto" {
		detectedConnector = "openai-compatible"
		server := strings.ToLower(resp.Header.Get("Server"))
		if strings.Contains(server, "litellm") {
			detectedConnector, runtimeName = "litellm", "litellm"
		} else if strings.Contains(server, "vllm") {
			detectedConnector, runtimeName = "vllm", "vllm"
		}
	}
	return endpointDiscovery{Runtime: runtimeName, Connector: detectedConnector, Model: model, Models: models, Health: "reachable", Evidence: []string{"GET /v1/models returned the selected model"}}, nil
}

func restrictedDiscoveryClient(supplied *http.Client) (*http.Client, error) {
	client := &http.Client{}
	var baseTransport *http.Transport
	if supplied != nil {
		*client = *supplied
		if supplied.Transport == nil {
			baseTransport = http.DefaultTransport.(*http.Transport)
		} else {
			var ok bool
			baseTransport, ok = supplied.Transport.(*http.Transport)
			if !ok {
				return nil, errors.New("endpoint discovery requires an inspectable HTTP transport")
			}
		}
	} else {
		baseTransport = http.DefaultTransport.(*http.Transport)
	}
	transport := baseTransport.Clone()
	transport.Proxy = nil
	// Force HTTPS through the same restricted DialContext. A supplied
	// DialTLSContext would otherwise be able to bypass the address checks.
	transport.DialTLSContext = nil
	dialer := &net.Dialer{Timeout: 3 * time.Second, KeepAlive: 30 * time.Second}
	transport.DialContext = func(dialCtx context.Context, network, address string) (net.Conn, error) {
		host, port, splitErr := net.SplitHostPort(address)
		if splitErr != nil {
			return nil, errors.New("endpoint discovery address is invalid")
		}
		addresses, lookupErr := net.DefaultResolver.LookupIPAddr(dialCtx, host)
		if lookupErr != nil || len(addresses) == 0 {
			return nil, errors.New("endpoint discovery DNS could not be resolved")
		}
		for _, candidate := range addresses {
			if unsafeDiscoveryIP(candidate.IP) {
				return nil, errors.New("endpoint discovery cannot access link-local, multicast, or unspecified addresses")
			}
		}
		return dialer.DialContext(dialCtx, network, net.JoinHostPort(addresses[0].IP.String(), port))
	}
	client.Transport = transport
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	client.Jar = nil
	if client.Timeout <= 0 || client.Timeout > 5*time.Second {
		client.Timeout = 5 * time.Second
	}
	return client, nil
}

func unsafeDiscoveryIP(ip net.IP) bool {
	return ip == nil || ip.IsUnspecified() || ip.IsMulticast() || ip.IsLinkLocalMulticast() || ip.IsLinkLocalUnicast()
}

func containsString(items []string, wanted string) bool {
	for _, item := range items {
		if item == wanted {
			return true
		}
	}
	return false
}
