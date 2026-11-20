---
title: Model artifacts
description: Serve a public, private, or self-trained model, weights from your own bucket, or a LoRA adapter, each pinned to immutable identity.
---

# Model artifacts

Every deployment revision pins its model before anything is provisioned. Retries reuse the persisted
artifact; they never resolve a mutable reference again.

| Source | Pinned by | Who reads the files | Providers |
|---|---|---|---|
| Public Hugging Face repository | commit | the runtime, without a token | every provider |
| Private or gated Hugging Face repository | commit | the runtime, with the workspace's token delivered by the provider | `runpod-pods`, `kubernetes`, `aws-ec2` |
| Weights in your own S3 or GCS bucket | sha256 manifest digest | the worker, with its own cloud identity | `kubernetes` (S3, GCS), `aws-ec2` (S3) |
| LoRA adapter on a base model | adapter commit or manifest digest | as for its source | `runpod-pods` (Hugging Face), `kubernetes`, `aws-ec2` |

Any other combination is refused before provisioning with `provider_configuration_invalid`; nothing
is created.

## Hugging Face repositories

InferCrane resolves a repository and requested revision through `huggingface_hub.HfApi.model_info`.
The commit is attached once to the immutable deployment revision and passed to vLLM with
`--revision`. Persisted evidence includes the repository, requested reference, commit, canonical
model identity, approximate Hub storage when returned, and grounded library and pipeline metadata.
Cache state is `unknown` unless the execution backend can measure the worker cache. The production
image installs `huggingface_hub` and `hf_xet` in an isolated virtual environment so their versions
cannot alter SkyPilot's environment.

### Private and gated models

Save the token once as a sealed workspace secret. It is encrypted with the control plane's credential
key, bound to the workspace and name, and never shown again:

```bash
export HF_TOKEN='hf_...'   # a fine-grained read token
infercrane secret create hf-token --sealed-from-env HF_TOKEN
infercrane deploy acme/support-model --model-secret SECRET_ID --compute-connection my-runpod --gpu H100
```

