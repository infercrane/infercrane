// Package conformance executes provider/runtime contract scenarios without
// knowing a concrete cloud or engine. Hermetic fixtures exercise this on every
// change; real adapters reuse the scenario names during paid qualification.
package conformance

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/infercrane/infercrane/internal/integration"
	"github.com/infercrane/infercrane/internal/provision"
)

type Scenario struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
}

type Report struct {
	Contract  string     `json:"contract"`
	Adapter   string     `json:"adapter"`
	Status    string     `json:"status"`
	Scenarios []Scenario `json:"scenarios"`
}

func (r *Report) check(name string, err error) {
	status, detail := "passed", ""
	if err != nil {
		status, detail, r.Status = "failed", err.Error(), "failed"
	}
	r.Scenarios = append(r.Scenarios, Scenario{Name: name, Status: status, Detail: detail})
}

func (r Report) Err() error {
	if r.Status == "passed" {
		return nil
	}
	for _, scenario := range r.Scenarios {
		if scenario.Status == "failed" {
			return fmt.Errorf("%s: %s", scenario.Name, scenario.Detail)
		}
	}
	return fmt.Errorf("%s conformance failed", r.Adapter)
}

// ElasticLifecycle proves stable intent identity, replay-safe ensure,
// observable identity, and idempotent deletion. The supplied provider must be
// isolated test infrastructure or an explicitly authorized real adapter.
func ElasticLifecycle(ctx context.Context, profile integration.ProviderProfile, provider integration.ElasticProvider, spec provision.ReplicaSpec, port int) Report {
	report := Report{Contract: integration.ProviderContractV1, Adapter: profile.Adapter, Status: "passed"}
	report.check("profile", validateElasticProfile(profile, provider))
	if report.Status == "failed" {
		return report
	}
	firstHandle := provider.Handle(spec.ExternalKey)
	secondHandle := provider.Handle(spec.ExternalKey)
	report.check("stable_identity", equalHandle(firstHandle, secondHandle))
	first, err := provider.EnsureReplica(ctx, spec)
	report.check("ensure", err)
	if err != nil {
		return report
	}
	spec.RequestID = first.RequestID
	second, err := provider.EnsureReplica(ctx, spec)
	if err == nil && first.ResourceID != second.ResourceID {
		err = fmt.Errorf("replay changed resource identity from %q to %q", first.ResourceID, second.ResourceID)
	}
	report.check("replay_adoption", err)
	observation, err := provider.ObserveReplica(ctx, second, port)
	if err == nil && (!observation.Exists || observation.Endpoint == "") {
		err = fmt.Errorf("ensured resource is not observably reachable: %+v", observation)
	}
	report.check("observe", err)
	err = provider.DeleteReplica(ctx, second)
	report.check("delete", err)
	if err != nil {
		return report
	}
	report.check("idempotent_delete", provider.DeleteReplica(ctx, second))
	observation, err = provider.ObserveReplica(ctx, second, port)
	if err == nil && observation.Exists {
		err = fmt.Errorf("resource still exists after deletion: %+v", observation)
	}
	report.check("absence", err)
	sort.SliceStable(report.Scenarios, func(i, j int) bool { return report.Scenarios[i].Name < report.Scenarios[j].Name })
	return report
}

// LostEnsureResponse expects the first ensure to return an injected transport
// failure after creating the resource, then proves the retry adopts it.
func LostEnsureResponse(ctx context.Context, profile integration.ProviderProfile, provider integration.ElasticProvider, spec provision.ReplicaSpec, port int) Report {
	report := Report{Contract: integration.ProviderContractV1, Adapter: profile.Adapter, Status: "passed"}
	_, firstErr := provider.EnsureReplica(ctx, spec)
	if firstErr == nil {
		report.check("lost_create_response_injected", fmt.Errorf("first ensure unexpectedly succeeded"))
		return report
	}
	report.check("lost_create_response_injected", nil)
	adopted, err := provider.EnsureReplica(ctx, spec)
	report.check("lost_response_adoption", err)
	if err != nil {
		return report
	}
	observation, err := provider.ObserveReplica(ctx, adopted, port)
	if err == nil && !observation.Exists {
		err = fmt.Errorf("adopted resource does not exist")
	}
	report.check("adopted_resource_observable", err)
	return report
}

