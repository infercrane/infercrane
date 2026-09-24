package openrouterprovider

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strings"
	"sync"
)

const (
	ChannelOpenRouter  = "openrouter"
	ChannelHuggingFace = "huggingface"
	ChannelRequesty    = "requesty"
	ChannelVercel      = "vercel"
	ChannelOpenCode    = "opencode"
	ChannelKilo        = "kilo"
)

type ChannelCredential struct {
	Channel string
	APIKey  string
}

type ChannelPolicy struct {
	MaxInFlight              int
	InputPricePerMillionUSD  float64
	OutputPricePerMillionUSD float64
	BillingLookupEnabled     bool
}

type requestIdentity struct {
	channel   string
	requestID string
}

type requestIdentityKey struct{}

func identityFromRequest(request *http.Request) requestIdentity {
	identity, _ := request.Context().Value(requestIdentityKey{}).(requestIdentity)
	return identity
}

func supportedChannel(channel string) bool {
	switch channel {
	case ChannelOpenRouter, ChannelHuggingFace, ChannelRequesty, ChannelVercel, ChannelOpenCode, ChannelKilo:
		return true
	default:
		return false
	}
}

func validateChannelCredentials(credentials []ChannelCredential) error {
	if len(credentials) == 0 {
		return errors.New("at least one marketplace credential is required")
	}
	seenChannels := make(map[string]struct{}, len(credentials))
	seenKeys := make(map[string]struct{}, len(credentials))
	for _, credential := range credentials {
		if !supportedChannel(credential.Channel) {
			return fmt.Errorf("unsupported marketplace channel %q", credential.Channel)
		}
		if strings.TrimSpace(credential.APIKey) == "" {
			return fmt.Errorf("marketplace channel %q has an empty API key", credential.Channel)
		}
		if _, duplicate := seenChannels[credential.Channel]; duplicate {
			return fmt.Errorf("duplicate marketplace channel %q", credential.Channel)
		}
		if _, duplicate := seenKeys[credential.APIKey]; duplicate {
			return errors.New("marketplace API keys must be unique across channels")
		}
		seenChannels[credential.Channel] = struct{}{}
		seenKeys[credential.APIKey] = struct{}{}
	}
	return nil
}

func validateChannelPolicies(policies map[string]ChannelPolicy) error {
	for channel, policy := range policies {
		if !supportedChannel(channel) {
			return fmt.Errorf("unsupported marketplace policy channel %q", channel)
		}
		if policy.MaxInFlight < 0 {
			return fmt.Errorf("marketplace channel %q has a negative concurrency limit", channel)
		}
		for _, price := range []float64{policy.InputPricePerMillionUSD, policy.OutputPricePerMillionUSD} {
			if math.IsNaN(price) || math.IsInf(price, 0) || price < 0 {
				return fmt.Errorf("marketplace channel %q has an invalid price", channel)
			}
		}
	}
	return nil
}

type channelAdmissionController struct {
	mu       sync.Mutex
	limits   map[string]int
	inFlight map[string]int
}

func newChannelAdmissionController(policies map[string]ChannelPolicy) *channelAdmissionController {
	limits := make(map[string]int, len(policies))
	for channel, policy := range policies {
		if policy.MaxInFlight > 0 {
			limits[channel] = policy.MaxInFlight
		}
	}
	return &channelAdmissionController{limits: limits, inFlight: make(map[string]int)}
}

func (c *channelAdmissionController) tryAcquire(channel string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	limit := c.limits[channel]
	if limit > 0 && c.inFlight[channel] >= limit {
		return false
	}
	c.inFlight[channel]++
	return true
}

func (c *channelAdmissionController) release(channel string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.inFlight[channel] <= 1 {
		delete(c.inFlight, channel)
		return
	}
	c.inFlight[channel]--
}

func (e *Edge) marketplaceAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		requestID := randomRequestID()
		writer.Header().Set("X-Request-ID", requestID)
		writer.Header().Set("Inference-Id", requestID)
		authorization := request.Header.Get("Authorization")
		channel := ""
		for _, credential := range e.marketplaceCredentials() {
			expected := "Bearer " + credential.APIKey
			if subtle.ConstantTimeCompare([]byte(authorization), []byte(expected)) == 1 {
				channel = credential.Channel
			}
		}
		if channel == "" {
			writer.Header().Set("WWW-Authenticate", "Bearer")
			writeProviderError(writer, http.StatusUnauthorized, "Invalid provider API key", "authentication_error")
			return
		}
		identity := requestIdentity{channel: channel, requestID: requestID}
		next(writer, request.WithContext(context.WithValue(request.Context(), requestIdentityKey{}, identity)))
	}
}

func (e *Edge) marketplaceCredentials() []ChannelCredential {
	credentials := append([]ChannelCredential(nil), e.Credentials...)
	if e.APIKey != "" {
		found := false
		for _, credential := range credentials {
			found = found || credential.Channel == ChannelOpenRouter
		}
		if !found {
			credentials = append(credentials, ChannelCredential{Channel: ChannelOpenRouter, APIKey: e.APIKey})
		}
	}
	return credentials
}

func (e *Edge) channelPolicy(channel string) ChannelPolicy {
	policy := e.ChannelPolicies[channel]
	if policy.InputPricePerMillionUSD == 0 {
		policy.InputPricePerMillionUSD = e.InputPricePerMillionUSD
	}
	if policy.OutputPricePerMillionUSD == 0 {
		policy.OutputPricePerMillionUSD = e.OutputPricePerMillionUSD
	}
	return policy
}

func requestCostNanoUSD(promptTokens, completionTokens int64, policy ChannelPolicy) int64 {
	if promptTokens < 0 || completionTokens < 0 {
		return 0
	}
	// A price expressed in USD per million tokens is price*1,000 nano-USD
	// per token. Round up once at the request boundary so a successful call
	// is never under-reported by fractional nano-USD arithmetic.
	cost := float64(promptTokens)*policy.InputPricePerMillionUSD*1_000 +
		float64(completionTokens)*policy.OutputPricePerMillionUSD*1_000
	if cost <= 0 {
		return 0
	}
	if rounded := math.Round(cost); math.Abs(cost-rounded) <= 1e-9*math.Max(1, math.Abs(cost)) {
		return int64(rounded)
	}
	return int64(math.Ceil(cost))
}
