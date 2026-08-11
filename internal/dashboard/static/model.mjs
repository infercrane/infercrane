export const terminalStatuses = new Set(['succeeded', 'failed', 'cancelled']);

export function classifyError(status, envelope = {}) {
  const detail = envelope && typeof envelope === 'object' ? envelope.error ?? {} : {};
  if (status === 401) return { kind: 'unauthorized', title: 'Credential rejected', message: detail.message ?? 'Enter a valid InferCrane API key.' };
  if (status === 403) return { kind: 'forbidden', title: 'Permission required', message: detail.message ?? 'This principal does not have the required scope.' };
  if (status === 404) return { kind: 'missing', title: 'Evidence no longer exists', message: detail.message ?? 'The selected resource may have been deleted.' };
  if (status >= 500) return { kind: 'disconnected', title: 'Control plane unavailable', message: detail.message ?? 'InferCrane could not load current evidence.' };
  return { kind: 'failure', title: detail.code ?? 'Request failed', message: detail.message ?? `The control plane returned HTTP ${status}.` };
}

export function freshness(lastSuccess, now = Date.now(), thresholdMs = 30_000) {
  if (!lastSuccess) return { stale: true, ageMs: null, label: 'No current evidence' };
  const ageMs = Math.max(0, now - lastSuccess);
  if (ageMs > thresholdMs) return { stale: true, ageMs, label: `Evidence is ${duration(ageMs)} old` };
  return { stale: false, ageMs, label: 'Current evidence' };
}

export function duration(milliseconds) {
  if (milliseconds === null || milliseconds === undefined || !Number.isFinite(Number(milliseconds))) return 'unknown';
  const seconds = Math.max(0, Math.round(Number(milliseconds) / 1000));
  if (seconds < 60) return `${seconds}s`;
  const minutes = Math.floor(seconds / 60), remainder = seconds % 60;
  return remainder ? `${minutes}m ${remainder}s` : `${minutes}m`;
}

export function metric(value, suffix = '') {
  if (value === null || value === undefined || value === '' || !Number.isFinite(Number(value))) return 'Not measured';
  return `${Number(value).toLocaleString(undefined, { maximumFractionDigits: 2 })}${suffix}`;
}

export function timestamp(value) {
  if (!value) return 'Unknown time';
  const date = new Date(value);
  return Number.isNaN(date.getTime()) ? 'Unknown time' : date.toLocaleString();
}

export function shortID(value, size = 12) {
  const text = String(value ?? '');
  return text.length > size ? `${text.slice(0, size)}…` : text || '—';
}

export function statusTone(value) {
  const status = String(value ?? '').toLowerCase();
  if (['healthy', 'serving', 'ready', 'succeeded', 'accept', 'accepted', 'converged', 'active'].includes(status)) return 'healthy';
  if (['failed', 'reject', 'rejected', 'unhealthy', 'unavailable', 'cancelled'].includes(status)) return 'error';
  if (['waiting', 'pending', 'running', 'provisioning', 'starting', 'draining', 'converging', 'degraded', 'cancelling'].includes(status)) return 'warning';
  return 'muted';
}

export function fleetSummary(deployments) {
  const result = { total: deployments.length, healthy: 0, changing: 0, degraded: 0 };
  for (const deployment of deployments) {
    const state = String(deployment.observed_state ?? '').toLowerCase();
    if (['healthy', 'serving', 'ready'].includes(state)) result.healthy += 1;
    else if (['pending', 'provisioning', 'updating', 'deleting', 'draining'].includes(state)) result.changing += 1;
    else result.degraded += 1;
  }
  return result;
}

export function canCancel(operation) {
  return Boolean(operation?.id) && !terminalStatuses.has(String(operation.status ?? '').toLowerCase()) && !operation.cancel_requested;
}

export function costLabel(value) {
  if (!value || value.available !== true) return value?.reason || 'Not measured';
  if (!Number.isFinite(Number(value.microusd))) return 'Not measured';
  return `$${(Number(value.microusd) / 1_000_000).toFixed(4)}`;
}
