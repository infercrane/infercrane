import { canCancel, classifyError, costLabel, fleetSummary, freshness, metric, shortID, statusTone, timestamp } from './model.mjs';

const credentialKey = 'infercrane.dashboard.credential';
const selectionKey = 'infercrane.dashboard.selection';
const views = new Set(['overview', 'rollout', 'performance', 'infrastructure', 'scaling', 'activity']);
const tabStorage = {
  get(key) { try { return sessionStorage.getItem(key) ?? ''; } catch { return ''; } },
  set(key, value) { try { sessionStorage.setItem(key, value); } catch { /* Memory-only operation remains available. */ } },
  remove(key) { try { sessionStorage.removeItem(key); } catch { /* Nothing persisted. */ } },
};
const state = { key: tabStorage.get(credentialKey), deployments: [], selected: tabStorage.get(selectionKey), view: 'overview', detail: null, supplemental: {}, partial: [], lastSuccess: 0, loading: false, timer: null };
const $ = (id) => document.getElementById(id);

function element(tag, options = {}, children = []) {
  const node = document.createElement(tag);
  for (const [name, value] of Object.entries(options)) {
    if (value === undefined || value === null) continue;
    if (name === 'class') node.className = value;
    else if (name === 'text') node.textContent = String(value);
    else if (name === 'dataset') Object.assign(node.dataset, value);
    else node.setAttribute(name, String(value));
  }
  for (const child of Array.isArray(children) ? children : [children]) if (child) node.append(child);
  return node;
}

function text(value, fallback = '—') { return value === null || value === undefined || value === '' ? fallback : String(value); }
function array(value) { return Array.isArray(value) ? value : []; }
function valueAt(value, ...keys) { for (const key of keys) if (value?.[key] !== undefined && value?.[key] !== null) return value[key]; return null; }
function toneBadge(label) { return element('span', { class: `badge tone-${statusTone(label)}`, text: text(label, 'unknown') }); }
function empty(title, copy) { return element('div', { class: 'empty' }, [element('strong', { text: title }), element('span', { text: copy })]); }
function panel(title, child, span = 12, description = '') {
  const heading = element('div', { class: 'panel-header' }, [element('div', {}, [element('h2', { text: title }), description ? element('p', { text: description }) : null])]);
  return element('section', { class: `panel span-${span}` }, [heading, child]);
}
function keyValues(entries) {
  const list = element('dl', { class: 'kv' });
  for (const [label, value] of entries) list.append(element('dt', { text: label }), element('dd', { text: text(value) }));
  return list;
}
function metrics(entries) {
  return element('div', { class: 'metric-grid' }, entries.map(([label, value]) => element('div', { class: 'metric' }, [element('span', { text: label }), element('strong', { text: value })])));
}
function table(headers, rows, emptyCopy = 'No persisted evidence') {
  if (!rows.length) return empty('No evidence yet', emptyCopy);
  const head = element('tr', {}, headers.map(([label]) => element('th', { scope: 'col', text: label })));
  const body = element('tbody');
  for (const row of rows) body.append(element('tr', {}, headers.map(([, getter]) => {
    const value = getter(row);
    return element('td', { text: text(value), class: typeof value === 'string' && value.length > 18 ? 'code' : '' });
  })));
  return element('div', { class: 'table-wrap', tabindex: '0', role: 'region', 'aria-label': 'Scrollable evidence table' }, [element('table', {}, [element('thead', {}, [head]), body])]);
}

async function api(path, options = {}) {
  const response = await fetch(`/api/v1${path}`, { ...options, cache: 'no-store', headers: { Accept: 'application/json', Authorization: `Bearer ${state.key}`, ...(options.headers ?? {}) } });
  const raw = await response.text();
  let value = {};
  if (raw) { try { value = JSON.parse(raw); } catch { throw { status: response.status || 502, envelope: { error: { message: 'The control plane returned invalid JSON.' } } }; } }
  if (!response.ok) throw { status: response.status, envelope: value };
  return value;
}