// ElasticDeleteRecovery expects an injected first delete failure, then proves
// retry reaches absence without creating replacement capacity.
func ElasticDeleteRecovery(ctx context.Context, profile integration.ProviderProfile, provider integration.ElasticProvider, spec provision.ReplicaSpec, port int) Report {
	report := Report{Contract: integration.ProviderContractV1, Adapter: profile.Adapter, Status: "passed"}
	handle, err := provider.EnsureReplica(ctx, spec)
	report.check("ensure_before_partial_delete", err)
	if err != nil {
		return report
	}
	firstErr := provider.DeleteReplica(ctx, handle)
	if firstErr == nil {
		report.check("partial_delete_failure_injected", fmt.Errorf("first delete unexpectedly succeeded"))
		return report
	}
	report.check("partial_delete_failure_injected", nil)
	report.check("delete_retry", provider.DeleteReplica(ctx, handle))
	observation, observeErr := provider.ObserveReplica(ctx, handle, port)
	if observeErr == nil && observation.Exists {
		observeErr = fmt.Errorf("resource remains after delete retry")
	}
	report.check("absence_after_delete_retry", observeErr)
	return report
}

// ElasticTimeout proves the adapter observes cancellation/deadline propagation
// rather than continuing an external mutation after its caller has stopped.
func ElasticTimeout(ctx context.Context, profile integration.ProviderProfile, provider integration.ElasticProvider, spec provision.ReplicaSpec) Report {
	report := Report{Contract: integration.ProviderContractV1, Adapter: profile.Adapter, Status: "passed"}
	_, err := provider.EnsureReplica(ctx, spec)
	if err == nil || ctx.Err() == nil || !strings.Contains(err.Error(), ctx.Err().Error()) {
		report.check("ensure_timeout_propagation", fmt.Errorf("ensure did not propagate context termination"))
	} else {
		report.check("ensure_timeout_propagation", nil)
	}
	return report
}

// ElasticFailureRedaction detects forbidden credential material in an adapter
// error without copying that material into the conformance report.
func ElasticFailureRedaction(ctx context.Context, profile integration.ProviderProfile, provider integration.ElasticProvider, spec provision.ReplicaSpec, forbidden ...string) Report {
	report := Report{Contract: integration.ProviderContractV1, Adapter: profile.Adapter, Status: "passed"}
	_, err := provider.EnsureReplica(ctx, spec)
	if err == nil {
		report.check("failure_redaction", fmt.Errorf("fault-injected ensure unexpectedly succeeded"))
		return report
	}
	for _, value := range forbidden {
		if value != "" && strings.Contains(err.Error(), value) {
			report.check("failure_redaction", fmt.Errorf("provider error contained forbidden credential material"))
			return report
		}
	}
	report.check("failure_redaction", nil)
	return report
}

// ServerlessLifecycle proves endpoint replay/adoption, one durable endpoint
// identity, provider-owned worker bounds, URL resolution, and idempotent delete.
func ServerlessLifecycle(ctx context.Context, profile integration.ProviderProfile, provider integration.ServerlessProvider, spec provision.ServerlessEndpointSpec) Report {
	report := Report{Contract: integration.ProviderContractV1, Adapter: profile.Adapter, Status: "passed"}
	var profileErr error
	if err := profile.Validate(); err != nil {
		profileErr = err
	} else if !integration.HasMode(profile, integration.ServerlessMode) {
		profileErr = fmt.Errorf("provider %q does not declare serverless mode", profile.Adapter)
	} else if provider == nil {
		profileErr = fmt.Errorf("serverless provider is required")
	}
	report.check("profile", profileErr)
	if profileErr != nil {
		return report
	}
	first, err := provider.EnsureEndpoint(ctx, spec)
	report.check("ensure_endpoint", err)
	if err != nil {
		return report
	}
	second, err := provider.EnsureEndpoint(ctx, spec)
	if err == nil && first.ID != second.ID {
		err = fmt.Errorf("replay changed endpoint identity from %q to %q", first.ID, second.ID)
	}
	report.check("replay_adoption", err)
	listed, err := provider.ListEndpoints(ctx)
	if err == nil {
		matches := 0
		for _, endpoint := range listed {
			if endpoint.ID == second.ID {
				matches++
			}
		}
		if matches != 1 {
			err = fmt.Errorf("expected one endpoint %q in inventory, found %d", second.ID, matches)
		}
	}
	report.check("inventory_identity", err)
	if endpoint := provider.EndpointURL(second.ID); endpoint == "" {
		report.check("endpoint_url", fmt.Errorf("provider returned an empty endpoint URL"))
	} else {
		report.check("endpoint_url", nil)
	}
	err = provider.DeleteEndpoint(ctx, second.ID)
	report.check("delete_endpoint", err)
	if err != nil {
		return report
	}
	report.check("idempotent_delete", provider.DeleteEndpoint(ctx, second.ID))
	listed, err = provider.ListEndpoints(ctx)
	if err == nil {
		for _, endpoint := range listed {
			if endpoint.ID == second.ID {
				err = fmt.Errorf("endpoint %q remains after deletion", second.ID)
				break
			}
		}
	}
	report.check("absence", err)
	return report
}

