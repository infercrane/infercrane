import assert from 'node:assert/strict';
import test from 'node:test';

import { ApiError, InferCrane, OperationFailed, OperationTimeout, StreamError } from '../dist/index.js';

const json = (body, status = 200) => new Response(JSON.stringify(body), { status, headers: { 'content-type': 'application/json' } });

test('deploy preserves explicit idempotency and wait reaches terminal state', async () => {
  const calls = [];
  const operations = [
    { id: 'op-1', kind: 'deployment.converge', status: 'running', progress: 50 },
    { id: 'op-1', kind: 'deployment.converge', status: 'succeeded', progress: 100 },
  ];
  const fetch = async (url, options = {}) => {
    calls.push({ url: String(url), options });
    if (String(url).endsWith('/deployments')) return json({ operation: { id: 'op-1', kind: 'deployment.converge', status: 'pending', progress: 0 } }, 202);
    return json(operations.shift());
  };
  const client = new InferCrane({ apiKey: 'secret', baseUrl: 'https://control.example', timeoutMs: 100, pollIntervalMs: 1, fetch });
  const operation = await client.deploy({ model: 'Qwen/Qwen3-8B', name: 'qwen', cloud: 'runpod', gpu: 'L40S', idempotencyKey: 'stable' });
  assert.equal(operation.id, 'op-1');
  assert.equal(calls[0].options.headers['Idempotency-Key'], 'stable');
  assert.equal((await client.wait(operation.id)).status, 'succeeded');
});

test('deploy preserves portable runtime workload intent', async () => {
	let body;
	const fetch = async (_url, options = {}) => { body = JSON.parse(options.body); return json({ operation: { id: 'op-oci', kind: 'deployment.converge', status: 'pending', progress: 0 } }, 202); };
	const client = new InferCrane({ apiKey: 'secret', fetch });
	const workload = { image: `registry.example/runtime@sha256:${'a'.repeat(64)}`, command: ['serve', '--model', '${MODEL}'], protocol: 'openai', port: 8000, readiness_path: '/health', models_path: '/v1/models', metrics_path: '/metrics', cancellation: 'http-disconnect', drain: 'connection', shutdown_grace_seconds: 30 };
	await client.deploy({ model: 'org/model', cloud: 'runpod', gpu: 'L40S', runtime: 'custom-oci', runtimeVersion: '1', runtimeArgs: ['--safe'], workload });
	assert.deepEqual(body.workload, workload); assert.deepEqual(body.runtime_args, ['--safe']); assert.equal(body.runtime_version, '1');
});

test('wait timeout never sends cancellation', async () => {
  const paths = [];
  const fetch = async (url) => { paths.push(String(url)); return json({ id: 'op-1', kind: 'deployment.converge', status: 'waiting', progress: 55 }); };
  const client = new InferCrane({ apiKey: 'secret', timeoutMs: 100, pollIntervalMs: 1, fetch });
  await assert.rejects(() => client.wait('op-1', { timeoutMs: 3 }), OperationTimeout);
  assert.equal(paths.some((path) => path.endsWith('/cancel')), false);
});

test('terminal and API errors are typed', async () => {
  let mode = 'operation';
  const fetch = async () => mode === 'operation'
    ? json({ id: 'op-1', kind: 'deployment.converge', status: 'failed', progress: 55, error_code: 'provider_denied' })
    : json({ error: { code: 'forbidden', message: 'denied', retryable: false } }, 403);
  const client = new InferCrane({ apiKey: 'secret', timeoutMs: 20, pollIntervalMs: 1, fetch });
  await assert.rejects(() => client.wait('op-1'), OperationFailed);
  mode = 'api';
  await assert.rejects(() => client.getDeployment('qwen'), ApiError);
});

test('streaming parses fragmented SSE without replay', async () => {
  let calls = 0;
  const body = new ReadableStream({ start(controller) { controller.enqueue(new TextEncoder().encode('data: {"choices":[{"delta":{"content":"hi"}}]}\n')); controller.enqueue(new TextEncoder().encode('\ndata: [DONE]\n\n')); controller.close(); } });
  const fetch = async () => { calls += 1; return new Response(body, { status: 200, headers: { 'content-type': 'text/event-stream' } }); };
  const client = new InferCrane({ apiKey: 'secret', fetch });
  const chunks = [];
  for await (const chunk of client.streamChat('qwen', [{ role: 'user', content: 'hi' }])) chunks.push(chunk);
  assert.equal(chunks[0].choices[0].delta.content, 'hi');
  assert.equal(calls, 1);
});

test('stream requires a terminal done event', async () => {
  const fetch = async () => new Response('data: {"choices":[]}\n\n', { status: 200 });
  const client = new InferCrane({ apiKey: 'secret', fetch });
  await assert.rejects(async () => { for await (const _ of client.streamChat('qwen', [])) void _; }, StreamError);
});
