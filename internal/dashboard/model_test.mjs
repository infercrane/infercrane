import assert from 'node:assert/strict';
import test from 'node:test';

import { canCancel, classifyError, costLabel, duration, fleetSummary, freshness, metric, statusTone } from './static/model.mjs';

test('API failures distinguish credentials, authorization, and disconnects', () => {
  assert.equal(classifyError(401, {}).kind, 'unauthorized');
  assert.equal(classifyError(403, {}).kind, 'forbidden');
  assert.equal(classifyError(503, {}).kind, 'disconnected');
  assert.equal(classifyError(422, { error: { code: 'validation_failed', message: 'bad spec' } }).message, 'bad spec');
});

test('freshness marks old retained evidence as stale', () => {
  assert.equal(freshness(10_000, 20_000, 30_000).stale, false);
  assert.deepEqual(freshness(10_000, 50_001, 30_000), { stale: true, ageMs: 40_001, label: 'Evidence is 40s old' });
  assert.equal(freshness(0).label, 'No current evidence');
});

test('unknown evidence is never fabricated as zero', () => {
  assert.equal(metric(null, 'ms'), 'Not measured');
  assert.equal(metric(undefined), 'Not measured');
  assert.equal(costLabel({ available: false, reason: 'provider price unavailable' }), 'provider price unavailable');
  assert.equal(costLabel({ available: true }), 'Not measured');
  assert.equal(duration(null), 'unknown');
});

test('fleet and status summaries remain deterministic', () => {
  const summary = fleetSummary([{ observed_state: 'healthy' }, { observed_state: 'provisioning' }, { observed_state: 'failed' }]);
  assert.deepEqual(summary, { total: 3, healthy: 1, changing: 1, degraded: 1 });
  assert.equal(statusTone('serving'), 'healthy');
  assert.equal(statusTone('degraded'), 'warning');
  assert.equal(statusTone('failed'), 'error');
});

test('only active non-cancelled operations expose cancellation', () => {
  assert.equal(canCancel({ id: 'op', status: 'running' }), true);
  assert.equal(canCancel({ id: 'op', status: 'running', cancel_requested: true }), false);
  assert.equal(canCancel({ id: 'op', status: 'succeeded' }), false);
  assert.equal(canCancel(null), false);
});
