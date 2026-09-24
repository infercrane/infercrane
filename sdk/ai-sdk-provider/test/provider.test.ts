import {generateText, streamText} from 'ai';
import {afterEach, describe, expect, it, vi} from 'vitest';
import {
  INFERCRANE_DEFAULT_BASE_URL,
  createInferCrane,
} from '../src/index.js';

const completion = {
  id: 'chatcmpl_test',
  object: 'chat.completion',
  created: 1_790_000_000,
  model: 'qwen/qwen3.8-27b',
  choices: [
    {
      index: 0,
      message: {role: 'assistant', content: 'Fast and qualified.'},
      finish_reason: 'stop',
    },
  ],
  usage: {prompt_tokens: 6, completion_tokens: 3, total_tokens: 9},
};

afterEach(() => {
  vi.unstubAllEnvs();
});

describe('createInferCrane', () => {
  it('uses the public endpoint and an explicit bearer credential', async () => {
    let requestURL = '';
    let requestInit: RequestInit | undefined;
    const provider = createInferCrane({
      apiKey: 'test-secret',
      fetch: async (input, init) => {
        requestURL = input instanceof Request ? input.url : String(input);
        requestInit = init;
        return Response.json(completion);
      },
    });

    const result = await generateText({
      model: provider('qwen/qwen3.8-27b'),
      prompt: 'Describe InferCrane.',
    });

    expect(result.text).toBe('Fast and qualified.');
    expect(result.usage).toMatchObject({inputTokens: 6, outputTokens: 3});
    expect(requestURL).toBe(`${INFERCRANE_DEFAULT_BASE_URL}/chat/completions`);

    const headers = new Headers(requestInit?.headers);
    expect(headers.get('authorization')).toBe('Bearer test-secret');

    const body = JSON.parse(String(requestInit?.body));
    expect(body.model).toBe('qwen/qwen3.8-27b');
    expect(body.messages).toEqual([
      {role: 'user', content: 'Describe InferCrane.'},
    ]);
  });

  it('loads server configuration from the InferCrane environment', async () => {
    vi.stubEnv('INFERCRANE_API_KEY', 'environment-secret');
    vi.stubEnv('INFERCRANE_BASE_URL', 'https://canary.infercrane.test/v1/');

    let request: Request | undefined;
    const provider = createInferCrane({
      fetch: async (input, init) => {
        request = new Request(input, init);
        return Response.json(completion);
      },
    });

    await generateText({
      model: provider('qwen/qwen3.8-27b'),
      prompt: 'hello',
    });

    expect(request?.url).toBe(
      'https://canary.infercrane.test/v1/chat/completions',
    );
    expect(request?.headers.get('authorization')).toBe(
      'Bearer environment-secret',
    );
  });

  it('normalizes a base URL with a long trailing slash sequence in linear time', async () => {
    let requestURL = '';
    const provider = createInferCrane({
      apiKey: 'test-secret',
      baseURL: `https://canary.infercrane.test/v1${'/'.repeat(100_000)}`,
      fetch: async input => {
        requestURL = input instanceof Request ? input.url : String(input);
        return Response.json(completion);
      },
    });

    await generateText({
      model: provider('qwen/qwen3.8-27b'),
      prompt: 'hello',
    });

    expect(requestURL).toBe(
      'https://canary.infercrane.test/v1/chat/completions',
    );
  });

  it('forwards native Qwen reasoning effort controls', async () => {
    let requestBody: Record<string, unknown> | undefined;
    const provider = createInferCrane({
      apiKey: 'test-secret',
      fetch: async (_input, init) => {
        requestBody = JSON.parse(String(init?.body));
        return Response.json(completion);
      },
    });

    await generateText({
      model: provider('qwen/qwen3.8-27b'),
      prompt: 'Think briefly.',
      providerOptions: {
        infercrane: {reasoningEffort: 'low'},
      },
    });

    expect(requestBody?.reasoning_effort).toBe('low');
  });

  it('streams text and final usage from the OpenAI-compatible endpoint', async () => {
    const chunks = [
      {
        id: 'chatcmpl_stream',
        object: 'chat.completion.chunk',
        created: 1_790_000_000,
        model: 'qwen/qwen3.8-27b',
        choices: [
          {index: 0, delta: {role: 'assistant', content: 'Fast '}, finish_reason: null},
        ],
      },
      {
        id: 'chatcmpl_stream',
        object: 'chat.completion.chunk',
        created: 1_790_000_000,
        model: 'qwen/qwen3.8-27b',
        choices: [
          {index: 0, delta: {content: 'and qualified.'}, finish_reason: null},
        ],
      },
      {
        id: 'chatcmpl_stream',
        object: 'chat.completion.chunk',
        created: 1_790_000_000,
        model: 'qwen/qwen3.8-27b',
        choices: [{index: 0, delta: {}, finish_reason: 'stop'}],
        usage: {prompt_tokens: 6, completion_tokens: 3, total_tokens: 9},
      },
    ];
    const encoder = new TextEncoder();
    const body = chunks.map(chunk => `data: ${JSON.stringify(chunk)}\n\n`).join('') +
      'data: [DONE]\n\n';
    const provider = createInferCrane({
      apiKey: 'test-secret',
      fetch: async () => new Response(encoder.encode(body), {
        headers: {'content-type': 'text/event-stream'},
      }),
    });

    const result = streamText({
      model: provider('qwen/qwen3.8-27b'),
      prompt: 'Describe InferCrane.',
    });
    let text = '';
    for await (const delta of result.textStream) text += delta;

    expect(text).toBe('Fast and qualified.');
    await expect(result.usage).resolves.toMatchObject({
      inputTokens: 6,
      outputTokens: 3,
    });
  });
});
