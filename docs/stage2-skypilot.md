# Stage 2: SkyPilot automatic deployment

Stage 2 adds an optional SkyPilot provisioner. InferCrane's public state remains provider
neutral; SkyPilot cluster IDs and generated task details are retained only as inspectable
adapter metadata.

```bash
go build -o ./bin/infercrane ./cmd/infercrane
export PATH="$PWD/bin:$PATH"
# Install the supported SkyPilot and vLLM Router CLIs separately.
sky check
export INFERCRANE_API_KEY='<strong-random-value>'

infercrane deploy Qwen/Qwen3-8B \
  --cloud runpod \
  --gpu A40 \
  --name qwen-prod

infercrane inspect qwen-prod
infercrane serve
```

Declarative form:

```bash
infercrane deploy examples/infercrane.yaml
```

The adapter invokes the supported SkyPilot CLI (`launch`, `status --endpoint`, and `down`)
behind a narrow Go process boundary. The cluster name is deterministic, so repeating the
same deployment does not intentionally create a random duplicate cluster.

SkyPilot is an external operator dependency rather than an InferCrane language dependency.
Pin its version in deployment automation and validate it during release qualification.

The vLLM worker API key is passed using SkyPilot's secret environment support. The secret is
not persisted in deployment records or generated task metadata. Worker ports may be publicly
reachable depending on provider networking; use a strong key and provider firewall/private
networking where available.

`infercrane delete` tears down InferCrane-owned SkyPilot clusters before deleting state. If
cleanup fails, state is retained so the operation can be retried. `--keep-resources` is an
explicit escape hatch.

Scaling fields are accepted in deployment YAML for schema continuity, but Stage 2 does not
claim autoscaling. Autoscaling begins in Stage 3.
