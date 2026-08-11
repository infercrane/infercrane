'use strict';

const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const { evaluateRelease, redact, renderPlan, resolveCLI, run } = require('./lib.js');

function input(name, fallback = '') { return process.env[`INPUT_${name.toUpperCase().replaceAll('-', '_')}`] ?? fallback; }
function append(file, value) { if (file) fs.appendFileSync(file, `${value}\n`, { encoding: 'utf8' }); }
function output(name, value) { const file = process.env.GITHUB_OUTPUT; if (!file) return; const marker = `infercrane_${Date.now()}_${Math.random().toString(16).slice(2)}`; append(file, `${name}<<${marker}\n${value}\n${marker}`); }
function summary(markdown) { append(process.env.GITHUB_STEP_SUMMARY, markdown); }

async function main() {
  const mode = input('mode', 'plan'), spec = input('spec'), deployment = input('deployment'), expectedRevision = input('revision');
  const artifactPath = input('output-path', 'infercrane-delivery.json');
  if (!['plan', 'apply', 'release-check'].includes(mode)) throw new Error('mode must be plan, apply, or release-check');
  const cli = await resolveCLI(input('cli-path', 'auto'), input('version', 'v2.0.0-rc.1'), path.join(process.env.RUNNER_TEMP ?? os.tmpdir(), 'infercrane-action'));
  if (mode === 'plan') {
    if (!spec) throw new Error('spec is required for plan');
    const raw = run(cli, ['plan', spec, '--output', 'json']); const plan = JSON.parse(raw);
    fs.writeFileSync(artifactPath, `${JSON.stringify(plan, null, 2)}\n`, { mode: 0o600 });
    const markdown = renderPlan(plan); summary(markdown); output('result', 'pass'); output('revision', plan.changes?.find((change) => change.field === 'revision')?.after ?? 'unchanged');
  } else if (mode === 'apply') {
    if (input('confirm-apply') !== 'true') throw new Error('apply requires confirm-apply: true and should run in a protected GitHub environment');
    if (!spec) throw new Error('spec is required for apply');
    const key = `github-${process.env.GITHUB_RUN_ID ?? 'local'}-${process.env.GITHUB_RUN_ATTEMPT ?? '1'}`;
    const raw = run(cli, ['apply', spec, '--idempotency-key', key, '--wait', '--wait-timeout', input('wait-timeout', '30m'), '--output', 'json']); const result = JSON.parse(raw);
    fs.writeFileSync(artifactPath, `${JSON.stringify(result, null, 2)}\n`, { mode: 0o600 }); output('result', 'pass'); output('operation-id', result.operation?.id ?? ''); summary(`## InferCrane apply accepted\n\nDurable operation: \`${result.operation?.id ?? 'unknown'}\``);
  } else {
    if (!deployment || !expectedRevision) throw new Error('deployment and exact revision are required for release-check');
    const raw = run(cli, ['status', deployment, '--output', 'json']); const view = JSON.parse(raw);
    const passportRaw = run(cli, ['passport', 'list', deployment, '--output', 'json']); const passports = JSON.parse(passportRaw).data ?? [];
    const evidence = evaluateRelease(view, expectedRevision, passports);
    fs.writeFileSync(artifactPath, `${JSON.stringify(evidence, null, 2)}\n`, { mode: 0o600 }); output('result', evidence.pass ? 'pass' : 'fail'); output('revision', evidence.revision ?? '');
    output('passport-digest', evidence.passport_digest ?? '');
    summary(`## InferCrane release check · \`${deployment}\`\n\n${evidence.pass ? `✅ Passed\n\nPassport: \`${evidence.passport_digest}\` · Key: \`${evidence.passport_key_id}\`` : `❌ Failed\n\n${evidence.failures.map((failure) => `- ${failure}`).join('\n')}`}`);
    if (!evidence.pass) throw new Error(`release check failed: ${evidence.failures.join('; ')}`);
  }
}

main().catch((error) => { const message = redact(error instanceof Error ? error.message : String(error)); process.stderr.write(`::error title=InferCrane delivery::${message.replaceAll('\n', '%0A')}\n`); process.exitCode = 1; });
