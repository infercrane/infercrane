package autoscale

import (
	"errors"
	"fmt"
	"math"
	"strings"
)

// CapacityPolicy separates fast fleet capacity decisions from slow recipe
// optimization. Every demand dimension must fit: request rate, prefill tokens,
// decode tokens, and concurrent sequences. A single aggregate throughput
// number is never treated as universal capacity.
type CapacityPolicy struct {
	MinReplicas                 int     `json:"min_replicas"`
	MaxReplicas                 int     `json:"max_replicas"`
	FailureReserveReplicas      int     `json:"failure_reserve_replicas"`
	MinimumFaultDomains         int     `json:"minimum_fault_domains"`
	MaximumScaleUpStep          int     `json:"maximum_scale_up_step"`
	TargetProductiveUtilization float64 `json:"target_productive_utilization"`
	DemandForecastMultiplier    float64 `json:"demand_forecast_multiplier"`
	MaximumFleetHourlyCostUSD   float64 `json:"maximum_fleet_hourly_cost_usd"`
	MinimumContributionMargin   float64 `json:"minimum_contribution_margin"`
	RequireAvailableSupply      bool    `json:"require_available_supply"`
}

type DemandEnvelope struct {
	RequestsPerSecond     float64 `json:"requests_per_second"`
	InputTokensPerSecond  float64 `json:"input_tokens_per_second"`
	OutputTokensPerSecond float64 `json:"output_tokens_per_second"`
	ConcurrentRequests    float64 `json:"concurrent_requests"`
	RevenuePerHourUSD     float64 `json:"revenue_per_hour_usd"`
}

// ReplicaEnvelope must come from exact-host, exact-workload qualification.
// Cross-provider or modeled capacity belongs in planning, never autoscaling.
type ReplicaEnvelope struct {
	RequestsPerSecond     float64 `json:"requests_per_second"`
	InputTokensPerSecond  float64 `json:"input_tokens_per_second"`
	OutputTokensPerSecond float64 `json:"output_tokens_per_second"`
	ConcurrentRequests    float64 `json:"concurrent_requests"`
	HourlyCostUSD         float64 `json:"hourly_cost_usd"`
	EvidenceClass         string  `json:"evidence_class"`
	WorkloadDigest        string  `json:"workload_digest"`
	SupplyAvailable       bool    `json:"supply_available"`
	AvailableFaultDomains int     `json:"available_fault_domains"`
}

type FleetState struct {
	ReadyReplicas    int `json:"ready_replicas"`
	PendingReplicas  int `json:"pending_replicas"`
	DrainingReplicas int `json:"draining_replicas"`
}

type CapacityDecision struct {
	Action                      string   `json:"action"`
	CurrentReplicas             int      `json:"current_replicas"`
	TargetReplicas              int      `json:"target_replicas"`
	RequiredServingReplicas     int      `json:"required_serving_replicas"`
	FailureReserveReplicas      int      `json:"failure_reserve_replicas"`
	RequiredFaultDomains        int      `json:"required_fault_domains"`
	HighAvailabilityBlocked     bool     `json:"high_availability_blocked"`
	ProjectedHourlyCostUSD      float64  `json:"projected_hourly_cost_usd"`
	ProjectedContributionMargin float64  `json:"projected_contribution_margin"`
	ShedExcessTraffic           bool     `json:"shed_excess_traffic"`
	ReadyServingReplicas        int      `json:"ready_serving_replicas"`
	BindingDimensions           []string `json:"binding_dimensions"`
	Reasons                     []string `json:"reasons"`
}