function connection(kind, label) {
  const region = $('connection-state');
  region.replaceChildren(element('span', { class: `status-dot tone-${kind}`, 'aria-hidden': 'true' }), element('span', { text: label }));
}
function notice(message, kind = '') {
  const node = $('notice');
  node.hidden = !message; node.className = `notice ${kind}`.trim(); node.textContent = message;
}
function setBusy(value) {
  state.loading = value; $('content').setAttribute('aria-busy', String(value)); $('refresh').disabled = value;
  for (const button of $('deployment-list').querySelectorAll('button')) button.disabled = value;
}
function clearWorkspace() {
  state.deployments = []; state.selected = ''; state.detail = null; state.supplemental = {}; state.partial = []; state.lastSuccess = 0;
  $('deployment-list').replaceChildren(); $('fleet-summary').replaceChildren(); $('fleet-count').textContent = '0'; $('view-tabs').hidden = true;
  $('deployment-context').textContent = 'NOT CONNECTED'; $('deployment-title').textContent = 'Fleet overview'; $('deployment-subtitle').textContent = 'Connect to load operational evidence.';
  $('content').replaceChildren(empty('Not connected', 'Connect to load operational evidence.'));
  $('skip-link').setAttribute('href', '#connection-panel'); $('skip-link').textContent = 'Skip to connection';
}

async function load({ initial = false } = {}) {
  if (!state.key || state.loading || document.hidden) return;
  setBusy(true);
  if (initial) connection('warning', 'Connecting…'); else connection('warning', 'Refreshing…');
  try {
    const fleet = await api('/deployments');
    state.deployments = array(fleet.data).sort((a, b) => String(a.name).localeCompare(String(b.name)));
    if (!state.selected || !state.deployments.some((item) => item.name === state.selected)) state.selected = state.deployments[0]?.name ?? '';
    if (state.selected) tabStorage.set(selectionKey, state.selected); else tabStorage.remove(selectionKey);
    state.partial = [];
    if (state.selected) await loadDetail(); else { state.detail = null; state.supplemental = {}; }
    state.lastSuccess = Date.now();
    $('workspace').hidden = false; $('connection-panel').hidden = true; $('skip-link').setAttribute('href', '#main'); $('skip-link').textContent = 'Skip to operational evidence';
    state.partial.sort();
    connection('healthy', 'Connected'); notice(state.partial.length ? `Some evidence is unavailable: ${state.partial.join(', ')}.` : '', state.partial.length ? '' : 'healthy');
    render();
  } catch (error) {
    const failure = classifyError(error?.status ?? 503, error?.envelope);
    connection('error', failure.kind === 'unauthorized' ? 'Authentication required' : 'Disconnected');
    if (failure.kind === 'unauthorized') {
      tabStorage.remove(credentialKey); state.key = ''; clearWorkspace(); $('workspace').hidden = true; $('connection-panel').hidden = false; $('api-key').focus(); $('connection-error').textContent = failure.message;
    } else {
      notice(`${failure.title}. ${failure.message} Existing evidence is retained and marked stale.`, 'error'); renderFreshness();
    }
  } finally { setBusy(false); }
}

async function optional(name, path) {
  try { return await api(path); } catch (error) {
    if (error?.status === 401) throw error;
    state.partial.push(name); return { data: [], unavailable: true, status: error?.status };
  }
}

async function loadDetail() {
  const name = encodeURIComponent(state.selected);
  const detail = await api(`/deployments/${name}`);
  const operationPath = detail.active_operation?.id ? `/operations/${encodeURIComponent(detail.active_operation.id)}/events?limit=100` : '';
  const [events, scaling, benchmarks, orphans, audit, operationEvents] = await Promise.all([
    optional('events', `/deployments/${name}/events`), optional('scaling decisions', `/deployments/${name}/scaling-decisions?limit=100`), optional('benchmarks', `/deployments/${name}/benchmarks?limit=50`), optional('orphan inventory', '/orphans'), optional('audit history', '/audit-events?limit=100'), operationPath ? optional('operation timeline', operationPath) : Promise.resolve({ data: [] }),
  ]);
  state.detail = detail; state.supplemental = { events: array(events.data), scaling: array(scaling.data), benchmarks: array(benchmarks.data), orphans: array(orphans.data), audit: array(audit.data), operationEvents: array(operationEvents.data), auditForbidden: audit.status === 403 };
}

function render() {
  renderFleet(); renderHeader(); renderFreshness();
  $('view-tabs').hidden = !state.detail;
  if (!state.detail) { $('content').replaceChildren(empty('No deployments', 'Run infercrane deploy <model> or apply a DeploymentSpec.')); return; }
  const renderers = { overview: renderOverview, rollout: renderRollout, performance: renderPerformance, infrastructure: renderInfrastructure, scaling: renderScaling, activity: renderActivity };
  $('content').replaceChildren(renderers[state.view]());
}

