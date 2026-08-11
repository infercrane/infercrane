'use strict';

const fs = require('node:fs');
const crypto = require('node:crypto');
const https = require('node:https');
const os = require('node:os');
const path = require('node:path');
const child = require('node:child_process');

function stripAnsi(value) { return value.replace(/\x1b\[[0-?]*[ -/]*[@-~]/g, ''); }
function secretValues(env) { return Object.entries(env).filter(([key, value]) => /(?:TOKEN|KEY|SECRET|PASSWORD|CREDENTIAL)/i.test(key) && typeof value === 'string' && value.length >= 4).map(([, value]) => value).sort((a, b) => b.length - a.length); }
function redact(value, env = process.env) { let safe = stripAnsi(String(value)); for (const secret of secretValues(env)) safe = safe.split(secret).join('***'); return safe; }
function bounded(value, limit = 60_000) { return value.length <= limit ? value : `${value.slice(0, limit)}\n\n_Output truncated; inspect the machine-readable artifact._`; }
function cell(value) { return String(value ?? '—').replaceAll('|', '\\|').replaceAll('\n', ' '); }

function renderPlan(plan) {
  const lines = [`## InferCrane plan · \`${cell(plan.name)}\``, '', `**Model:** \`${cell(plan.model)}\` · **Runtime:** \`${cell(plan.runtime)}\` · **Mode:** \`${cell(plan.mode)}\``, ''];
  if ((plan.changes ?? []).length) {
    lines.push('| Change | Current | Proposed |', '|---|---|---|');
    for (const change of plan.changes) lines.push(`| ${cell(change.field)} | \`${cell(change.before)}\` | \`${cell(change.after)}\` |`);
  } else lines.push('No persisted deployment changes are required.');
  lines.push('', '### Lifecycle');
  for (const action of [...(plan.actions ?? [])].sort((a, b) => a.order - b.order)) lines.push(`- ${action.kind === 'drain' || action.kind === 'terminate' ? '−' : action.kind === 'noop' ? '=' : '+'} ${cell(action.summary)}`);
  if ((plan.warnings ?? []).length) { lines.push('', '### Warnings'); for (const warning of plan.warnings) lines.push(`- ${cell(warning)}`); }
  const cost = plan.cost ?? {};
  lines.push('', `**Cost:** ${cell(cost.status ?? 'unknown')} — ${cell(cost.reason ?? 'No grounded provider price is available.')}`);
  return bounded(lines.join('\n'));
}

function evaluateRelease(view, expectedRevision) {
  const deployment = view.deployment ?? {};
  const lifecycle = view.lifecycle_status ?? {};
  const failures = [];
  if (expectedRevision && deployment.active_revision_id !== expectedRevision) failures.push(`active revision is ${deployment.active_revision_id || 'none'}, expected ${expectedRevision}`);
  if (!['serving', 'ready'].includes(lifecycle.serving_state)) failures.push(`serving state is ${lifecycle.serving_state || 'unknown'}`);
  if (lifecycle.convergence_state !== 'converged') failures.push(`convergence state is ${lifecycle.convergence_state || 'unknown'}`);
  if (view.active_operation) failures.push(`operation ${view.active_operation.id} is still ${view.active_operation.status}`);
  return { pass: failures.length === 0, deployment: deployment.name, revision: deployment.active_revision_id, failures };
}

function run(command, args, env = process.env) {
  const result = child.spawnSync(command, args, { encoding: 'utf8', env, maxBuffer: 8 * 1024 * 1024 });
  const stdout = redact(result.stdout ?? '', env), stderr = redact(result.stderr ?? '', env);
  if (result.error) throw new Error(`could not run InferCrane CLI: ${result.error.message}`);
  if (result.status !== 0) throw new Error(stderr.trim() || stdout.trim() || `InferCrane exited ${result.status}`);
  return stdout;
}

function download(url, destination, redirects = 0) {
  return new Promise((resolve, reject) => {
    let parsed;
    try { parsed = new URL(url); } catch { reject(new Error('download URL is invalid')); return; }
    if (parsed.protocol !== 'https:') { reject(new Error('download URL must use HTTPS')); return; }
    if (redirects > 5) { reject(new Error('download exceeded five redirects')); return; }
    const request = https.get(url, { headers: { 'User-Agent': 'infercrane-github-action' } }, (response) => {
      if ([301, 302, 307, 308].includes(response.statusCode)) {
        response.resume();
        const location = response.headers.location;
        if (!location) { reject(new Error('download redirect omitted Location')); return; }
        download(new URL(location, parsed).toString(), destination, redirects + 1).then(resolve, reject);
        return;
      }
      if (response.statusCode !== 200) { response.resume(); reject(new Error(`download failed with HTTP ${response.statusCode}`)); return; }
      const output = fs.createWriteStream(destination, { mode: 0o600 }); response.pipe(output); output.on('finish', () => output.close(resolve)); output.on('error', reject);
    });
    request.setTimeout(30_000, () => request.destroy(new Error('download timed out'))); request.on('error', reject);
  });
}

async function resolveCLI(input, version, tempDirectory) {
  if (input && input !== 'auto') return input;
  const located = child.spawnSync(process.platform === 'win32' ? 'where' : 'sh', process.platform === 'win32' ? ['infercrane'] : ['-c', 'command -v infercrane'], { encoding: 'utf8' });
  if (located.status === 0 && located.stdout.trim()) return located.stdout.trim().split(/\r?\n/)[0];
  if (!/^v\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?$/.test(version)) throw new Error('version must be an exact v-prefixed release tag');
  const platform = { linux: 'linux', darwin: 'darwin' }[process.platform];
  const architecture = { x64: 'amd64', arm64: 'arm64' }[process.arch];
  if (!platform || !architecture) throw new Error(`automatic installation does not support ${process.platform}/${process.arch}`);
  const releaseVersion = version.slice(1), asset = `infercrane_${releaseVersion}_${platform}_${architecture}.tar.gz`;
  const root = `https://github.com/infercrane/infercrane/releases/download/${version}`;
  fs.mkdirSync(tempDirectory, { recursive: true });
  const archive = path.join(tempDirectory, asset), checksums = path.join(tempDirectory, 'checksums.txt');
  await download(`${root}/${asset}`, archive); await download(`${root}/checksums.txt`, checksums);
  const expectedLine = fs.readFileSync(checksums, 'utf8').split(/\r?\n/).find((line) => line.trim().endsWith(`  ${asset}`));
  if (!expectedLine) throw new Error(`checksums.txt does not contain ${asset}`);
  const expected = expectedLine.trim().split(/\s+/)[0], actual = crypto.createHash('sha256').update(fs.readFileSync(archive)).digest('hex');
  if (actual !== expected) throw new Error(`checksum mismatch for ${asset}`);
  const extracted = child.spawnSync('tar', ['-xzf', archive, '-C', tempDirectory, 'infercrane'], { encoding: 'utf8' });
  if (extracted.status !== 0) throw new Error(`could not extract InferCrane: ${redact(extracted.stderr)}`);
  const binary = path.join(tempDirectory, 'infercrane'); fs.chmodSync(binary, 0o755); return binary;
}

module.exports = { bounded, evaluateRelease, redact, renderPlan, resolveCLI, run };
