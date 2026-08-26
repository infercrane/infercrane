# InferCrane v1.0.0

`v1.0.0` is the first public release of InferCrane, an open-source inference infrastructure layer
for teams deploying open-weight models or adopting inference they already run.

Applications use one stable OpenAI-compatible endpoint while InferCrane operates changes beneath
it. The release includes:

- model-to-endpoint workload initialization, validation, planning, deployment, and durable operation
  inspection;
- observe-only and traffic-managed adoption of existing OpenAI-compatible inference;
- versioned serving plans for supported vLLM, SGLang, custom OCI, Kubernetes, and cloud adapter
  boundaries;
- bounded admission, explicit overload responses, immutable request-path route snapshots, and
  request evidence that excludes prompt and output content by default;
- monitoring, Doctor findings, Request Inspector, sourced cost evidence, capacity recommendations,
  and guarded human-approved actions;
- benchmark, replay, quality, reliability, and cost evidence for candidate comparison, Release Guard,
  promotion, post-promotion monitoring, and rollback;
- artifact-cache intent and observation, curated runtime recipes, optimization campaigns, and
  qualified serving-plan activation;
- CLI, control API, OpenAI-compatible gateway, Python and TypeScript SDKs, Terraform provider,
  GitHub Action, terminal workspace, and a separately deployed private-preview console;
- tenant isolation, durable leases, migration safety, backup and restore controls, signed webhooks,
  secret redaction, and explicit provider ownership boundaries.

Start a model project:

```bash
infercrane workload init my-model --model Qwen/Qwen3-8B
cd my-model
infercrane workload validate
infercrane workload plan
infercrane workload deploy
```

For custom OCI projects, build and deploy remain separate mutation boundaries. Local images are not
treated as deployable artifacts. A deployed workload must resolve to an immutable registry digest.
Runtime-managed model projects use their supported adapter and do not require InferCrane to rebuild
the serving engine.

Release Guard can require signed aggregate quality evidence from an external evaluator. Evidence is
bound to one deployment revision, evaluator version, suite version, artifact digest, and sample
count. InferCrane compares only compatible evidence and makes the threshold decision
deterministically. It does not inspect prompts or ask an LLM to decide promotion.

## Qualification boundary

Software version and infrastructure qualification are separate. Local, simulated-cloud, package,
migration, security, Docker, Kind, and KWOK gates are automated. AWS has archived real-GPU evidence
for exact model, runtime, image, accelerator, region, and workload tuples. GCP GPU and real GPU
Kubernetes/KServe qualification remain pending. No benchmark or compatibility result is generalized
beyond the evidence that produced it.

Review [compatibility and qualification](https://docs.infercrane.com/compatibility),
[feature qualification matrix](https://docs.infercrane.com/testing/feature-qualification-matrix),
and the evidence linked for the exact production combination before relying on it.