function renderFleet() {
  $('fleet-count').textContent = String(state.deployments.length); $('fleet-empty').hidden = state.deployments.length > 0;
  const summary = fleetSummary(state.deployments);
  $('fleet-summary').replaceChildren(...[['Healthy', summary.healthy, 'healthy'], ['Changing', summary.changing, 'warning'], ['Attention', summary.degraded, 'error']].map(([label, count, tone]) => element('div', {}, [element('strong', { class: `tone-${tone}`, text: count }), element('span', { text: label })])));
  const list = $('deployment-list'); list.replaceChildren();
  for (const deployment of state.deployments) {
    const button = element('button', { class: 'deployment-item', type: 'button', 'aria-current': deployment.name === state.selected ? 'true' : 'false' }, [element('strong', { text: deployment.name }), element('span', { class: `status-line tone-${statusTone(deployment.observed_state)}` }, [element('span', { class: 'status-dot', 'aria-hidden': 'true' }), element('span', { text: text(deployment.observed_state, 'unknown') })]), element('span', { class: 'model', text: deployment.model })]);
    button.addEventListener('click', async () => { if (state.selected === deployment.name) return; state.selected = deployment.name; state.view = 'overview'; await load(); }); list.append(button);
  }
}

function renderHeader() {
  const deployment = state.detail?.deployment;
  $('deployment-context').textContent = deployment ? `${text(deployment.runtime, 'runtime unknown')} · ${text(deployment.routing_strategy, 'routing unknown')}`.toUpperCase() : 'FLEET';
  $('deployment-title').textContent = deployment?.name ?? 'Fleet overview';
  $('deployment-subtitle').textContent = deployment ? `${deployment.model} · revision ${shortID(deployment.active_revision_id)}` : 'Select a deployment to inspect persisted operational evidence.';
  for (const tab of $('view-tabs').querySelectorAll('button')) tab.setAttribute('aria-current', tab.dataset.view === state.view ? 'page' : 'false');
}

function renderFreshness() {
  const current = freshness(state.lastSuccess);
  $('updated-at').textContent = current.stale ? current.label : `Updated ${new Date(state.lastSuccess).toLocaleTimeString()}`;
  if (current.stale && state.lastSuccess) notice(`${current.label}. Refresh before making an operational decision.`, 'error');
}

function deterministicExplanation(detail) {
  const lifecycle = detail.lifecycle_status ?? {}, operation = detail.active_operation;
  if (operation) return `${operation.kind} is ${operation.status}: ${operation.message || 'the durable operation is still converging'}.`;
  if (lifecycle.convergence_state === 'degraded') return `${lifecycle.unhealthy_targets ?? 0} target(s) are unhealthy; desired state is not converged.`;
  if (lifecycle.provisioning_replicas > 0) return `${lifecycle.provisioning_replicas} replica(s) are provisioning.`;
  if (lifecycle.draining_replicas > 0) return `${lifecycle.draining_replicas} replica(s) are draining.`;
  if (lifecycle.serving_state === 'serving' && lifecycle.convergence_state === 'converged') return 'The deployment is serving and no persisted convergence blocker is active.';
  return `Serving is ${text(lifecycle.serving_state, 'unknown')} and convergence is ${text(lifecycle.convergence_state, 'unknown')}.`;
}

