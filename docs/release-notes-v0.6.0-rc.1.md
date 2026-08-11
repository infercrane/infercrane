# v0.6.0-rc.1

InferCrane v0.6 introduces the portable runtime workload contract.

- Custom OCI revisions persist an immutable image digest, argv, OpenAI protocol, standard probe
  paths, cancellation/drain semantics and bounded shutdown grace.
- SGLang v0.5.12 is the second registered engine profile, pinned by OCI manifest digest.
- Provider launch translation preserves argv boundaries and rejects mutable images before mutation.
- The authenticated integrations API and CLI expose exact runtime/provider/mode evidence states.
- DeploymentSpec, OpenAPI, Python, TypeScript and Terraform runtime selection are updated together.

SGLang and custom OCI GPU qualification remain deferred until the consolidated v1 manual gate.
This release candidate makes no real-GPU performance or broad model-compatibility claim.
