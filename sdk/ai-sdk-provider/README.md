# InferCrane provider for the AI SDK

Use InferCrane's workload-qualified open-weight models with the [Vercel AI SDK](https://ai-sdk.dev/).

## Install

```bash
npm install ai @infercrane/ai-sdk-provider
```

## Stream a response

```ts
import {infercrane} from '@infercrane/ai-sdk-provider';
import {streamText} from 'ai';

const result = streamText({
  model: infercrane('qwen/qwen3.8-27b'),
  prompt: 'Summarize why speculative decoding can improve inference.',
});

for await (const text of result.textStream) {
  process.stdout.write(text);
}
```

Set the API key in the server environment:

```bash
export INFERCRANE_API_KEY=your_api_key
```

Keep API keys in server-side code. Do not expose them in browser bundles.

## Tools

```ts
import {infercrane} from '@infercrane/ai-sdk-provider';
import {generateText, tool} from 'ai';
import {z} from 'zod';

const result = await generateText({
  model: infercrane('qwen/qwen3.8-27b'),
  prompt: 'What is the weather in Berlin?',
  tools: {
    weather: tool({
      description: 'Read the current weather for a city',
      inputSchema: z.object({city: z.string()}),
      execute: async ({city}) => ({city, temperatureCelsius: 18}),
    }),
  },
});

console.log(result.text);
```

## Custom endpoint

```ts
import {createInferCrane} from '@infercrane/ai-sdk-provider';

const infercrane = createInferCrane({
  apiKey: process.env.INFERCRANE_API_KEY,
  baseURL: 'https://provider.infercrane.com/v1',
});
```

`createInferCrane` also reads `INFERCRANE_BASE_URL`. This is useful for a private canary or a dedicated InferCrane deployment.

## Reasoning effort

Qwen3.8 accepts `none`, `low`, `medium`, or `xhigh` through AI SDK provider options:

```ts
const result = await generateText({
  model: infercrane('qwen/qwen3.8-27b'),
  prompt: 'Review this rollout plan.',
  providerOptions: {
    infercrane: {reasoningEffort: 'low'},
  },
});
```

## Supported API surface

The provider uses InferCrane's OpenAI-compatible Chat Completions API and supports streaming, usage reporting, tool calls, structured output, and reasoning controls available on the selected model.

## License

Apache-2.0