Pass the secret's ID as `model_secret_reference_id` in the API, `--model-secret` in `deploy`, or
`model.secret_reference_id` in a DeploymentSpec. The control plane uses the token to pin the commit
and read `config.json` ([the automatic check](/features/model-catalog#check-any-model-automatically)),
and each provider delivers it to the worker natively:

- **RunPod Pods.** InferCrane stores the value as a RunPod secret named `infercrane_model_<hash>` in
  the account that runs the Pod: your own account when you deploy through a compute connection. The
  Pod's environment holds only `HF_TOKEN={{ RUNPOD_SECRET_infercrane_model_<hash> }}`, so the value
  never appears in the Pod specification or in RunPod's Pod API. InferCrane keeps the RunPod secret's
  value current when it creates a Pod. Deleting the InferCrane secret does not delete the RunPod
  secret; remove it in the RunPod console.
- **Kubernetes.** Keep the token in a Secret in the InferCrane namespace and name it with a
  `kubernetes` reference: `infercrane secret create hf-token --kubernetes hf-token/HF_TOKEN`. The
  runtime gets `HF_TOKEN` through a `secretKeyRef`; the control plane never reads the value and needs
  no Secret permissions.
- **AWS EC2.** Keep the token in AWS Secrets Manager and name its ARN with an `aws-secrets-manager`
  reference: `infercrane secret create hf-token --aws-secret arn:aws:secretsmanager:...`. The
  instance profile reads it at boot, like the worker key, and passes it to the runtime through the
  inherited environment, never the docker command line.

Kubernetes and AWS references are for workspaces the operator lets deploy on its own cluster or AWS
account. The control plane cannot read them, so the automatic check reports access as unknown, and a
deployment must pin `model_revision` to the full 40-character commit.

The token reaches the runtime only as `HF_TOKEN`, which `huggingface_hub` sends only to
`huggingface.co`: InferCrane never sets `HF_ENDPOINT`, and `runtime_env` accepts only reviewed engine
switches. SkyPilot, GCP Compute, RunPod Serverless, and the Kubernetes Dynamo adapter refuse a model
credential.

## Weights in your own bucket

InferCrane never reads your bucket. You import a file manifest: the path, size, and sha256 of every
file. Its digest pins the deployment, and each worker downloads exactly those files with its own
cloud identity and checks every sha256 before the server starts.

```bash
aws s3 sync s3://acme-models/run-42 ./run-42   # or the directory you uploaded from
infercrane models import s3://acme-models/run-42 --from ./run-42
infercrane deploy s3://acme-models/run-42 --model-revision sha256:... --cloud kubernetes --gpu L4 --name run-42
```

`models import` hashes the local copy and sends the manifest with `config.json`
(`POST /api/v1/model-imports`). The import says what it proved and what stays unknown:

| Verified | Unknown |
|---|---|
| The manifest digest over every file and the total size | Whether the bucket holds these exact bytes: each worker checks before serving |
| `config.json` matches its manifest entry, and its architecture | Throughput, latency, quality, and price |
| Tokenizer files and safetensors weights are listed | |
| Whether the pinned vLLM or SGLang registers the architecture | |

An import without `config.json`, a tokenizer file, or safetensors weights is refused with
`model_import_failed` and its checks. Without the `config.json` bytes, the architecture stays unknown.

How a worker reads the files:

- **Kubernetes.** An init container (`fetch-model`, the pinned AWS CLI or Google Cloud CLI image)
  downloads the listed files into the Pod's own volume and runs `sha256sum -c`. It uses the service
  account's workload identity (IRSA, GKE Workload Identity) or, when the operator sets
  `INFERCRANE_KUBERNETES_OBJECT_STORE_SECRET`, that Secret's keys, for example `AWS_ACCESS_KEY_ID`,
  `AWS_SECRET_ACCESS_KEY`, and `AWS_ENDPOINT_URL` for an S3-compatible store such as MinIO. The
  serving container never sees those keys.
- **AWS EC2.** The instance profile runs `aws s3 cp` on the host and `sha256sum -c`, then mounts the
  files into the runtime. It needs `s3:GetObject` on the prefix. GCS files need Kubernetes.

The runtime serves the verified local copy and keeps answering to the location you deployed, for
example `--served-model-name s3://acme-models/run-42`. One replica reads one object store. Streaming
loaders are not used, because they cannot check the pinned digest before serving.

## LoRA adapters

Serve an adapter on a base model with the `vllm` or `sglang` runtime. The endpoint answers with the
adapter's name; one endpoint serves one adapter.

```bash
infercrane models adapter-check acme/support-lora --base-model Qwen/Qwen3-8B
infercrane deploy Qwen/Qwen3-8B --adapter support-bot=acme/support-lora@COMMIT --compute-connection my-runpod --gpu H100
```

```yaml
model:
  id: Qwen/Qwen3-8B
  revision: 0123456789abcdef0123456789abcdef01234567
  adapters:
    - name: support-bot
      repository: acme/support-lora        # or s3://acme-adapters/support
      revision: abcdefabcdefabcdefabcdefabcdefabcdefabcd  # or sha256:... from its import
      rank: 16
```

The adapter check pins the commit and reads `adapter_config.json`: the LoRA rank and the declared base
model, compared with the one you serve it on. A declared local training path cannot be compared and
stays unknown. Import a bucket adapter with `infercrane models import s3://... --from DIR --kind
adapter --base-model OWNER/NAME`.

A Hugging Face adapter is downloaded at its pinned commit before the server starts; a bucket adapter
is fetched and checked like weights. InferCrane renders the reviewed flags of the pinned runtimes:
vLLM 0.22.1 gets `--enable-lora --max-loras 1 --max-lora-rank R --lora-modules NAME=PATH`, with the
rank rounded up to a rank vLLM accepts; SGLang 0.5.12 gets `--enable-lora --max-loras-per-batch 1
--max-lora-rank R --lora-paths NAME=PATH`. `runtime_args` may not carry LoRA flags.

## Optimization runs

A proposal's `model_secret_reference_id` and `model_adapters` reach every candidate, so each setup
serves the same private model or adapter; the token never enters the proposal. Runs that Stay
optimized proposes for an endpoint carry its running revision's credential and adapter the same way. Imported weights are
pinned by their manifest digest as `model_revision`. Candidates that swap in another checkpoint are
skipped when an adapter is served, because the adapter was trained on this one.
