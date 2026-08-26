# InferCrane TypeScript SDK

Typed Node.js client for the InferCrane control API and OpenAI-compatible inference gateway.

## Install

```bash
npm install @infercrane/sdk
```

Node.js 20 or newer is required.

## Configure

```bash
export INFERCRANE_CONTROL_URL=https://infercrane.internal
export INFERCRANE_API_KEY=YOUR_CONTROL_API_KEY
```

```ts
import { InferCrane } from '@infercrane/sdk';

const infercrane = new InferCrane();
```

Pass `baseUrl`, `gatewayUrl`, or `apiKey` to the constructor when environment configuration is not
appropriate. Keep control-plane credentials in trusted server processes, not browser bundles.

## Deploy and wait

```ts
const operation = await infercrane.deploy({
  model: 'Qwen/Qwen3-8B',
  name: 'support-production',
  endpointName: 'support-production',
  cloud: 'runpod',
  gpu: 'L40S',
  minReplicas: 1,
  maxReplicas: 2,
});

const completed = await infercrane.wait(operation.id, {timeoutMs: 900_000});
console.log(completed.status);
console.log(completed.result?.endpoint_name);
```

`name` identifies the deployment operation. `endpointName` is the stable model alias used by the
application and defaults to `name`.

`wait` is client-side observation. A timeout, abort signal, or process exit does not cancel the
durable server-side operation. Call `cancel(operation.id)` only when cancellation is intentional.

## Stream inference

```ts
for await (const event of infercrane.streamChat('support-production', [
  {role: 'user', content: 'Summarize the incident.'},
])) {
  console.log(event);
}
```

## Set an objective and request evidence

```ts
await infercrane.setSloPolicy('support-production', {
  max_ttft_p95_ms: 250,
  max_error_rate: 0.01,
});

const recommendation = await infercrane.recommend('support-production');
```

Recommendations preserve unavailable evidence instead of inventing a metric or price.

## Errors and full API access

```ts
import {ApiError, OperationFailed, OperationTimeout} from '@infercrane/sdk';

try {
  await infercrane.wait(operation.id, {timeoutMs: 30_000});
} catch (error) {
  if (error instanceof OperationTimeout) console.log('The operation is still running.');
  if (error instanceof OperationFailed) console.error(error.operation);
  if (error instanceof ApiError) console.error(error.code, error.remediation);
}
```

The ergonomic methods cover common deployment and inference workflows. `infercrane.api` exposes the
complete generated control API with the same authentication and error contract.

See the [TypeScript SDK guide](https://docs.infercrane.com/integrations/typescript),
[control API](https://docs.infercrane.com/control-api), and
[security guidance](https://docs.infercrane.com/security).

## License

[MIT](LICENSE)
