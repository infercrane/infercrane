# InferCrane Python SDK

Typed Python client for the InferCrane control API and OpenAI-compatible inference gateway. The
package has no runtime dependencies and supports Python 3.10 or newer.

## Install

For the public beta:

```bash
python -m pip install 'infercrane==1.0.0rc1'
```

Use the SDK version from the same InferCrane release as the control plane. Stable `1.0.0` remains
reserved for the stable release.

## Configure

```bash
export INFERCRANE_CONTROL_URL=https://infercrane.internal
export INFERCRANE_API_KEY=YOUR_CONTROL_API_KEY
```

```python
from infercrane import InferCrane

infercrane = InferCrane()
```

Pass `base_url`, `gateway_url`, or `api_key` to the constructor when environment configuration is not
appropriate. Use `ca_file`, `cert_file`, and `key_file` for a private CA or mutual TLS. Keep
control-plane credentials in trusted server processes.

## Deploy and wait

```python
operation = infercrane.deploy(
    model="Qwen/Qwen3-8B",
    name="support-production",
    endpoint_name="support-production",
    cloud="runpod",
    gpu="L40S",
    min_replicas=1,
    max_replicas=2,
)

completed = infercrane.wait(operation.id, timeout=900)
print(completed.status)
print(completed.result.get("endpoint_name"))
```

`name` identifies the deployment operation. `endpoint_name` is the stable model alias used by the
application and defaults to `name`.

`wait` observes durable server-side work. A timeout or process exit does not cancel the operation.
Call `cancel(operation.id)` only when cancellation is intentional.

## Stream inference

```python
for event in infercrane.stream_chat(
    "support-production",
    [{"role": "user", "content": "Summarize the incident."}],
):
    print(event, flush=True)
```

`AsyncInferCrane` provides the same common workflows for async applications.

## Set an objective and request evidence

```python
infercrane.set_slo_policy(
    "support-production",
    max_ttft_p95_ms=250,
    max_error_rate=0.01,
)

recommendation = infercrane.recommend("support-production")
```

Recommendations preserve unavailable evidence instead of inventing a metric or price.

## Errors and full API access

```python
from infercrane import APIError, OperationFailed, OperationTimeout

try:
    infercrane.wait(operation.id, timeout=30)
except OperationTimeout:
    print("The operation is still running.")
except OperationFailed as error:
    print(error.operation)
except APIError as error:
    print(error.code, error.remediation)
```

The ergonomic methods cover common deployment and inference workflows. `infercrane.api` exposes the
complete generated control API with the same authentication and error contract.

See the [Python SDK guide](https://docs.infercrane.com/integrations/python),
[control API](https://docs.infercrane.com/control-api), and
[security guidance](https://docs.infercrane.com/security).

## License

[Apache-2.0](LICENSE)