// EvaluateCapacityEnvelope computes desired ready capacity. It does not
// provision infrastructure; the reconciler owns provider mutations and keeps
// admission fail-fast until new replicas are actually ready.
func EvaluateCapacityEnvelope(policy CapacityPolicy, demand DemandEnvelope, replica ReplicaEnvelope, fleet FleetState) (CapacityDecision, error) {
	if err := validateCapacityInputs(policy, demand, replica, fleet); err != nil {
		return CapacityDecision{}, err
	}
	d := CapacityDecision{Action: "hold", CurrentReplicas: fleet.ReadyReplicas + fleet.PendingReplicas, FailureReserveReplicas: policy.FailureReserveReplicas, RequiredFaultDomains: policy.MinimumFaultDomains}
	forecast := policy.DemandForecastMultiplier / policy.TargetProductiveUtilization
	type dimension struct {
		name             string
		demand, capacity float64
	}
	dimensions := []dimension{
		{"requests", demand.RequestsPerSecond, replica.RequestsPerSecond},
		{"prefill_tokens", demand.InputTokensPerSecond, replica.InputTokensPerSecond},
		{"decode_tokens", demand.OutputTokensPerSecond, replica.OutputTokensPerSecond},
		{"concurrency", demand.ConcurrentRequests, replica.ConcurrentRequests},
	}
	required := 0
	for _, item := range dimensions {
		if item.demand == 0 {
			continue
		}
		count := int(math.Ceil(item.demand * forecast / item.capacity))
		if count > required {
			required = count
			d.BindingDimensions = []string{item.name}
		} else if count == required {
			d.BindingDimensions = append(d.BindingDimensions, item.name)
		}
	}
	required = max(required, policy.MinReplicas)
	d.RequiredServingReplicas = required
	target := required + policy.FailureReserveReplicas
	target = max(target, policy.MinimumFaultDomains)
	target = max(policy.MinReplicas, min(policy.MaxReplicas, target))
	d.TargetReplicas = target
	d.ProjectedHourlyCostUSD = float64(target) * replica.HourlyCostUSD
	if demand.RevenuePerHourUSD > 0 {
		d.ProjectedContributionMargin = (demand.RevenuePerHourUSD - d.ProjectedHourlyCostUSD) / demand.RevenuePerHourUSD
	}

	available := fleet.ReadyReplicas + fleet.PendingReplicas - fleet.DrainingReplicas
	if replica.AvailableFaultDomains < policy.MinimumFaultDomains {
		d.HighAvailabilityBlocked = true
		d.Reasons = append(d.Reasons, fmt.Sprintf("only %d qualified fault domains are available; policy requires %d", replica.AvailableFaultDomains, policy.MinimumFaultDomains))
	}
	readyServing := fleet.ReadyReplicas - fleet.DrainingReplicas - policy.FailureReserveReplicas
	if fleet.ReadyReplicas > 0 && readyServing < 1 {
		// A one-replica fleet is degraded but still useful. The reserve becomes
		// enforceable as soon as the second replica is ready.
		readyServing = 1
	}
	d.ReadyServingReplicas = readyServing
	if required > readyServing {
		d.ShedExcessTraffic = true
		d.Reasons = append(d.Reasons, "forecast demand exceeds currently ready SLO-qualified capacity; fail fast above the safe admission envelope until scale-up completes")
	}
	if required+policy.FailureReserveReplicas > policy.MaxReplicas {
		d.ShedExcessTraffic = true
		d.Reasons = append(d.Reasons, "forecast demand exceeds the configured fleet maximum")
	}
	if target > available {
		if policy.RequireAvailableSupply && !replica.SupplyAvailable {
			d.ShedExcessTraffic = true
			d.Reasons = append(d.Reasons, "qualified accelerator supply is unavailable; preserve the active fleet and fail fast above safe admission")
			return d, nil
		}
		if policy.MaximumFleetHourlyCostUSD > 0 && d.ProjectedHourlyCostUSD > policy.MaximumFleetHourlyCostUSD {
			d.ShedExcessTraffic = true
			d.Reasons = append(d.Reasons, "required fleet exceeds the operator-approved hourly cost ceiling")
			return d, nil
		}
		if demand.RevenuePerHourUSD > 0 && d.ProjectedContributionMargin < policy.MinimumContributionMargin {
			d.ShedExcessTraffic = true
			d.Reasons = append(d.Reasons, "required fleet would violate contribution-margin policy; price, routing share, or recipe must change")
			return d, nil
		}
		stepTarget := min(target, available+policy.MaximumScaleUpStep)
		d.Action, d.TargetReplicas = "scale_up", stepTarget
		d.Reasons = append(d.Reasons, fmt.Sprintf("%s requires %d serving replicas plus %d failure reserve", strings.Join(d.BindingDimensions, ","), required, policy.FailureReserveReplicas))
		return d, nil
	}
	if target < fleet.ReadyReplicas-fleet.DrainingReplicas {
		d.Action = "scale_down"
		d.Reasons = append(d.Reasons, "forecast demand and failure reserve fit on a smaller qualified fleet")
		return d, nil
	}
	d.Reasons = append(d.Reasons, "ready and pending qualified capacity covers forecast demand plus failure reserve")
	return d, nil
}

func validateCapacityInputs(policy CapacityPolicy, demand DemandEnvelope, replica ReplicaEnvelope, fleet FleetState) error {
	if policy.MinReplicas < 1 || policy.MaxReplicas < policy.MinReplicas || policy.FailureReserveReplicas < 0 || policy.FailureReserveReplicas >= policy.MaxReplicas || policy.MinimumFaultDomains < 1 || policy.MinimumFaultDomains > policy.MaxReplicas || policy.MaximumScaleUpStep < 1 || policy.TargetProductiveUtilization <= 0 || policy.TargetProductiveUtilization >= 1 || policy.DemandForecastMultiplier < 1 || policy.MaximumFleetHourlyCostUSD < 0 || policy.MinimumContributionMargin < -1 || policy.MinimumContributionMargin >= 1 {
		return errors.New("capacity policy is outside safe bounds")
	}
	if replica.EvidenceClass != "measured" || replica.WorkloadDigest == "" {
		return errors.New("autoscaling requires exact measured replica capacity and workload digest")
	}
	if replica.AvailableFaultDomains < 1 {
		return errors.New("at least one qualified fault domain is required")
	}
	for _, value := range []float64{replica.RequestsPerSecond, replica.InputTokensPerSecond, replica.OutputTokensPerSecond, replica.ConcurrentRequests, replica.HourlyCostUSD} {
		if value <= 0 || math.IsNaN(value) || math.IsInf(value, 0) {
			return errors.New("replica capacity and cost must be positive finite values")
		}
	}
	for _, value := range []float64{demand.RequestsPerSecond, demand.InputTokensPerSecond, demand.OutputTokensPerSecond, demand.ConcurrentRequests, demand.RevenuePerHourUSD} {
		if value < 0 || math.IsNaN(value) || math.IsInf(value, 0) {
			return errors.New("demand and revenue must be non-negative finite values")
		}
	}
	if fleet.ReadyReplicas < 0 || fleet.PendingReplicas < 0 || fleet.DrainingReplicas < 0 || fleet.DrainingReplicas > fleet.ReadyReplicas {
		return errors.New("fleet replica counts are invalid")
	}
	return nil
}
