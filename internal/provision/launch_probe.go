package provision

import (
	"context"
	"time"
)

// LaunchProbeRequest describes a provider-neutral, read-only placement
// preflight. It must never reserve or create capacity.
type LaunchProbeRequest struct {
	Provider string `json:"provider"`
	Region   string `json:"region,omitempty"`
	GPU      string `json:"gpu"`
	GPUCount int    `json:"gpu_count"`
}

// LaunchProbeEvidence deliberately separates facts that provider APIs often
// conflate. A configured credential is not stock, stock is not quota, and none
// of them is a reservation. Unknown is the fail-closed default.
type LaunchProbeEvidence struct {
	Provider          string    `json:"provider"`
	Region            string    `json:"region,omitempty"`
	GPU               string    `json:"gpu"`
	GPUCount          int       `json:"gpu_count"`
	ConnectionState   string    `json:"connection_state"`
	AvailabilityState string    `json:"availability_state"`
	QuotaState        string    `json:"quota_state"`
	Deployability     string    `json:"deployability"`
	Source            string    `json:"source"`
	ObservedAt        time.Time `json:"observed_at"`
	ExpiresAt         time.Time `json:"expires_at"`
	Message           string    `json:"message"`
	Limitations       []string  `json:"limitations,omitempty"`
}

type LaunchProber interface {
	ProbeLaunch(context.Context, LaunchProbeRequest) (LaunchProbeEvidence, error)
}

// ConfiguredLaunchProbe is used when a provider connection is known to be
// configured but its API has no safe non-mutating stock/quota operation.
// Returning unknown is more useful and safer than treating credentials as
// proof that a launch will succeed.
type ConfiguredLaunchProbe struct {
	Provider, Source string
	Now              func() time.Time
}

func (p ConfiguredLaunchProbe) ProbeLaunch(_ context.Context, request LaunchProbeRequest) (LaunchProbeEvidence, error) {
	now := time.Now().UTC()
	if p.Now != nil {
		now = p.Now().UTC()
	}
	provider := p.Provider
	if provider == "" {
		provider = request.Provider
	}
	source := p.Source
	if source == "" {
		source = provider + ".configuration"
	}
	return LaunchProbeEvidence{
		Provider: provider, Region: request.Region, GPU: request.GPU, GPUCount: request.GPUCount,
		ConnectionState: "configured", AvailabilityState: "unknown", QuotaState: "unknown", Deployability: "unknown",
		Source: source, ObservedAt: now, ExpiresAt: now.Add(30 * time.Second),
		Message:     "Provider credentials are configured, but current stock and quota cannot be proven without a launch",
		Limitations: []string{"read-only preflight does not reserve capacity", "a catalog price is not a launch quote"},
	}, nil
}
