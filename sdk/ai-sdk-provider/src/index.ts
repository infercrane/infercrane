import {
  createOpenAICompatible,
  type OpenAICompatibleProvider,
} from '@ai-sdk/openai-compatible';
import type {FetchFunction} from '@ai-sdk/provider-utils';

export const INFERCRANE_DEFAULT_BASE_URL = 'https://provider.infercrane.com/v1';

export type InferCraneChatModelId =
  | 'qwen/qwen3.8-27b'
  | (string & {});

export type InferCraneProvider = OpenAICompatibleProvider<
  InferCraneChatModelId,
  never,
  never,
  never
>;

export interface InferCraneProviderSettings {
  /** InferCrane API key. Defaults to INFERCRANE_API_KEY in server runtimes. */
  apiKey?: string;

  /** Override the InferCrane API base URL. */
  baseURL?: string;

  /** Additional headers sent after the authorization header. */
  headers?: Record<string, string>;

  /** Custom fetch implementation for tracing, retries, or tests. */
  fetch?: FetchFunction;
}

function readEnvironment(name: string): string | undefined {
  if (typeof process === 'undefined') return undefined;
  const value = process.env?.[name]?.trim();
  return value || undefined;
}

function normalizeBaseURL(value: string): string {
  let end = value.length;
  while (end > 0 && value.charCodeAt(end - 1) === 47) end -= 1;
  return end === value.length ? value : value.slice(0, end);
}

/**
 * Create an InferCrane provider for the Vercel AI SDK.
 *
 * Keep API keys in server-side environments. InferCrane's model API is not
 * intended to be called directly from browser bundles.
 */
export function createInferCrane(
  settings: InferCraneProviderSettings = {},
): InferCraneProvider {
  return createOpenAICompatible<InferCraneChatModelId, never, never, never>({
    name: 'infercrane',
    baseURL: normalizeBaseURL(
      settings.baseURL ??
        readEnvironment('INFERCRANE_BASE_URL') ??
        INFERCRANE_DEFAULT_BASE_URL,
    ),
    apiKey: settings.apiKey ?? readEnvironment('INFERCRANE_API_KEY'),
    headers: settings.headers,
    fetch: settings.fetch,
    includeUsage: true,
    supportsStructuredOutputs: true,
  });
}

/** Default provider configured from INFERCRANE_API_KEY. */
export const infercrane = createInferCrane();
