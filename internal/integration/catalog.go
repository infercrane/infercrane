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

// V03Catalog extends the stable V1 contracts with governed external capacity
// and the narrow AWS EC2 BYOC adapter. Real-provider qualification remains
// explicitly deferred until the consolidated v1 manual gate.
func V03Catalog() (*Registry, error) {
	registry, err := V02Catalog()
	if err != nil {
		return nil, err
	}
	profiles := []ProviderProfile{
		{
			Adapter: "openrouter", Cloud: "external", ContractVersion: ProviderContractV1, AdapterVersion: "builtin-v0.3", Modes: []ComputeMode{ExternalMode},
			Capabilities: []Capability{
				{Name: "explicit_health_fallback", State: CapabilitySupported, Evidence: "go:test/internal/reconcile#TestExternalFallbackPublishesOnlyWhenNoPrimaryTargetIsHealthy"},
				{Name: "hard_budget", State: CapabilitySupported, Evidence: "go:test/internal/external#TestBudgetPoolNeverAuthorizesBeyondLease"},
				{Name: "privacy_acknowledgement", State: CapabilitySupported, Evidence: "go:test/internal/controlapi#TestExternalPolicyRequiresPrivacyAndHardBudgets"},
				{Name: "streaming_without_replay", State: CapabilitySupported, Evidence: "go:test/internal/gateway#TestExternalFallbackConsumesHardBudgetBeforeTransmissionAndNeverReplaysStream"},
				{Name: "provisioning", State: CapabilityUnsupported, Detail: "OpenRouter is a governed external target, not provisioned capacity"},
			},
			Qualification: []Qualification{
				{State: QualificationLocal, Environment: "hermetic-external-target", Evidence: "go:test/internal/external#TestCoordinatorSelectsOnlyHealthyExplicitBudgetedFallback"},
				{State: QualificationDeferred, Environment: "real-openrouter", Reason: "awaiting consolidated v1 manual qualification"},
			},
		},
		{
			Adapter: "aws-ec2", Cloud: "aws", ContractVersion: ProviderContractV1, AdapterVersion: "builtin-v0.3", Modes: []ComputeMode{ElasticMode},
			Capabilities: []Capability{
				{Name: "adoption", State: CapabilitySupported, Evidence: "go:test/internal/conformance#TestAWSEC2LostCreateResponseConformance"},
				{Name: "idempotent_delete", State: CapabilitySupported, Evidence: "go:test/internal/conformance#TestAWSEC2ProviderContractConformance"},
				{Name: "iam_role_assumption", State: CapabilitySupported, Evidence: "go:test/internal/provision#TestAWSEC2RedactsRoleFailureOutput"},
				{Name: "immutable_workload", State: CapabilitySupported, Evidence: "go:test/internal/provision#TestAWSEC2RejectsMutableImageBeforeAWSCall"},
				{Name: "private_network", State: CapabilitySupported, Evidence: "go:test/internal/provision#TestAWSEC2LifecycleIsIdempotentPrivateAndTagged"},
				{Name: "orphan_inventory", State: CapabilitySupported, Evidence: "go:test/internal/conformance#TestAWSEC2ProviderContractConformance"},
			},
			Qualification: []Qualification{
				{State: QualificationLocal, Environment: "hermetic-aws-cli", Evidence: "go:test/internal/conformance#TestAWSEC2ProviderContractConformance"},
				{State: QualificationDeferred, Environment: "real-aws-ec2", Reason: "awaiting consolidated v1 manual qualification"},
			},
		},
	}
	for _, profile := range profiles {
		if err := registry.RegisterProvider(profile); err != nil {
			return nil, err
		}
	}
	return registry, nil
}