function renderOverview() {
  const d = state.detail, deployment = d.deployment, lifecycle = d.lifecycle_status ?? {}, operation = d.active_operation;
  const grid = element('div', { class: 'grid' });
  grid.append(panel('Current state', metrics([['Serving', text(lifecycle.serving_state)], ['Convergence', text(lifecycle.convergence_state)], ['Candidate', text(lifecycle.candidate_state)], ['Ready replicas', `${lifecycle.ready_replicas ?? 0} / ${lifecycle.desired_replicas ?? 0}`], ['Provisioning', text(lifecycle.provisioning_replicas, '0')], ['Draining', text(lifecycle.draining_replicas, '0')]]), 7));
  grid.append(panel('Deployment identity', keyValues([['Model', deployment.model], ['Runtime', deployment.runtime], ['Desired state', deployment.desired_state], ['Active revision', deployment.active_revision_id], ['Candidate revision', deployment.candidate_revision_id], ['Replica bounds', `${deployment.min_replicas} → ${deployment.max_replicas}`]]), 5));
  grid.append(panel('Deterministic explanation', element('div', { class: 'explanation' }, [element('p', { text: deterministicExplanation(d) })]), 12, 'Derived from current persisted lifecycle state.'));
  const operationBody = operation ? keyValues([['ID', operation.id], ['Kind', operation.kind], ['Status', operation.status], ['Progress', `${operation.progress ?? 0}%`], ['Attempt', `${operation.attempt ?? 0} / ${operation.max_attempts ?? 0}`], ['Next attempt', timestamp(operation.next_attempt_at)], ['Message', operation.message]]) : empty('No active operation', 'The deployment is not blocked by durable work.');
  const operationPanel = panel('Active operation', operationBody, 12, operation ? 'Closing this page does not stop durable work.' : 'Deployment operations will appear here.');
  if (operation && canCancel(operation)) { const action = element('button', { class: 'button danger', type: 'button', text: 'Request cancellation' }); action.addEventListener('click', () => openCancel(operation)); operationPanel.querySelector('.panel-header').append(action); }
  grid.append(operationPanel);
  if (operation) grid.append(panel('Operation timeline', table([['Sequence', (r) => r.sequence], ['When', (r) => timestamp(r.created_at)], ['Level', (r) => r.level], ['Type', (r) => r.type], ['Message', (r) => r.message]], state.supplemental.operationEvents, 'No progress event has been persisted for this operation.'), 12));
  return grid;
}

function renderRollout() {
  const d = state.detail, deployment = d.deployment, evaluations = array(d.release_guard_evaluations), revisions = array(d.revisions);
  const latest = evaluations[0];
  const grid = element('div', { class: 'grid' });
  grid.append(panel('Revision pointers', keyValues([['Active', deployment.active_revision_id], ['Candidate', deployment.candidate_revision_id], ['Candidate state', d.lifecycle_status?.candidate_state], ['Immutable revisions', revisions.length]]), 5));
  grid.append(panel('Latest Release Guard', latest ? keyValues([['Decision', latest.decision], ['Active evidence', latest.active_revision_id], ['Candidate evidence', latest.candidate_revision_id], ['Evaluated', timestamp(latest.created_at)], ['Reasons', JSON.stringify(latest.reasons ?? [])]]) : empty('Not evaluated', 'Create a candidate and run Release Guard to persist a decision.'), 7, 'Only a decision bound to the current candidate can authorize promotion.'));
  grid.append(panel('Revision history', table([['Revision', (r) => `#${r.number} · ${shortID(r.id)}`], ['Status', (r) => r.status], ['Reason', (r) => r.reason], ['Created', (r) => timestamp(r.created_at)], ['Activated', (r) => timestamp(r.activated_at)]], revisions, 'No revisions have been persisted.'), 12));
  grid.append(panel('Guard evidence', table([['Decision', (r) => r.decision], ['Candidate', (r) => shortID(r.candidate_revision_id)], ['Reasons', (r) => JSON.stringify(r.reasons ?? [])], ['Recorded', (r) => timestamp(r.created_at)]], evaluations, 'No Release Guard evaluation has been persisted.'), 12)); return grid;
}

