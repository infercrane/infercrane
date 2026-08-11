# InferCrane Python SDK

The SDK submits durable operations through the InferCrane control-plane API. A local timeout or
process exit does not cancel server-side work.

```python
from infercrane import InferCrane

client = InferCrane(api_key="...", base_url="http://127.0.0.1:18000")
operation = client.deploy(
    model="Qwen/Qwen3-8B",
    name="qwen-prod",
    cloud="runpod",
    gpu="L40S",
)
ready = client.wait(operation.id, timeout=900)

client.set_slo_policy("qwen-prod", max_ttft_p95_ms=250)
recommendation = client.recommend("qwen-prod")
```

See the [SDK guide](https://infercrane.mintlify.site/integrations/python) for async operations and
streaming examples. Run `python -m unittest discover -s tests` from this directory for hermetic
tests.
