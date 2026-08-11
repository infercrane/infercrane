import { randomUUID } from 'node:crypto';
import { ControlApi, type ApiTransport } from './generated/api.js';
import type { Deployment, JsonValue, Operation } from './generated/models.js';
import { ApiError, OperationCancelled, OperationFailed, OperationTimeout, StreamError } from './errors.js';

export interface InferCraneOptions {
  apiKey?: string;
  baseUrl?: string;
  gatewayUrl?: string;
  timeoutMs?: number;
  pollIntervalMs?: number;
  fetch?: typeof globalThis.fetch;
}

export interface DeployRequest {
  model: string;
  name?: string;
  cloud: string;
  gpu: string;
  runtime?: string;
  computeMode?: 'elastic' | 'serverless';
  minReplicas?: number;
  maxReplicas?: number;
  region?: string;
  modelRevision?: string;
  runtimeVersion?: string;
  runtimeArgs?: string[];
  workload?: {
    image: string; command: string[]; protocol: 'openai'; port: number;
    readiness_path: '/health'; models_path: '/v1/models'; metrics_path: '/metrics';
    cancellation: 'http-disconnect'; drain: 'connection'; shutdown_grace_seconds: number;
  };
  idempotencyKey?: string;
}

function controlUrl(value: string): string {
  const clean = value.replace(/\/$/, '');
  return clean.endsWith('/api/v1') ? clean : `${clean}/api/v1`;
}

function gatewayUrl(value: string): string {
  return value.replace(/\/$/, '').replace(/\/api\/v1$/, '');
}

class Transport implements ApiTransport {
  constructor(private readonly baseUrl: string, private readonly apiKey: string, private readonly timeoutMs: number, private readonly fetcher: typeof globalThis.fetch) {}

  async request(method: string, path: string, options: { body?: JsonValue; idempotencyKey?: string } = {}): Promise<unknown> {
    const controller = new AbortController();
    const timer = setTimeout(() => controller.abort(new Error(`request timed out after ${this.timeoutMs}ms`)), this.timeoutMs);
    try {
      const headers: Record<string, string> = { Accept: 'application/json', Authorization: `Bearer ${this.apiKey}`, 'User-Agent': 'infercrane-typescript/0.9.0-rc.1' };
      if (options.body !== undefined) headers['Content-Type'] = 'application/json';
      if (options.idempotencyKey) headers['Idempotency-Key'] = options.idempotencyKey;
      const init: RequestInit = { method, headers, signal: controller.signal };
      if (options.body !== undefined) init.body = JSON.stringify(options.body);
      const response = await this.fetcher(`${this.baseUrl}${path}`, init);
      const text = await response.text();
      let value: unknown = undefined;
      if (text) {
        try { value = JSON.parse(text); } catch { throw new ApiError(response.status, 'invalid_response', 'control plane returned invalid JSON'); }
      }
      if (!response.ok) {
        const envelope = value as { error?: { code?: string; message?: string; retryable?: boolean; remediation?: string } } | undefined;
        const detail = envelope?.error;
        throw new ApiError(response.status, detail?.code ?? 'http_error', detail?.message ?? response.statusText, detail?.retryable ?? false, detail?.remediation ?? '');
      }
      return value;
    } catch (error) {
      if (error instanceof ApiError) throw error;
      throw new ApiError(0, 'transport_error', error instanceof Error ? error.message : String(error), true);
    } finally {
      clearTimeout(timer);
    }
  }
}

export class InferCrane {
  readonly api: ControlApi;
	private readonly transport: Transport;
  private readonly apiKey: string;
  private readonly gatewayUrl: string;
  private readonly timeoutMs: number;
  private readonly pollIntervalMs: number;
  private readonly fetcher: typeof globalThis.fetch;

  constructor(options: InferCraneOptions = {}) {
    const apiKey = options.apiKey ?? process.env['INFERCRANE_API_KEY'] ?? '';
    if (!apiKey) throw new TypeError('apiKey or INFERCRANE_API_KEY is required');
    const baseUrl = options.baseUrl ?? process.env['INFERCRANE_CONTROL_URL'] ?? 'http://127.0.0.1:18000';
    const timeoutMs = options.timeoutMs ?? 30_000;
    const pollIntervalMs = options.pollIntervalMs ?? 1_000;
    if (timeoutMs <= 0 || pollIntervalMs <= 0) throw new TypeError('timeoutMs and pollIntervalMs must be positive');
    this.apiKey = apiKey;
    this.gatewayUrl = gatewayUrl(options.gatewayUrl ?? baseUrl);
    this.timeoutMs = timeoutMs;
    this.pollIntervalMs = pollIntervalMs;
    this.fetcher = options.fetch ?? globalThis.fetch;
	this.transport = new Transport(controlUrl(baseUrl), apiKey, timeoutMs, this.fetcher);
	this.api = new ControlApi(this.transport);
  }

  async deploy(request: DeployRequest): Promise<Operation> {
    const name = request.name ?? request.model.split('/').at(-1)!.toLowerCase().replaceAll('_', '-');
    const body: Record<string, JsonValue> = { name, model: request.model, cloud: request.cloud, gpu: request.gpu, runtime: request.runtime ?? 'vllm', compute_mode: request.computeMode ?? 'elastic', min_replicas: request.minReplicas ?? 1, max_replicas: request.maxReplicas ?? 1 };
    if (request.region) body['region'] = request.region;
    if (request.modelRevision) body['model_revision'] = request.modelRevision;
    if (request.runtimeVersion) body['runtime_version'] = request.runtimeVersion;
    if (request.runtimeArgs?.length) body['runtime_args'] = request.runtimeArgs;
    if (request.workload) body['workload'] = request.workload as unknown as JsonValue;
    const result = await this.api.createDeployment(body, request.idempotencyKey ?? `sdk-deploy-${randomUUID()}`) as { operation: Operation };
    return result.operation;
  }