function renderPerformance() {
  const stats = state.detail.request_stats ?? {}, cold = state.detail.cold_start_stats ?? {}, benchmarks = state.supplemental.benchmarks;
  const latest = benchmarks[0] ?? {};
  const errorRate = valueAt(stats, 'error_rate');
  const requestsPerSecond = valueAt(stats, 'requests_per_second');
  const grid = element('div', { class: 'grid' });
  grid.append(panel('Last 5 minutes', metrics([['Requests / second', metric(requestsPerSecond)], ['Error rate', Number(requestsPerSecond) === 0 ? 'No requests' : errorRate === null ? 'Not measured' : metric(Number(errorRate) * 100, '%')], ['Latency p95', metric(valueAt(stats, 'p95_latency_ms'), 'ms')], ['TTFT p95', metric(valueAt(stats, 'p95_ttft_ms'), 'ms')], ['Output tok/s', metric(valueAt(stats, 'output_tokens_per_second'))], ['Input tok/s', metric(valueAt(stats, 'input_tokens_per_second'))]]), 7, 'Unknown measurements remain unknown; they are never displayed as zero.'));
  grid.append(panel('Cold starts · 24h', metrics([['Cold starts', metric(valueAt(cold, 'cold_starts', 'count'))], ['Cold TTFT p95', metric(valueAt(cold, 'cold_ttft_p95_ms'), 'ms')], ['Warm TTFT p95', metric(valueAt(cold, 'warm_ttft_p95_ms'), 'ms')], ['Time to ready p95', metric(valueAt(cold, 'time_to_ready_p95_ms'), 'ms')]]), 5));
  grid.append(panel('Cold-start evidence', keyValues([['Classified requests', metric(cold.classified_requests)], ['Available boundaries', array(cold.available_boundaries).join(' → ') || 'None exposed'], ['Unavailable boundaries', array(cold.unavailable_boundaries).join(', ') || 'None'], ['Bottleneck code', cold.bottleneck_code || 'Not established'], ['Method', cold.evidence || 'No persisted evidence']]), 12, 'Only provider/runtime boundaries actually observed by InferCrane are shown.'));
  grid.append(panel('Latest benchmark', latest.id ? keyValues([['Tool', `${latest.tool} ${latest.tool_version ?? ''}`], ['Revision', latest.revision_id], ['TTFT p95', metric(latest.ttft_p95_ms, 'ms')], ['TPOT p95', metric(latest.tpot_p95_ms, 'ms')], ['Output tok/s', metric(latest.output_token_throughput)], ['Errors', metric(latest.failed)], ['Cost', costLabel(latest.cost_metadata)], ['Reproduce', latest.reproduction_command]]) : empty('No benchmark history', 'Run infercrane benchmark to persist a reproducible measurement.'), 12));
  grid.append(panel('Benchmark history', table([['When', (r) => timestamp(r.created_at)], ['Revision', (r) => shortID(r.revision_id)], ['GPU', (r) => r.gpu], ['TTFT p95', (r) => metric(r.ttft_p95_ms, 'ms')], ['TPOT p95', (r) => metric(r.tpot_p95_ms, 'ms')], ['Output tok/s', (r) => metric(r.output_token_throughput)], ['Errors', (r) => r.failed]], benchmarks, 'No benchmark has been persisted.'), 12)); return grid;
}

function renderInfrastructure() {
  const d = state.detail, targets = array(d.targets), replicas = array(d.replicas), artifacts = array(d.model_artifacts), orphans = state.supplemental.orphans;
  const grid = element('div', { class: 'grid' });
  grid.append(panel('Capacity', metrics([['Targets', String(targets.length)], ['Replicas', String(replicas.length)], ['Healthy targets', String(targets.filter((r) => r.health === 'healthy').length)], ['Run-owned orphans', String(orphans.length)]]), 12));
  grid.append(panel('Targets', table([['Name', (r) => r.name], ['Provider', (r) => r.provider], ['Runtime', (r) => r.runtime], ['Health', (r) => r.health], ['Resource', (r) => r.provider_resource_id]], targets, 'No route target is registered.'), 12));
  grid.append(panel('Provider replicas', table([['Ordinal', (r) => r.ordinal], ['Revision', (r) => shortID(r.revision_id)], ['Provider', (r) => r.provider], ['Lifecycle', (r) => r.lifecycle_state], ['Health', (r) => r.health], ['Resource', (r) => r.provider_resource_id]], replicas, 'This deployment may use existing targets rather than provider-managed replicas.'), 12));
  grid.append(panel('Model artifacts', table([['Repository', (r) => r.repository], ['Immutable revision', (r) => shortID(r.immutable_revision)], ['Cache', (r) => r.cache_state], ['Approx. size', (r) => r.approximate_size_bytes ? metric(r.approximate_size_bytes / 1e9, ' GB') : 'Not measured']], artifacts, 'No immutable artifact identity is available.'), 12));
  grid.append(panel('Orphan inventory', table([['Name', (r) => r.name], ['Provider', (r) => r.provider], ['Resource', (r) => r.provider_resource_id], ['Observed', (r) => timestamp(r.created_at)]], orphans, 'No run-owned orphan is recorded.'), 12)); return grid;
}

