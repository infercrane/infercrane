package autoscale

import "testing"

func capacityPolicy() CapacityPolicy {
	return CapacityPolicy{MinReplicas: 1, MaxReplicas: 20, FailureReserveReplicas: 1, MinimumFaultDomains: 2, MaximumScaleUpStep: 4, TargetProductiveUtilization: .70, DemandForecastMultiplier: 1.15, MaximumFleetHourlyCostUSD: 100, MinimumContributionMargin: .20, RequireAvailableSupply: true}
}

func replicaCapacity() ReplicaEnvelope {
	return ReplicaEnvelope{RequestsPerSecond: 2.84, InputTokensPerSecond: 11500, OutputTokensPerSecond: 1450, ConcurrentRequests: 12, HourlyCostUSD: 4.6, EvidenceClass: "measured", WorkloadDigest: "sha256:workload", SupplyAvailable: true, AvailableFaultDomains: 2}
}

func TestCapacityEnvelopeScalesOnBindingDecodeDemandWithReserve(t *testing.T) {
	decision, err := EvaluateCapacityEnvelope(capacityPolicy(), DemandEnvelope{RequestsPerSecond: 10, InputTokensPerSecond: 40000, OutputTokensPerSecond: 6200, ConcurrentRequests: 55, RevenuePerHourUSD: 60}, replicaCapacity(), FleetState{ReadyReplicas: 1})
	if err != nil || decision.Action != "scale_up" || decision.RequiredServingReplicas != 8 || decision.TargetReplicas != 5 || len(decision.BindingDimensions) != 2 || decision.BindingDimensions[0] != "decode_tokens" || decision.BindingDimensions[1] != "concurrency" || !decision.ShedExcessTraffic {
		t.Fatalf("decision=%+v err=%v", decision, err)
	}
}

func TestCapacityEnvelopeBlocksUnprofitableGrowthAndSheds(t *testing.T) {
	policy := capacityPolicy()
	decision, err := EvaluateCapacityEnvelope(policy, DemandEnvelope{RequestsPerSecond: 10, InputTokensPerSecond: 40000, OutputTokensPerSecond: 6200, ConcurrentRequests: 55, RevenuePerHourUSD: 20}, replicaCapacity(), FleetState{ReadyReplicas: 1})
	if err != nil || decision.Action != "hold" || !decision.ShedExcessTraffic {
		t.Fatalf("decision=%+v err=%v", decision, err)
	}
}

func TestCapacityEnvelopeDoesNotAssumeUnavailableSupply(t *testing.T) {
	replica := replicaCapacity()
	replica.SupplyAvailable = false
	decision, err := EvaluateCapacityEnvelope(capacityPolicy(), DemandEnvelope{OutputTokensPerSecond: 3000, RevenuePerHourUSD: 50}, replica, FleetState{ReadyReplicas: 1})
	if err != nil || decision.Action != "hold" || !decision.ShedExcessTraffic {
		t.Fatalf("decision=%+v err=%v", decision, err)
	}
}

func TestCapacityEnvelopeFailsClosedWithoutMeasuredTuple(t *testing.T) {
	replica := replicaCapacity()
	replica.EvidenceClass = "modeled"
	if _, err := EvaluateCapacityEnvelope(capacityPolicy(), DemandEnvelope{}, replica, FleetState{ReadyReplicas: 1}); err == nil {
		t.Fatal("modeled capacity must not drive autoscaling")
	}
}

func TestCapacityEnvelopeMarksInsufficientFaultDomains(t *testing.T) {
	replica := replicaCapacity()
	replica.AvailableFaultDomains = 1
	decision, err := EvaluateCapacityEnvelope(capacityPolicy(), DemandEnvelope{OutputTokensPerSecond: 100, RevenuePerHourUSD: 20}, replica, FleetState{ReadyReplicas: 1})
	if err != nil || !decision.HighAvailabilityBlocked || decision.RequiredFaultDomains != 2 || decision.TargetReplicas != 2 {
		t.Fatalf("decision=%+v err=%v", decision, err)
	}
}

func TestOpenRouterFourPointSevenBillionTokenScenario(t *testing.T) {
	const totalTokensPerDay = 4_700_000_000.0
	const tokensPerRequest = 4512.0
	requestsPerSecond := totalTokensPerDay / tokensPerRequest / 86400
	demand := DemandEnvelope{
		RequestsPerSecond:     requestsPerSecond,
		InputTokensPerSecond:  requestsPerSecond * 4000,
		OutputTokensPerSecond: requestsPerSecond * 512,
		ConcurrentRequests:    requestsPerSecond * (12 / 2.84),
		RevenuePerHourUSD:     80,
	}
	decision, err := EvaluateCapacityEnvelope(capacityPolicy(), demand, replicaCapacity(), FleetState{ReadyReplicas: 1})
	if err != nil || decision.RequiredServingReplicas != 7 || decision.TargetReplicas != 5 || decision.FailureReserveReplicas != 1 {
		t.Fatalf("decision=%+v err=%v", decision, err)
	}
	// Target is step-limited to five on this reconciliation. The stable target
	// is seven serving replicas plus one reserve.
	if decision.ProjectedHourlyCostUSD != 8*4.6 {
		t.Fatalf("projected cost=%f", decision.ProjectedHourlyCostUSD)
	}
}
