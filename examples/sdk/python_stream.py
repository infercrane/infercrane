import os

from infercrane import InferCrane

client = InferCrane(
    api_key=os.environ["INFERCRANE_API_KEY"],
    base_url=os.environ.get("INFERCRANE_CONTROL_URL", "http://127.0.0.1:18000"),
)

events = list(client.stream_chat(
    os.environ.get("INFERCRANE_DEPLOYMENT", "qwen-prod"),
    [{"role": "user", "content": "SDK smoke test"}],
))
if not events:
    raise RuntimeError("stream returned no events")

print("Python SDK stream completed")
