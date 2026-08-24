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
				{Name: "hard_budget", State: CapabilitySupported, Evidence: "go:test/internal/store#TestManagedExternalBindingIsImmutableTenantSafeAndHardBudgeted"},
				{Name: "provisioning", State: CapabilityUnsupported, Detail: "external targets are registered, not provisioned"},
				{Name: "reference_credentials", State: CapabilitySupported, Evidence: "go:test/internal/controlapi#TestManagedExternalEndpointBindingRequiresConsentReferenceAndHardBudget"},
				{Name: "stable_endpoint_binding", State: CapabilitySupported, Evidence: "go:test/internal/reconcile#TestManagedExternalBindingCompilesThroughCredentialAndBudgetCoordinator"},
				{Name: "streaming", State: CapabilityUnknown, Detail: "must be qualified per target"},
			},
			Qualification: []Qualification{
				{State: QualificationLocal, Environment: "hermetic-openai-compatible", Evidence: "go:test/internal/external#TestCoordinatorResolvesManagedExternalBindingWithoutPersistingCredential"},
				{State: QualificationDeferred, Environment: "real-openai-compatible", Reason: "target-specific streaming and provider behavior require operator qualification"},
			},
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
			{Name: "completions", State: CapabilitySupported, Evidence: "go:test/internal/gateway#TestQualifiedProtocolSurfacesPreservePayloads"},
			{Name: "embeddings", State: CapabilitySupported, Detail: "requires an embedding-capable model", Evidence: "go:test/internal/gateway#TestQualifiedProtocolSurfacesPreservePayloads"},
			{Name: "responses", State: CapabilityUnknown, Detail: "the upgraded vLLM profile exposes this surface, but it remains unclaimed until protocol and real-GPU qualification pass"},
			{Name: "chat_batch", State: CapabilityUnknown, Detail: "the upgraded vLLM profile exposes this surface, but it remains unclaimed until protocol and real-GPU qualification pass"},
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
				{Name: "reference_credentials", State: CapabilitySupported, Evidence: "go:test/internal/controlapi#TestManagedExternalEndpointBindingRequiresConsentReferenceAndHardBudget"},
				{Name: "stable_endpoint_binding", State: CapabilitySupported, Evidence: "go:test/internal/reconcile#TestManagedExternalBindingCompilesThroughCredentialAndBudgetCoordinator"},
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

