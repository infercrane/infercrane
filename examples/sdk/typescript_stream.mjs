import { InferCrane } from '../../sdk/typescript/dist/index.js';

const client = new InferCrane({
  apiKey: process.env.INFERCRANE_API_KEY,
  baseUrl: process.env.INFERCRANE_CONTROL_URL ?? 'http://127.0.0.1:18000',
});

let events = 0;
for await (const _event of client.streamChat(
  process.env.INFERCRANE_DEPLOYMENT ?? 'qwen-prod',
  [{ role: 'user', content: 'SDK smoke test' }],
)) events += 1;

if (events === 0) throw new Error('stream returned no events');
console.log('TypeScript SDK stream completed');
