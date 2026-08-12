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
			{Name: "completions", State: CapabilitySupported, Evidence: "go:test/internal/gateway#TestQualifiedProtocolSurfacesPreservePayloads"},
			{Name: "embeddings", State: CapabilitySupported, Detail: "requires an embedding-capable model", Evidence: "go:test/internal/gateway#TestQualifiedProtocolSurfacesPreservePayloads"},
			{Name: "responses", State: CapabilityUnsupported, Detail: "the pinned vLLM 0.8.5.post1 runtime predates the Responses API; use an explicitly qualified runtime profile"},
			{Name: "chat_batch", State: CapabilityUnsupported, Detail: "the pinned vLLM 0.8.5.post1 runtime does not qualify the online chat batch API"},
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
	compatibility := append([]RuntimeCompatibility(nil), registry.Snapshot().Compatibility...)
	compatibility = append(compatibility,
		RuntimeCompatibility{Runtime: "vllm", Adapter: "gcp-compute", Cloud: "gcp", Mode: ElasticMode, State: QualificationLocal, Evidence: "go:test/internal/provision#TestGCPComputeLifecycleIsPrivateIdempotentAndAdoptable"},
		RuntimeCompatibility{Runtime: "sglang", Adapter: "gcp-compute", Cloud: "gcp", Mode: ElasticMode, State: QualificationSimulated, Evidence: "go:test/internal/provision#TestGCPComputePortableWorkloadExpandsArgumentsSafely"},
		RuntimeCompatibility{Runtime: "custom-oci", Adapter: "gcp-compute", Cloud: "gcp", Mode: ElasticMode, State: QualificationSimulated, Evidence: "go:test/internal/provision#TestGCPComputePortableWorkloadExpandsArgumentsSafely"},
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