// V06Catalog adds portable OCI and SGLang profiles without changing the V1
// contract version. Real GPU qualification remains explicitly deferred.
func V06Catalog() (*Registry, error) {
	registry, err := V03Catalog()
	if err != nil {
		return nil, err
	}
	common := []Capability{
		{Name: "autoscaling_signals", State: CapabilityUnsupported, Detail: "v0.6 portable runtimes require fixed replica bounds until normalized runtime metrics are qualified"},
		{Name: "buffered_chat", State: CapabilitySupported, Evidence: "go:test/internal/gateway#TestCompletionRewritesAlias"},
		{Name: "cancellation", State: CapabilitySupported, Evidence: "go:test/internal/gateway#TestClientCancellationPropagatesToRuntime"},
		{Name: "graceful_drain", State: CapabilitySupported, Evidence: "go:test/internal/reconcile#TestRouterRetirementWaitsForPinnedRequest"},
		{Name: "immutable_oci_image", State: CapabilitySupported, Evidence: "go:test/internal/runtimecontract#TestWorkloadValidation"},
		{Name: "readiness", State: CapabilitySupported, Evidence: "go:test/internal/conformance#TestPortableRuntimeConformance"},
		{Name: "streaming_chat", State: CapabilitySupported, Evidence: "go:test/internal/gateway#TestActiveStreamKeepsSelectedRouterAcrossGenerationPublish"},
		{Name: "telemetry", State: CapabilitySupported, Evidence: "go:test/internal/conformance#TestPortableRuntimeConformance"},
	}
	sglang := RuntimeProfile{
		Runtime: "sglang", ContractVersion: RuntimeContractV1, AdapterVersion: "builtin-v0.6", EngineVersion: support.SGLangRuntimeVersion, Protocol: "openai", Capabilities: common,
		DefaultWorkload: support.SGLangWorkload(),
		Qualification:   []Qualification{{State: QualificationSimulated, Environment: "hermetic-runtime-contract", Evidence: "go:test/internal/conformance#TestPortableRuntimeConformance"}, {State: QualificationDeferred, Environment: "real-sglang-gpu", Reason: "awaiting consolidated v1 manual qualification"}},
	}
	custom := RuntimeProfile{Runtime: "custom-oci", ContractVersion: RuntimeContractV1, AdapterVersion: "declarative-v0.6", Protocol: "openai", Capabilities: common, Qualification: []Qualification{{State: QualificationSimulated, Environment: "hermetic-runtime-contract", Evidence: "go:test/internal/conformance#TestPortableRuntimeConformance"}, {State: QualificationDeferred, Environment: "real-custom-oci-gpu", Reason: "awaiting consolidated v1 manual qualification"}}}
	for _, profile := range []RuntimeProfile{sglang, custom} {
		if err = registry.RegisterRuntime(profile); err != nil {
			return nil, err
		}
	}
	if err = registry.SetCompatibility(
		RuntimeCompatibility{Runtime: "vllm", Adapter: "skypilot", Cloud: "runpod", Mode: ElasticMode, State: QualificationLocal, Evidence: "make:dev-check-full"},
		RuntimeCompatibility{Runtime: "vllm", Adapter: "runpod-serverless", Cloud: "runpod", Mode: ServerlessMode, State: QualificationLocal, Evidence: "go:test/internal/workflows#TestServerlessConvergeRegistersScaleToZeroEndpointWithoutWarmingWorker"},
		RuntimeCompatibility{Runtime: "vllm", Adapter: "aws-ec2", Cloud: "aws", Mode: ElasticMode, State: QualificationLocal, Evidence: "go:test/internal/conformance#TestAWSEC2ProviderContractConformance"},
		RuntimeCompatibility{Runtime: "sglang", Adapter: "aws-ec2", Cloud: "aws", Mode: ElasticMode, State: QualificationSimulated, Evidence: "go:test/internal/conformance#TestPortableRuntimeConformance"},
		RuntimeCompatibility{Runtime: "custom-oci", Adapter: "aws-ec2", Cloud: "aws", Mode: ElasticMode, State: QualificationSimulated, Evidence: "go:test/internal/conformance#TestPortableRuntimeConformance"},
	); err != nil {
		return nil, err
	}
	return registry, nil
}

// V09Catalog adds the namespaced Kubernetes provider boundary. Kind proves
// Kubernetes API lifecycle semantics; real GPU/runtime compatibility remains
// deferred until the consolidated v1 manual qualification.
func V09Catalog() (*Registry, error) {
	registry, err := V06Catalog()
	if err != nil {
		return nil, err
	}
	if err = registry.RegisterProvider(ProviderProfile{
		Adapter: "kubernetes", Cloud: "kubernetes", ContractVersion: ProviderContractV1, AdapterVersion: "builtin-v0.9", Modes: []ComputeMode{ElasticMode},
		Capabilities: []Capability{
			{Name: "adoption", State: CapabilitySupported, Evidence: "go:test/internal/conformance#TestKubernetesLostCreateResponseConformance"},
			{Name: "idempotent_delete", State: CapabilitySupported, Evidence: "go:test/internal/conformance#TestKubernetesProviderContractConformance"},
			{Name: "immutable_workload", State: CapabilitySupported, Evidence: "go:test/internal/provision#TestKubernetesConfigurationAndManifestFailClosed"},
			{Name: "kserve_standard", State: CapabilitySupported, Detail: "conditional on the serving.kserve.io InferenceService CRD; KServe owns generated children", Evidence: "go:test/internal/provision#TestKubernetesKServeModeRequiresCRDAndUsesOneOwner"},
			{Name: "gateway_api_exposure", State: CapabilitySupported, Detail: "optional HTTPRoute exposes the InferCrane gateway and never routes revisions", Evidence: "script:scripts/test-kubernetes-manifests.sh"},
			{Name: "namespaced_rbac", State: CapabilitySupported, Evidence: "script:scripts/test-kubernetes-manifests.sh"},
			{Name: "orphan_inventory", State: CapabilitySupported, Evidence: "go:test/internal/provision#TestKubernetesProviderAdoptsLostResponseAndRepairsPartialOwnerSet"},
			{Name: "server_side_apply", State: CapabilitySupported, Evidence: "script:scripts/test-kubernetes-kind.sh"},
			{Name: "advanced_disaggregated_runtime", State: CapabilityUnsupported, Detail: "KServe LLMInferenceService, llm-d, and Dynamo own routing or scheduling and require a future explicit routing contract"},
		},
		Qualification: []Qualification{
			{State: QualificationLocal, Environment: "kind-and-hermetic-kubectl", Evidence: "script:scripts/test-kubernetes-kind.sh"},
			{State: QualificationDeferred, Environment: "real-kubernetes-gpu", Reason: "awaiting consolidated v1 manual qualification"},
		},
	}); err != nil {
		return nil, err
	}
	compatibility := append(registry.Snapshot().Compatibility,
		RuntimeCompatibility{Runtime: "vllm", Adapter: "kubernetes", Cloud: "kubernetes", Mode: ElasticMode, State: QualificationSimulated, Evidence: "go:test/internal/conformance#TestKubernetesProviderContractConformance"},
		RuntimeCompatibility{Runtime: "sglang", Adapter: "kubernetes", Cloud: "kubernetes", Mode: ElasticMode, State: QualificationSimulated, Evidence: "go:test/internal/conformance#TestPortableRuntimeConformance"},
		RuntimeCompatibility{Runtime: "custom-oci", Adapter: "kubernetes", Cloud: "kubernetes", Mode: ElasticMode, State: QualificationSimulated, Evidence: "go:test/internal/conformance#TestPortableRuntimeConformance"},
	)
	if err = registry.SetCompatibility(compatibility...); err != nil {
		return nil, err
	}
	return registry, nil
}

