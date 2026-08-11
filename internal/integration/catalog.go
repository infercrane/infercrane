package integration

import "github.com/infercrane/infercrane/internal/support"

// V02Catalog is the compiled integration inventory. It describes registered
// adapters and honest evidence states; public support remains owned by support.Matrix.
func V02Catalog() (*Registry, error) {
	registry := NewRegistry()
	profiles := []ProviderProfile{
		{
			Adapter: "skypilot", Cloud: "runpod", ContractVersion: ProviderContractV1, AdapterVersion: "builtin-v0.2", Modes: []ComputeMode{ElasticMode},
			Capabilities: []Capability{
				{Name: "adoption", State: CapabilitySupported, Evidence: "go:test/internal/provision#TestProviderContractSkyPilotLifecycle"},
				{Name: "capacity_preflight", State: CapabilitySupported, Detail: "advisory provider stock; not a reservation", Evidence: "go:test/internal/provision#TestRunPodAvailabilityAggregatesCompatibleGPUStock"},
				{Name: "idempotent_delete", State: CapabilitySupported, Evidence: "go:test/internal/provision#TestProviderContractSkyPilotLifecycle"},
				{Name: "orphan_inventory", State: CapabilitySupported, Evidence: "go:test/internal/provision#TestInventoryFiltersOwnedResources"},
			},
			Qualification: []Qualification{
				{State: QualificationLocal, Environment: "hermetic-provider-contract", Evidence: "go:test/internal/provision#TestProviderContractSkyPilotLifecycle"},
				{State: QualificationDeferred, Environment: "real-runpod-elastic", Reason: "awaiting consolidated v1 manual qualification"},
			},
		},
		{
			Adapter: "runpod-serverless", Cloud: "runpod", ContractVersion: ProviderContractV1, AdapterVersion: "builtin-v0.2", Modes: []ComputeMode{ServerlessMode},
			Capabilities: []Capability{
				{Name: "adoption", State: CapabilitySupported, Evidence: "go:test/internal/provision#TestProviderContractRunPodServerlessLifecycleAndLostResponseAdoption"},
				{Name: "provider_native_scale_to_zero", State: CapabilitySupported, Evidence: "go:test/internal/workflows#TestServerlessConvergeRegistersScaleToZeroEndpointWithoutWarmingWorker"},
				{Name: "stream_cancellation", State: CapabilitySupported, Evidence: "go:test/internal/gateway#TestClientCancellationPropagatesToRuntime"},
				{Name: "warm_workers", State: CapabilitySupported, Evidence: "go:test/internal/gateway#TestServerlessNonzeroWorkerEvidenceClassifiesWarmRequest"},
			},
			Qualification: []Qualification{
				{State: QualificationLocal, Environment: "hermetic-provider-contract", Evidence: "go:test/internal/provision#TestProviderContractRunPodServerlessLifecycleAndLostResponseAdoption"},
				{State: QualificationDeferred, Environment: "real-runpod-serverless", Reason: "awaiting consolidated v1 manual qualification"},
			},
		},
		{
			Adapter: "openai-compatible-external", Cloud: "external", ContractVersion: ProviderContractV1, AdapterVersion: "boundary-v0.2", Modes: []ComputeMode{ExternalMode},
			Capabilities: []Capability{
				{Name: "provisioning", State: CapabilityUnsupported, Detail: "external targets are registered, not provisioned"},
				{Name: "streaming", State: CapabilityUnknown, Detail: "must be qualified per target"},
			},
			Qualification: []Qualification{{State: QualificationRegistered, Environment: "contract-boundary"}},
		},
	}
	for _, profile := range profiles {
		if err := registry.RegisterProvider(profile); err != nil {
			return nil, err
		}
	}
	if err := registry.RegisterRuntime(RuntimeProfile{
		Runtime: support.DefaultRuntime, ContractVersion: RuntimeContractV1, AdapterVersion: "builtin-v0.2", EngineVersion: support.DefaultRuntimeVersion, Protocol: "openai",
		Capabilities: []Capability{
			{Name: "buffered_chat", State: CapabilitySupported, Evidence: "go:test/internal/gateway#TestCompletionRewritesAlias"},
			{Name: "cancellation", State: CapabilitySupported, Evidence: "go:test/internal/gateway#TestClientCancellationPropagatesToRuntime"},
			{Name: "graceful_drain", State: CapabilitySupported, Evidence: "go:test/internal/reconcile#TestRouterRetirementWaitsForPinnedRequest"},
			{Name: "readiness", State: CapabilitySupported, Evidence: "go:test/internal/conformance#TestRuntimeReadinessConformance"},
			{Name: "streaming_chat", State: CapabilitySupported, Evidence: "go:test/internal/gateway#TestActiveStreamKeepsSelectedRouterAcrossGenerationPublish"},
			{Name: "structured_output", State: CapabilitySupported, Evidence: "go:test/internal/gateway#TestCompletionPreservesOpenAIParametersAndStructuredTools"},
			{Name: "telemetry", State: CapabilitySupported, Evidence: "go:test/internal/gateway#TestTelemetryExportsPrometheusHistogram"},
			{Name: "tool_calling", State: CapabilitySupported, Evidence: "go:test/internal/gateway#TestCompletionPreservesOpenAIParametersAndStructuredTools"},
		},
		Qualification: []Qualification{
			{State: QualificationLocal, Environment: "docker-fake-vllm", Evidence: "make:dev-check-full"},
			{State: QualificationDeferred, Environment: "real-vllm-gpu", Reason: "awaiting consolidated v1 manual qualification"},
		},
	}); err != nil {
		return nil, err
	}
	return registry, nil
}
