# InferCrane TypeScript SDK

```ts
import { InferCrane } from '@infercrane/sdk';

const client = new InferCrane({ apiKey: process.env.INFERCRANE_API_KEY! });
const operation = await client.deploy({
  model: 'Qwen/Qwen3-8B',
  name: 'qwen-prod',
  cloud: 'runpod',
  gpu: 'L40S',
});
await client.wait(operation.id, { timeoutMs: 900_000 });
```

Waiting is client-side only. A timeout or process exit leaves the durable operation running. See the
[SDK guide](https://infercrane.mintlify.site/integrations/typescript) for streaming and cancellation.