// V15Catalog introduces independently identified provider profiles. Registration
// is not qualification: mutation profiles become local-qualified only after an
// executable adapter passes Provider Contract V1, while real environments stay
// deferred until the approval-locked manual gate.
func V15Catalog() (*Registry, error) {
	registry, err := V09Catalog()
	if err != nil {
		return nil, err
	}
	// External gateways remain separately operated dependencies. These profiles
	// describe only the protocol surfaces InferCrane is qualified to preserve;
	// they do not transfer gateway lifecycle or licensing into the control plane.
	externalCapabilities := []Capability{
		{Name: "buffered_chat", State: CapabilitySupported, Evidence: "go:test/internal/gateway#TestQualifiedProtocolSurfacesPreservePayloads"},
		{Name: "responses", State: CapabilitySupported, Evidence: "go:test/internal/gateway#TestQualifiedProtocolSurfacesPreservePayloads"},
		{Name: "embeddings", State: CapabilitySupported, Evidence: "go:test/internal/gateway#TestQualifiedProtocolSurfacesPreservePayloads"},
		{Name: "completions", State: CapabilitySupported, Evidence: "go:test/internal/gateway#TestQualifiedProtocolSurfacesPreservePayloads"},
		{Name: "streaming_chat", State: CapabilitySupported, Evidence: "go:test/internal/gateway#TestActiveStreamKeepsSelectedRouterAcrossGenerationPublish"},
		{Name: "cancellation", State: CapabilitySupported, Evidence: "go:test/internal/gateway#TestClientCancellationPropagatesToRuntime"},
	}
	for _, runtimeName := range []string{"openai-compatible", "litellm"} {
		if err = registry.RegisterRuntime(RuntimeProfile{Runtime: runtimeName, ContractVersion: RuntimeContractV1, AdapterVersion: "external-v2.1", Protocol: "openai", Capabilities: externalCapabilities, Qualification: []Qualification{{State: QualificationLocal, Environment: "hermetic-openai-compatible", Evidence: "go:test/internal/controlapi#TestDiscoverEndpointSelectsSingleModelAndConservativelyClassifiesRuntime"}, {State: QualificationDeferred, Environment: "real-" + runtimeName, Reason: "operator-managed gateway requires target-specific qualification"}}}); err != nil {
			return nil, err
		}
	}
	for _, profile := range []CompositionProfile{
		{
			Adapter: "litellm", Kind: "gateway", ContractVersion: CompositionV1,
			Ownership: "LiteLLM owns provider protocol translation and credentials; InferCrane owns stable endpoint identity, policy, evidence, and release semantics.",
			Capabilities: []Capability{
				{Name: "connect_existing", State: CapabilitySupported, Evidence: "go:test/internal/controlapi#TestDiscoverEndpointNamesLiteLLMOnlyFromEvidence"},
				{Name: "provider_translation", State: CapabilitySupported, Detail: "executed by the separately operated LiteLLM deployment", Evidence: "go:test/internal/gateway#TestQualifiedProtocolSurfacesPreservePayloads"},
				{Name: "gateway_lifecycle", State: CapabilityUnsupported, Detail: "InferCrane does not install, fork, or upgrade LiteLLM"},
			},
			Qualification: []Qualification{{State: QualificationLocal, Environment: "hermetic-openai-compatible", Evidence: "go:test/internal/controlapi#TestDiscoverEndpointNamesLiteLLMOnlyFromEvidence"}, {State: QualificationDeferred, Environment: "real-litellm", Reason: "operator-managed gateway requires target-specific protocol qualification"}},
		},
		{
			Adapter: "external-sandbox", Kind: "sandbox", ContractVersion: CompositionV1,
			Ownership: "The sandbox provider owns isolation, commands, files, snapshots, and network policy; InferCrane owns only the external reference and expiring endpoint-scoped inference credential.",
			Capabilities: []Capability{
				{Name: "external_reference", State: CapabilitySupported, Evidence: "go:test/internal/store#TestSandboxReferenceIssuesEndpointRestrictedExpiringCredential"},
				{Name: "endpoint_scoped_credential", State: CapabilitySupported, Evidence: "go:test/internal/gateway#TestEndpointRestrictedInferenceCredentialCannotEnumerateOrInvokeOtherAlias"},
				{Name: "sandbox_execution", State: CapabilityUnsupported, Detail: "execution remains in E2B, Modal, Kubernetes, or another user-managed sandbox"},
			},
			Qualification: []Qualification{{State: QualificationLocal, Environment: "postgres-and-hermetic-gateway", Evidence: "go:test/internal/store#TestSandboxReferenceIssuesEndpointRestrictedExpiringCredential"}, {State: QualificationDeferred, Environment: "real-sandbox-provider", Reason: "provider secret injection and network policy remain provider-specific"}},
		},
		{
			Adapter: "signed-artifact-handoff", Kind: "training", ContractVersion: CompositionV1,
			Ownership: "The external platform owns training data, execution, checkpoints, and scheduling; InferCrane verifies immutable provenance and connects the artifact to release qualification.",
			Capabilities: []Capability{
				{Name: "signed_lineage", State: CapabilitySupported, Evidence: "go:test/internal/trainingartifact#TestSignedHandoffRoundTripAndTamperRejection"},
				{Name: "immutable_revision_binding", State: CapabilitySupported, Evidence: "go:test/internal/store#TestTrainingHandoffIsRevisionBoundImmutableAndTenantSafe"},
				{Name: "training_execution", State: CapabilityUnsupported, Detail: "training remains in MLflow-connected pipelines, SkyPilot, Kubeflow, or another external system"},
			},
			Qualification: []Qualification{{State: QualificationLocal, Environment: "signed-fixture-and-postgres", Evidence: "go:test/internal/store#TestTrainingHandoffIsRevisionBoundImmutableAndTenantSafe"}, {State: QualificationDeferred, Environment: "real-training-platform", Reason: "registry identity and checkpoint availability require system-specific qualification"}},
		},
		{
			Adapter: "llm-compressor", Kind: CompositionArtifactBuilder, ContractVersion: CompositionV1,
			Ownership: "LLM Compressor owns quantization execution; InferCrane owns the immutable build plan, digest attestation, semantic quality evidence, candidate binding, and release decision.",
			Capabilities: []Capability{
				{Name: "quantized_checkpoint_provenance", State: CapabilitySupported, Evidence: "go:test/internal/store#TestOptimizedArtifactRequiresExactBuilderCandidateRevisionAndQualityEvidence"},
				{Name: "builder_execution", State: CapabilityUnsupported, Detail: "the control plane never imports or executes the Python builder in-process"},
			},
			Qualification: []Qualification{{State: QualificationLocal, Environment: "immutable-plan-and-attestation", Evidence: "go:test/internal/store#TestOptimizedArtifactRequiresExactBuilderCandidateRevisionAndQualityEvidence"}, {State: QualificationDeferred, Environment: "real-llm-compressor-gpu", Reason: "model-specific quantization, kernel compatibility, and semantic quality require a real builder and target GPU"}},
		},
		{
			Adapter: "modelopt", Kind: CompositionArtifactBuilder, ContractVersion: CompositionV1,
			Ownership: "NVIDIA ModelOpt owns quantization and export execution; InferCrane owns the immutable plan, artifact provenance, quality gate, and rollout evidence.",
			Capabilities: []Capability{
				{Name: "quantized_checkpoint_provenance", State: CapabilitySupported, Evidence: "go:test/internal/store#TestOptimizedArtifactRequiresExactBuilderCandidateRevisionAndQualityEvidence"},
				{Name: "builder_execution", State: CapabilityUnsupported, Detail: "execution stays in an isolated, digest-pinned external builder"},
			},
			Qualification: []Qualification{{State: QualificationLocal, Environment: "immutable-plan-and-attestation", Evidence: "go:test/internal/store#TestOptimizedArtifactRequiresExactBuilderCandidateRevisionAndQualityEvidence"}, {State: QualificationDeferred, Environment: "real-modelopt-gpu", Reason: "hardware-specific formats and exported runtime compatibility require real GPU qualification"}},
		},
		{
			Adapter: "vllm-speculators", Kind: CompositionArtifactBuilder, ContractVersion: CompositionV1,
			Ownership: "vLLM Speculators owns draft-model training and conversion; InferCrane owns verifier compatibility, immutable artifact lineage, performance and semantic qualification, and rollout.",
			Capabilities: []Capability{
				{Name: "speculator_checkpoint_provenance", State: CapabilitySupported, Evidence: "go:test/internal/store#TestOptimizedArtifactRequiresExactBuilderCandidateRevisionAndQualityEvidence"},
				{Name: "speculator_training", State: CapabilityUnsupported, Detail: "training remains an external GPU job and is never run in the control-plane process"},
			},
			Qualification: []Qualification{{State: QualificationLocal, Environment: "immutable-plan-and-attestation", Evidence: "go:test/internal/store#TestOptimizedArtifactRequiresExactBuilderCandidateRevisionAndQualityEvidence"}, {State: QualificationDeferred, Environment: "real-vllm-speculator-gpu", Reason: "verifier pairing, acceptance rate, latency, and output equivalence require model-specific GPU evidence"}},
		},
		{
			Adapter: "tensorrt-llm", Kind: CompositionArtifactBuilder, ContractVersion: CompositionV1,
			Ownership: "TensorRT-LLM owns engine build and GPU execution; InferCrane owns exact hardware constraints, immutable engine provenance, qualification, and release policy.",
			Capabilities: []Capability{
				{Name: "engine_artifact_provenance", State: CapabilitySupported, Evidence: "go:test/internal/store#TestOptimizedArtifactRequiresExactBuilderCandidateRevisionAndQualityEvidence"},
				{Name: "runtime_execution", State: CapabilityUnsupported, Detail: "no TensorRT-LLM runtime adapter is locally or GPU qualified yet"},
			},
			Qualification: []Qualification{{State: QualificationLocal, Environment: "immutable-plan-and-attestation", Evidence: "go:test/internal/store#TestOptimizedArtifactRequiresExactBuilderCandidateRevisionAndQualityEvidence"}, {State: QualificationDeferred, Environment: "real-tensorrt-llm-gpu", Reason: "engine build and execution are model, GPU architecture, CUDA, and TensorRT version specific"}},
		},
		{
			Adapter: "lmcache", Kind: CompositionCache, ContractVersion: CompositionV1,
			Ownership: "LMCache owns KV storage, eviction, and transfer; the serving backend owns cache-aware request execution; InferCrane owns the selected immutable topology and qualification evidence.",
			Capabilities: []Capability{
				{Name: "configuration_registration", State: CapabilitySupported, Evidence: "go:test/internal/optimizationcapability#TestV1CompilesOnlyQualifiedExactTuples"},
				{Name: "executable_lifecycle", State: CapabilityUnsupported, Detail: "registered combinations fail closed until an exact runtime and cache-service lifecycle are qualified"},
			},
			Qualification: []Qualification{{State: QualificationRegistered, Environment: "serving-contract"}, {State: QualificationDeferred, Environment: "real-lmcache-gpu", Reason: "cache protocol, process lifecycle, hit-rate, memory pressure, and failure behavior need real runtime evidence"}},
		},
		{
			Adapter: "llm-d", Kind: CompositionOrchestrator, ContractVersion: CompositionV1,
			Ownership: "llm-d owns Kubernetes scheduling and data-plane routing for its deployment; InferCrane may own only the outer immutable serving plan, evidence, and release decision.",
			Capabilities: []Capability{
				{Name: "outer_plan_registration", State: CapabilitySupported, Evidence: "go:test/internal/servingcontract#TestDynamoTopologyRejectsConflictingOwnershipAndCacheCombinations"},
				{Name: "mutation_ownership", State: CapabilityUnsupported, Detail: "no shared scaling or routing mutation owner is permitted"},
			},
			Qualification: []Qualification{{State: QualificationRegistered, Environment: "ownership-contract"}, {State: QualificationDeferred, Environment: "real-llm-d-kubernetes", Reason: "Gateway API routing, scheduler ownership, drain, and GPU behavior require a dedicated adapter and cluster evidence"}},
		},
		{
			Adapter: "aibrix", Kind: CompositionOrchestrator, ContractVersion: CompositionV1,
			Ownership: "AIBrix owns its Kubernetes autoscaling, routing, and cache components; InferCrane may own only an outer plan and evidence when an explicit single-writer adapter exists.",
			Capabilities: []Capability{
				{Name: "outer_plan_registration", State: CapabilitySupported, Evidence: "go:test/internal/servingcontract#TestDynamoTopologyRejectsConflictingOwnershipAndCacheCombinations"},
				{Name: "mutation_ownership", State: CapabilityUnsupported, Detail: "the current adapter intentionally refuses overlapping controllers"},
			},
			Qualification: []Qualification{{State: QualificationRegistered, Environment: "ownership-contract"}, {State: QualificationDeferred, Environment: "real-aibrix-kubernetes", Reason: "routing, autoscaling, cache and model-adapter APIs require a dedicated version-pinned cluster qualification"}},
		},
	} {
		if err = registry.RegisterComposition(profile); err != nil {
			return nil, err
		}
	}
	profiles := []ProviderProfile{
		providerBoundary("aws-asg", "aws", ElasticMode, []Capability{
			{Name: "launch_template_version", State: CapabilityUnknown, Detail: "requires an explicit immutable numbered launch-template version"},
			{Name: "intent_observation", State: CapabilityUnknown, Detail: "accepted ASG mutations must be observed to convergence"},
			{Name: "instance_refresh", State: CapabilityUnknown, Detail: "provider-owned rolling replacement; InferCrane owns desired revision evidence"},
		}),
		providerBoundary("aws-eks", "aws", ElasticMode, []Capability{
			{Name: "kubernetes_contract", State: CapabilityUnknown, Detail: "reuses the namespaced Kubernetes adapter against an explicit EKS context"},
			{Name: "workload_identity", State: CapabilityUnknown, Detail: "service-account identity must be short-lived and cluster scoped"},
		}),
		providerBoundary("aws-sagemaker", "aws", ElasticMode, []Capability{
			{Name: "import", State: CapabilityUnknown, Detail: "adopt an existing endpoint without assuming lifecycle ownership"},
			{Name: "managed_endpoint_lifecycle", State: CapabilityUnknown, Detail: "SageMaker owns endpoint instances and autoscaling children"},
			{Name: "private_network", State: CapabilityUnknown, Detail: "requires explicit VPC/private endpoint evidence"},
		}),
		providerBoundary("aws-bedrock", "aws", ExternalMode, []Capability{
			{Name: "provisioning", State: CapabilityUnsupported, Detail: "Bedrock is governed external capacity"},
			{Name: "hard_budget", State: CapabilityUnknown, Detail: "must reuse external-capacity request and cost reservations"},
			{Name: "private_network", State: CapabilityUnknown, Detail: "optional interface VPC endpoint is customer configured"},
		}),
		providerBoundary("gcp-compute", "gcp", ElasticMode, []Capability{
			{Name: "idempotent_request_id", State: CapabilityUnknown, Detail: "mutations require durable request identity and label adoption"},
			{Name: "private_network", State: CapabilityUnknown, Detail: "workers require no external IP and explicit subnet"},
			{Name: "workload_identity", State: CapabilityUnknown, Detail: "attached service account without stored key material"},
		}),
		providerBoundary("gcp-mig", "gcp", ElasticMode, []Capability{
			{Name: "immutable_instance_template", State: CapabilityUnknown},
			{Name: "intent_observation", State: CapabilityUnknown, Detail: "declarative update acceptance is not convergence"},
			{Name: "rolling_update", State: CapabilityUnknown, Detail: "MIG owns replacement; rollback is a new desired template"},
		}),
		providerBoundary("gcp-gke", "gcp", ElasticMode, []Capability{
			{Name: "kubernetes_contract", State: CapabilityUnknown, Detail: "reuses the namespaced Kubernetes adapter against an explicit GKE context"},
			{Name: "workload_identity", State: CapabilityUnknown, Detail: "Kubernetes service account maps to short-lived Google identity"},
		}),
		providerBoundary("gcp-vertex", "gcp", ElasticMode, []Capability{
			{Name: "import", State: CapabilityUnknown},
			{Name: "managed_endpoint_lifecycle", State: CapabilityUnknown, Detail: "Vertex owns deployed model replicas"},
			{Name: "private_service_connect", State: CapabilityUnknown, Detail: "endpoint type limitations remain provider-owned"},
		}),
		providerBoundary("coreweave-cks", "coreweave", ElasticMode, []Capability{
			{Name: "kubernetes_contract", State: CapabilityUnknown, Detail: "CKS-first profile reuses namespaced Kubernetes lifecycle"},
			{Name: "provider_managed_gpu_operator", State: CapabilityUnsupported, Detail: "InferCrane must not install or own the NVIDIA GPU Operator"},
			{Name: "private_vpc", State: CapabilityUnknown},
		}),
	}
	for _, profile := range profiles {
		if profile.Adapter == "gcp-compute" {
			profile.AdapterVersion = "builtin-v1.5"
			profile.Capabilities = []Capability{{Name: "adoption", State: CapabilitySupported, Evidence: "go:test/internal/provision#TestGCPComputeLifecycleIsPrivateIdempotentAndAdoptable"}, {Name: "idempotent_delete", State: CapabilitySupported, Evidence: "go:test/internal/provision#TestGCPComputeLifecycleIsPrivateIdempotentAndAdoptable"}, {Name: "immutable_workload", State: CapabilitySupported, Evidence: "go:test/internal/provision#TestGCPComputeRejectsMutableContainerBeforeProviderCall"}, {Name: "orphan_inventory", State: CapabilitySupported, Evidence: "go:test/internal/provision#TestGCPComputeLifecycleIsPrivateIdempotentAndAdoptable"}, {Name: "private_network", State: CapabilitySupported, Evidence: "go:test/internal/provision#TestGCPComputeLifecycleIsPrivateIdempotentAndAdoptable"}, {Name: "workload_identity", State: CapabilitySupported, Detail: "attached service account; no static service-account key", Evidence: "go:test/internal/provision#TestGCPComputeLifecycleIsPrivateIdempotentAndAdoptable"}}
			profile.Qualification = []Qualification{{State: QualificationLocal, Environment: "hermetic-gcloud", Evidence: "go:test/internal/provision#TestGCPComputeLifecycleIsPrivateIdempotentAndAdoptable"}, {State: QualificationDeferred, Environment: "real-gcp-compute", Reason: "awaiting consolidated manual qualification"}}
		}
		if err = registry.RegisterProvider(profile); err != nil {
			return nil, err
		}
	}
	dynamo := ProviderProfile{
		Adapter: "kubernetes-dynamo", Cloud: "kubernetes", ContractVersion: ProviderContractV1, AdapterVersion: "builtin-v2.0", Modes: []ComputeMode{ElasticMode},
		Capabilities: []Capability{
			{Name: "dgd_parent_lifecycle", State: CapabilitySupported, Detail: "InferCrane owns one DynamoGraphDeployment; the Dynamo operator owns children", Evidence: "go:test/internal/provision#TestKubernetesDynamoLifecycleIsReplaySafeAndOwnsOneParent"},
			{Name: "lost_response_adoption", State: CapabilitySupported, Evidence: "go:test/internal/provision#TestKubernetesDynamoAdoptsLostApplyResponseAndRejectsStaleReadiness"},
			{Name: "aggregated_serving", State: CapabilitySupported, Evidence: "go:test/internal/provision#TestKubernetesDynamoManifestMakesTopologyAndSecretsExplicit"},
			{Name: "disaggregated_serving", State: CapabilitySupported, Detail: "manifest and ownership contract are locally qualified; GPU performance is not", Evidence: "go:test/internal/provision#TestKubernetesDynamoDisaggregatedVLLMAndSGLangAreExplicit"},
			{Name: "kv_aware_routing", State: CapabilitySupported, Evidence: "go:test/internal/provision#TestKubernetesDynamoManifestMakesTopologyAndSecretsExplicit"},
			{Name: "kvbm_cache", State: CapabilitySupported, Detail: "aggregated vLLM only until additional combinations are qualified", Evidence: "go:test/internal/provision#TestKubernetesDynamoManifestMakesTopologyAndSecretsExplicit"},
			{Name: "dynamo_planner_autoscaling", State: CapabilityUnsupported, Detail: "registered in the serving contract but fails closed until DGDSA ownership is qualified"},
			{Name: "lmcache", State: CapabilityUnsupported, Detail: "registered in the serving contract but not emitted by this adapter"},
			{Name: "hicache", State: CapabilityUnsupported, Detail: "registered in the serving contract but not emitted by this adapter"},
		},
		Qualification: []Qualification{
			{State: QualificationLocal, Environment: "hermetic-kubectl", Evidence: "go:test/internal/provision#TestKubernetesDynamoLifecycleIsReplaySafeAndOwnsOneParent"},
			{State: QualificationDeferred, Environment: "real-dynamo-gpu-kubernetes", Reason: "Dynamo operator, GPU runtime, NIXL, cache, and performance semantics require real infrastructure"},
		},
	}
	if err = registry.RegisterProvider(dynamo); err != nil {
		return nil, err
	}
	compatibility := append([]RuntimeCompatibility(nil), registry.Snapshot().Compatibility...)
	compatibility = append(compatibility,
		RuntimeCompatibility{Runtime: "vllm", Adapter: "gcp-compute", Cloud: "gcp", Mode: ElasticMode, State: QualificationLocal, Evidence: "go:test/internal/provision#TestGCPComputeLifecycleIsPrivateIdempotentAndAdoptable"},
		RuntimeCompatibility{Runtime: "sglang", Adapter: "gcp-compute", Cloud: "gcp", Mode: ElasticMode, State: QualificationSimulated, Evidence: "go:test/internal/provision#TestGCPComputePortableWorkloadExpandsArgumentsSafely"},
		RuntimeCompatibility{Runtime: "custom-oci", Adapter: "gcp-compute", Cloud: "gcp", Mode: ElasticMode, State: QualificationSimulated, Evidence: "go:test/internal/provision#TestGCPComputePortableWorkloadExpandsArgumentsSafely"},
		RuntimeCompatibility{Runtime: "vllm", Adapter: "kubernetes-dynamo", Cloud: "kubernetes", Mode: ElasticMode, State: QualificationSimulated, Evidence: "go:test/internal/provision#TestKubernetesDynamoDisaggregatedVLLMAndSGLangAreExplicit"},
		RuntimeCompatibility{Runtime: "sglang", Adapter: "kubernetes-dynamo", Cloud: "kubernetes", Mode: ElasticMode, State: QualificationSimulated, Evidence: "go:test/internal/provision#TestKubernetesDynamoDisaggregatedVLLMAndSGLangAreExplicit"},
	)
	for _, profile := range profiles {
		if profile.Adapter == "gcp-compute" || profile.Adapter == "aws-bedrock" {
			continue
		}
		compatibility = append(compatibility, RuntimeCompatibility{Runtime: "vllm", Adapter: profile.Adapter, Cloud: profile.Cloud, Mode: ElasticMode, State: QualificationDeferred, Reason: "registered provider boundary requires executable local conformance and real manual qualification"})
	}
	if err = registry.SetCompatibility(compatibility...); err != nil {
		return nil, err
	}
	return registry, nil
}

func providerBoundary(adapter, cloud string, mode ComputeMode, capabilities []Capability) ProviderProfile {
	return ProviderProfile{Adapter: adapter, Cloud: cloud, ContractVersion: ProviderContractV1, AdapterVersion: "boundary-v1.5", Modes: []ComputeMode{mode}, Capabilities: capabilities, Qualification: []Qualification{{State: QualificationRegistered, Environment: "contract-boundary"}, {State: QualificationDeferred, Environment: "real-" + adapter, Reason: "awaiting executable local adapter and consolidated manual qualification"}}}
}

// V1Catalog is the current public v1 integration catalog. Historical catalog
// functions remain stable so prior release evidence is reproducible.
func V1Catalog() (*Registry, error) { return V15Catalog() }
