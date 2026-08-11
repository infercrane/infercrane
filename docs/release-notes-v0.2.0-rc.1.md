# InferCrane v0.2.0-rc.1 engineering checkpoint

This local release-candidate checkpoint establishes versioned integration contracts. It is not a
published stable release and does not claim new real-provider qualification.

## Added

- Provider Contract V1 and Runtime Contract V1 capability profiles
- authenticated `GET /api/v1/integrations` inventory
- `infercrane integrations` human and JSON inspection
- evidence-backed capability claims with drift detection
- reusable elastic, serverless, lost-response, and runtime-readiness conformance scenarios
- deterministic lost-response, timeout, delete-recovery, and redaction fault fixtures
- validated runtime-profile bindings in provisioning and reconciliation
- commit-addressed, sanitized local contract qualification manifests
- repository-native v0.2-to-v1 roadmap and release gates

## Compatibility

The public v0.1 support matrix remains vLLM on RunPod. Existing deployment, lifecycle, API and
gateway behavior is preserved. Provider workflow composition now rejects missing or mismatched V1
profiles before mutation.

## Deferred evidence

RunPod elastic, RunPod Serverless, and real-vLLM qualification remain deferred until the
consolidated v1 manual acceptance. Registration and local conformance do not imply that those gates
passed for this checkpoint.
