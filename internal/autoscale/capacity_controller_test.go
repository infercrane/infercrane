package autoscale

import (
	"context"
	"errors"
	"testing"
)

type capacityRepositoryStub struct {
	deployments []CapacityDeployment
	demand      DemandEnvelope
	recorded    []CapacityDecision
}

func (s *capacityRepositoryStub) CapacityDeployments(context.Context) ([]CapacityDeployment, error) {
	return s.deployments, nil
}
func (s *capacityRepositoryStub) CapacityDemand(context.Context, string) (DemandEnvelope, error) {
	return s.demand, nil
}
func (s *capacityRepositoryStub) RecordCapacityDecision(_ context.Context, _ string, decision CapacityDecision, evidence string) error {
	if evidence == "" {
		return errors.New("evidence required")
	}
	s.recorded = append(s.recorded, decision)
	return nil
}

type capacityAdmissionStub struct{ decisions []CapacityDecision }

func (s *capacityAdmissionStub) ApplyCapacityDecision(_ context.Context, _ string, decision CapacityDecision) error {
	s.decisions = append(s.decisions, decision)
	return nil
}

type capacityFleetStub struct {
	target int
	err    error
}

func (s *capacityFleetStub) ScaleCapacityTo(_ context.Context, _ string, target int) error {
	s.target = target
	return s.err
}

func TestCapacityControllerBoundsAdmissionBeforeScaling(t *testing.T) {
	repository := &capacityRepositoryStub{
		deployments: []CapacityDeployment{{ID: "qwen38", Policy: capacityPolicy(), Replica: replicaCapacity(), Fleet: FleetState{ReadyReplicas: 1}}},
		demand:      DemandEnvelope{RequestsPerSecond: 10, InputTokensPerSecond: 40000, OutputTokensPerSecond: 6200, ConcurrentRequests: 55, RevenuePerHourUSD: 60},
	}
	admission, fleet := &capacityAdmissionStub{}, &capacityFleetStub{}
	if err := (CapacityController{Repository: repository, Admission: admission, Fleet: fleet}).Once(context.Background()); err != nil {
		t.Fatal(err)
	}
	if fleet.target != 5 || len(admission.decisions) != 1 || len(repository.recorded) != 1 {
		t.Fatalf("target=%d admission=%d recorded=%d", fleet.target, len(admission.decisions), len(repository.recorded))
	}
	if !admission.decisions[0].ShedExcessTraffic {
		t.Fatal("admission must remain fail-fast while qualified replicas are launching")
	}
}

func TestCapacityControllerRecordsProviderFailureWithoutOpeningAdmission(t *testing.T) {
	repository := &capacityRepositoryStub{
		deployments: []CapacityDeployment{{ID: "qwen38", Policy: capacityPolicy(), Replica: replicaCapacity(), Fleet: FleetState{ReadyReplicas: 1}}},
		demand:      DemandEnvelope{OutputTokensPerSecond: 6200, RevenuePerHourUSD: 60},
	}
	admission, fleet := &capacityAdmissionStub{}, &capacityFleetStub{err: errors.New("supply exhausted")}
	err := (CapacityController{Repository: repository, Admission: admission, Fleet: fleet}).Once(context.Background())
	if err == nil || len(repository.recorded) != 1 || repository.recorded[0].Action != "scale_failed" || len(admission.decisions) != 1 {
		t.Fatalf("err=%v recorded=%+v admission=%+v", err, repository.recorded, admission.decisions)
	}
}