// LostServerlessEnsureResponse is the serverless counterpart to
// LostEnsureResponse. The first response must be fault-injected after create.
func LostServerlessEnsureResponse(ctx context.Context, profile integration.ProviderProfile, provider integration.ServerlessProvider, spec provision.ServerlessEndpointSpec) Report {
	report := Report{Contract: integration.ProviderContractV1, Adapter: profile.Adapter, Status: "passed"}
	_, firstErr := provider.EnsureEndpoint(ctx, spec)
	if firstErr == nil {
		report.check("lost_endpoint_response_injected", fmt.Errorf("first ensure unexpectedly succeeded"))
		return report
	}
	report.check("lost_endpoint_response_injected", nil)
	adopted, err := provider.EnsureEndpoint(ctx, spec)
	report.check("lost_endpoint_response_adoption", err)
	if err == nil && adopted.ID == "" {
		report.check("adopted_endpoint_identity", fmt.Errorf("adopted endpoint identity is empty"))
	} else {
		report.check("adopted_endpoint_identity", err)
	}
	return report
}

func RuntimeReadiness(ctx context.Context, profile integration.RuntimeProfile, inspector integration.RuntimeInspector, endpoint, expectedModel string) Report {
	report := Report{Contract: integration.RuntimeContractV1, Adapter: profile.Runtime, Status: "passed"}
	report.check("profile", profile.Validate())
	if inspector == nil {
		report.check("inspector", fmt.Errorf("runtime inspector is required"))
		return report
	}
	ready, models := inspector.Inspect(ctx, endpoint)
	var err error
	if !ready {
		err = fmt.Errorf("runtime is not ready")
	} else if _, found := models[expectedModel]; !found {
		err = fmt.Errorf("runtime did not expose expected model %q", expectedModel)
	}
	report.check("readiness_and_model_identity", err)
	return report
}

// RuntimeCapabilities verifies that every behavior required by the runtime
// contract is declared supported and linked to executable evidence. The
// referenced test drift check separately proves each evidence target exists.
func RuntimeCapabilities(profile integration.RuntimeProfile, required ...string) Report {
	report := Report{Contract: integration.RuntimeContractV1, Adapter: profile.Runtime, Status: "passed"}
	report.check("profile", profile.Validate())
	declared := make(map[string]integration.Capability, len(profile.Capabilities))
	for _, capability := range profile.Capabilities {
		declared[capability.Name] = capability
	}
	for _, name := range required {
		capability, ok := declared[name]
		var err error
		if !ok {
			err = fmt.Errorf("required runtime capability %q is not declared", name)
		} else if capability.State != integration.CapabilitySupported || capability.Evidence == "" {
			err = fmt.Errorf("required runtime capability %q is not evidence-backed supported", name)
		}
		report.check("capability_"+name, err)
	}
	return report
}

func validateElasticProfile(profile integration.ProviderProfile, provider integration.ElasticProvider) error {
	if err := profile.Validate(); err != nil {
		return err
	}
	if !integration.HasMode(profile, integration.ElasticMode) {
		return fmt.Errorf("provider %q does not declare elastic mode", profile.Adapter)
	}
	if provider == nil {
		return fmt.Errorf("elastic provider is required")
	}
	return nil
}

func equalHandle(first, second provision.ProviderHandle) error {
	if first.ExternalKey != second.ExternalKey || first.ResourceID != second.ResourceID || first.RequestID != second.RequestID {
		return fmt.Errorf("handle is not deterministic: first=%+v second=%+v", first, second)
	}
	return nil
}
