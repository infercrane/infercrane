# Qwen3.8 H100 provisional RunPod package

This directory is deployable input for an exact RunPod qualification. It is
**not a production recipe**, is **not `recipe-v1`**, and must not be published
to the InferCrane catalog or receive customer traffic yet.

The candidate passed 3,000/3,000 requests on a Modal H100. That evidence does
not qualify RunPod, its load balancer, storage, cold starts, autoscaler, cost,
or reliability. `manifest.json` is the machine-readable contract and keeps
`production_publication_allowed` false.

## Pinned candidate

- Model: `Qwen/Qwen3.8-27B-FP8`
- Revision: `017b9c7af6b5689d5dd426a76e0bc077eb5ca20a`
- Runtime: SGLang `0.5.18`, commit `71de97b264b04dcd514cf904003028aefe9775c8`
- Base image: `lmsysorg/sglang@sha256:9e148f5ac788e856a06166bd6347a831831eb9fcfab4d1770874823a7c29a1a1`
- Platform: `linux/amd64`
- GPU: exactly one NVIDIA H100 80GB HBM3, TP1
- Minimum host envelope: 8 vCPU and 64 GiB RAM
- Model context: 18,432 tokens
- Qualified output maximum: 512 tokens

The serving arguments are fixed in `serve.sh`. The script rejects container
arguments and changes to its five contract environment variables. A RunPod
template must not override the Docker entrypoint or start command.

## Build and pin the derived image

Choose a private registry reference, then build and push from the repository
root. A tag is only a build handle; deployment must use the resulting digest.

```bash
docker buildx build \
  --platform linux/amd64 \
  --tag REGISTRY/OWNER/qwen38-h100-current-qualified:provisional \
  --push \
  deploy/runpod/qwen38-h100-current-qualified

docker buildx imagetools inspect \
  REGISTRY/OWNER/qwen38-h100-current-qualified:provisional
```

Record the derived `linux/amd64` OCI digest in the RunPod qualification receipt
that references this package manifest and checksum. The image cannot embed its
own digest without changing that digest. Do not substitute a mutable tag in the
template.

## Create the RunPod qualification endpoint

Create a **Serverless Load Balancer** endpoint, not a queue endpoint. Apply the
following exact settings:

| Setting | Required value |
|---|---|
| Image | Derived image by immutable OCI digest |
| Docker entrypoint | No override |
| Docker start command | No override |
| GPU | 1 × NVIDIA H100 80GB HBM3 |
| Container disk | 100 GiB |
| Network volume | Attached, at least 50 GiB |
| Application port | `30000` |
| Health port | `30001` |
| Health path | `/ping` |
| Minimum workers | `1` |
| Maximum workers | `1` during qualification |
| Startup/readiness allowance | At least 1,200 seconds |

RunPod mounts Serverless network volumes at `/runpod-volume`. The image pins
`/model-cache` as a symlink to that mount so the qualified Hugging Face cache
layout remains stable. Startup fails if `/runpod-volume` is not a real mount,
preventing an accidental download into ephemeral container storage.

Set these non-secret template variables exactly:

```text
HF_HOME=/model-cache
HF_HUB_CACHE=/model-cache/hub
PORT=30000
PORT_HEALTH=30001
HEALTH_CHECK_PATH=/ping
```

The model repository is public. If a Hugging Face token becomes necessary,
inject `HF_TOKEN` with RunPod's secret mechanism. Never put credentials in the
image, manifest, command, or logs.

## Health behavior

SGLang returns HTTP 503 while model loading, JIT compilation, and CUDA graph
capture are incomplete. `health_shim.py` listens separately on port 30001 and
maps that state to the RunPod Load Balancer contract:

- `GET /ping` → HTTP 204 while SGLang is unavailable or not ready.
- `GET /ping` → HTTP 200 only after SGLang `GET /health` returns 200.
- Any other health-shim path → HTTP 404.

The shell remains PID 1 and supervises both processes. If either the model
server or health shim exits, the other is terminated and the worker exits.

## Qualification before publication

Keep the endpoint isolated from customer routes. Complete every RunPod gate in
`manifest.json`, including:

1. Verify the deployed image digest, H100 identity, driver, and CUDA support.
2. Verify persistent-cache behavior and measure both cold-cache and warm-cache
   startup. The existing 345–439 second startup evidence reused Modal's cache;
   cold download is unmeasured.
3. Verify the 204-to-200 health transition and that no request reaches SGLang
   before readiness.
4. Verify `/v1/models`, buffered chat, streaming usage and `[DONE]`, structured
   output, tool calls, cancellation, timeouts, and error mapping.
5. Reproduce the immutable `serverless-primary` workload digest
   `60380d21475dbaaf7ebe3e544f518854ad67707f03b330f8ec55d3af4b9586b2`
   on RunPod at concurrency 1, 4, and 8.
6. Measure RunPod cost and reconcile it to provider invoices.
7. Verify worker loss and recovery. Test autoscaling and scale-to-zero as
   separate, unqualified configurations before changing worker bounds.

Only after evidence is reviewed should a separate change bind the target,
publish rates and entitlements, or start a zero-percent/canary route. This
package intentionally performs none of those actions.

Provider contract references: [RunPod Load Balancer workers](https://docs.runpod.io/serverless/load-balancing/build-a-worker)
and [RunPod Serverless network volumes](https://docs.runpod.io/serverless/storage/network-volumes).

## Local checks

The checks require no GPU and do not build the image:

```bash
python3 -m unittest discover \
  -s deploy/runpod/qwen38-h100-current-qualified \
  -p 'test_*.py'
(cd deploy/runpod/qwen38-h100-current-qualified && sha256sum -c SHA256SUMS)
```