  async getOperation(id: string): Promise<Operation> { return await this.api.getOperation(id) as Operation; }

  async wait(id: string, options: { timeoutMs?: number; signal?: AbortSignal } = {}): Promise<Operation> {
    const timeoutMs = options.timeoutMs ?? this.timeoutMs;
    if (timeoutMs <= 0) throw new TypeError('timeoutMs must be positive');
    const deadline = Date.now() + timeoutMs;
    while (true) {
      if (options.signal?.aborted) throw options.signal.reason ?? new DOMException('Aborted', 'AbortError');
      const operation = await this.getOperation(id);
      if (operation.status === 'succeeded') return operation;
      if (operation.status === 'failed') throw new OperationFailed(operation);
      if (operation.status === 'cancelled') throw new OperationCancelled(operation);
      const remaining = deadline - Date.now();
      if (remaining <= 0) throw new OperationTimeout(id, timeoutMs);
      await new Promise<void>((resolve, reject) => {
        const aborted = () => { clearTimeout(timer); reject(options.signal?.reason ?? new DOMException('Aborted', 'AbortError')); };
        const timer = setTimeout(() => { options.signal?.removeEventListener('abort', aborted); resolve(); }, Math.min(this.pollIntervalMs, remaining));
        options.signal?.addEventListener('abort', aborted, { once: true });
      });
    }
  }

  async cancel(id: string): Promise<void> { await this.api.cancelOperation(id); }
  async getDeployment(name: string): Promise<Deployment> { return (await this.api.getDeployment(name) as { deployment: Deployment }).deployment; }
  async delete(name: string, idempotencyKey = `sdk-delete-${randomUUID()}`): Promise<Operation> { return (await this.api.deleteDeployment(name, idempotencyKey) as { operation: Operation }).operation; }

  async setSloPolicy(deployment: string, policy: { max_ttft_p95_ms?: number; max_latency_p95_ms?: number; max_error_rate?: number; min_output_tokens_second?: number; max_hourly_cost?: number }): Promise<Record<string, JsonValue>> {
    if (!Object.keys(policy).length) throw new TypeError('at least one SLO threshold is required');
    if (Object.values(policy).some((value) => value === undefined || !Number.isFinite(value) || value < 0) || (policy.max_error_rate ?? 0) > 1) throw new TypeError('SLO thresholds must be finite, nonnegative, and error rate cannot exceed 1');
    return (await this.transport.request('PUT', `/deployments/${encodeURIComponent(deployment)}/slo-policy`, { body: policy as JsonValue }) as { policy: Record<string, JsonValue> }).policy;
  }

  async getSloPolicy(deployment: string): Promise<Record<string, JsonValue>> { return (await this.transport.request('GET', `/deployments/${encodeURIComponent(deployment)}/slo-policy`) as { policy: Record<string, JsonValue> }).policy; }
  async deleteSloPolicy(deployment: string): Promise<void> { await this.transport.request('DELETE', `/deployments/${encodeURIComponent(deployment)}/slo-policy`); }

  async recommend(deployment: string): Promise<Record<string, JsonValue>> {
    return (await this.transport.request('POST', `/deployments/${encodeURIComponent(deployment)}/recommendations`, { body: {} }) as { recommendation: Record<string, JsonValue> }).recommendation;
  }

  async recommendations(deployment: string, limit = 20): Promise<Array<Record<string, JsonValue>>> { if (!Number.isInteger(limit) || limit < 1 || limit > 100) throw new TypeError('limit must be an integer between 1 and 100'); return (await this.transport.request('GET', `/deployments/${encodeURIComponent(deployment)}/recommendations?limit=${limit}`) as { data: Array<Record<string, JsonValue>> }).data; }

  async *streamChat(deployment: string, messages: Array<{ role: string; content: string }>, options: { signal?: AbortSignal; parameters?: Record<string, JsonValue> } = {}): AsyncGenerator<Record<string, JsonValue>> {
    const init: RequestInit = { method: 'POST', headers: { Accept: 'text/event-stream', Authorization: `Bearer ${this.apiKey}`, 'Content-Type': 'application/json', 'User-Agent': 'infercrane-typescript/0.9.0-rc.1' }, body: JSON.stringify({ model: deployment, messages, stream: true, ...options.parameters }) };
    if (options.signal) init.signal = options.signal;
    const response = await this.fetcher(`${this.gatewayUrl}/v1/chat/completions`, init);
    if (!response.ok) throw new ApiError(response.status, 'inference_error', await response.text());
    if (!response.body) throw new StreamError('inference response has no stream body');
    const reader = response.body.getReader();
    const decoder = new TextDecoder();
    let buffer = '';
    let completed = false;
    try {
      while (true) {
        const { value, done } = await reader.read();
        buffer += decoder.decode(value, { stream: !done }).replaceAll('\r\n', '\n');
        let boundary: number;
        while ((boundary = buffer.indexOf('\n\n')) >= 0) {
          const event = buffer.slice(0, boundary); buffer = buffer.slice(boundary + 2);
          const data = event.split('\n').filter((line) => line.startsWith('data:')).map((line) => line.slice(5).trimStart()).join('\n');
          if (!data) continue;
          if (data === '[DONE]') { completed = true; return; }
          try { yield JSON.parse(data) as Record<string, JsonValue>; } catch { throw new StreamError('inference stream contained invalid JSON'); }
        }
        if (done) break;
      }
    } finally {
      await reader.cancel().catch(() => undefined);
    }
    if (!completed) throw new StreamError('inference stream ended before [DONE]');
  }
}
