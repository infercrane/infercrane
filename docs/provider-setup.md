# Provider setup

Provider integrations are registered control-plane adapters. Each provider owns its credentials, infrastructure semantics, and billable resources; InferCrane owns durable intent, reconciliation, and cleanup. A provider is supported only after its adapter combination appears in the release qualification matrix.

## RunPod — first v0.1 qualification target

Set a scoped `RUNPOD_API_KEY` on the control plane and configure SkyPilot's RunPod credentials for elastic workers. Run `infercrane doctor --cloud` before provisioning.

For Serverless, create a RunPod vLLM template with `MODEL_NAME`, immutable `MODEL_REVISION`, and `RAW_OPENAI_OUTPUT=1`, then set `INFERCRANE_RUNPOD_SERVERLESS_TEMPLATE_ID`. `infercrane doctor --serverless` reads and validates the template without creating an endpoint.

Set `INFERCRANE_URL` to a URL reachable from AIPerf and clients. Keep provider and InferCrane credentials out of specifications, logs, issue reports, and benchmark artifacts. Always inspect existing pods/endpoints before retrying manual acceptance and delete paid resources after the test.