function renderScaling() {
  const deployment = state.detail.deployment, decisions = state.supplemental.scaling;
  const grid = element('div', { class: 'grid' });
  grid.append(panel('Policy', keyValues([['Enabled', deployment.autoscaling_enabled ? 'Yes' : 'No'], ['Minimum replicas', deployment.min_replicas], ['Maximum replicas', deployment.max_replicas], ['Current desired', state.detail.lifecycle_status?.desired_replicas]]), 4));
  const latest = decisions[0];
  grid.append(panel('Why capacity changed', latest ? element('div', { class: 'explanation' }, [element('p', { text: `${latest.action}: ${latest.reason}` })]) : empty('No scaling decision', deployment.autoscaling_enabled ? 'The controller has not persisted a scaling decision.' : 'Autoscaling is disabled for this deployment.'), 8));
  grid.append(panel('Decision history', table([['When', (r) => timestamp(r.created_at)], ['Action', (r) => r.action], ['Replicas', (r) => `${r.old_replicas} → ${r.new_replicas}`], ['Reason', (r) => r.reason], ['Signals', (r) => JSON.stringify(r.signals ?? {})]], decisions, 'No scaling decision is recorded.'), 12)); return grid;
}

function renderActivity() {
  const events = state.supplemental.events, audit = state.supplemental.audit;
  const timeline = events.length ? element('ol', { class: 'timeline' }, events.map((event) => element('li', {}, [element('time', { text: timestamp(event.created_at) }), element('code', { text: event.type }), element('p', { text: event.summary })]))) : empty('No deployment events', 'Persisted lifecycle events will appear here.');
  const auditBody = state.supplemental.auditForbidden ? empty('Administrative scope required', 'Audit history is available only to a principal with tenant-management scope.') : table([['When', (r) => timestamp(r.created_at)], ['Actor', (r) => r.actor], ['Action', (r) => r.action], ['Resource', (r) => `${r.resource_type}/${r.resource_name}`], ['Outcome', (r) => r.outcome]], audit, 'No audit event is visible.');
  return element('div', { class: 'grid' }, [panel('Deployment events', timeline, 12, 'Historical failures are evidence, not current health.'), panel('Audit history', auditBody, 12)]);
}

function openCancel(operation) {
  const dialog = $('cancel-dialog'), input = $('cancel-confirmation'), submit = $('cancel-submit');
  dialog.dataset.operation = operation.id; input.value = ''; submit.disabled = true;
  input.oninput = () => { submit.disabled = input.value !== operation.id; };
  dialog.showModal(); setTimeout(() => input.focus(), 0);
}

async function cancelOperation(id) {
  setBusy(true);
  try { await api(`/operations/${encodeURIComponent(id)}/cancel`, { method: 'POST' }); notice(`Cancellation requested for ${id}. Durable cleanup may continue after this page closes.`, 'healthy'); setBusy(false); await load(); }
  catch (error) { const failure = classifyError(error?.status ?? 503, error?.envelope); notice(`${failure.title}. ${failure.message}`, 'error'); }
  finally { setBusy(false); }
}

$('connection-form').addEventListener('submit', async (event) => { event.preventDefault(); const key = $('api-key').value.trim(); if (!key) return; state.key = key; tabStorage.set(credentialKey, key); $('api-key').value = ''; $('connection-error').textContent = ''; await load({ initial: true }); });
$('refresh').addEventListener('click', () => load());
$('forget').addEventListener('click', () => { tabStorage.remove(credentialKey); tabStorage.remove(selectionKey); state.key = ''; clearWorkspace(); $('workspace').hidden = true; $('connection-panel').hidden = false; connection('muted', 'Not connected'); $('api-key').focus(); });
$('view-tabs').addEventListener('click', (event) => { const view = event.target?.dataset?.view; if (!views.has(view)) return; state.view = view; render(); });
$('cancel-dialog').addEventListener('close', () => { if ($('cancel-dialog').returnValue === 'confirm' && $('cancel-confirmation').value === $('cancel-dialog').dataset.operation) cancelOperation($('cancel-dialog').dataset.operation); });

const themes = ['system', 'light', 'dark'];
function setTheme(theme) { const value = themes.includes(theme) ? theme : 'system'; document.documentElement.dataset.theme = value; $('theme-toggle').setAttribute('aria-label', `Appearance: ${value}`); $('theme-toggle').title = `Appearance: ${value}`; }
setTheme(tabStorage.get('infercrane.dashboard.theme') || 'system');
$('theme-toggle').addEventListener('click', () => { const next = themes[(themes.indexOf(document.documentElement.dataset.theme) + 1) % themes.length]; tabStorage.set('infercrane.dashboard.theme', next); setTheme(next); });

document.addEventListener('visibilitychange', () => { if (!document.hidden && state.key) load(); });
state.timer = setInterval(() => { if (!document.hidden && state.key) load(); }, 15_000);
if (state.key) load({ initial: true }); else $('api-key').focus();
