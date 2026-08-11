'use strict';

const assert = require('node:assert/strict');
const child = require('node:child_process');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const test = require('node:test');
const { evaluateRelease, redact, renderPlan } = require('../lib.js');

test('semantic Markdown is deterministic, ordered, and free of ANSI', () => {
  const plan = { name:'qwen', model:'Qwen/Qwen3-8B', runtime:'vllm', mode:'provisioned', changes:[{field:'GPU',before:'L40S',after:'H100'}], actions:[{order:2,kind:'route',summary:'Route candidate'},{order:1,kind:'provision',summary:'Provision candidate'}], cost:{status:'unknown',reason:'provider did not expose price'} };
  const first = renderPlan(plan), second = renderPlan(plan);
  assert.equal(first, second); assert.ok(first.indexOf('Provision candidate') < first.indexOf('Route candidate')); assert.equal(first.includes('\x1b'), false);
});

test('release checks require exact revision and convergence', () => {
  const passing = evaluateRelease({deployment:{name:'qwen',active_revision_id:'rev-2'},lifecycle_status:{serving_state:'serving',convergence_state:'converged'}}, 'rev-2', [{id:'passport',revision_id:'rev-2',verified:true,complete:true,digest:'sha256:abc'}]);
  assert.equal(passing.pass, true);
  const failing = evaluateRelease({deployment:{name:'qwen',active_revision_id:'rev-1'},lifecycle_status:{serving_state:'unavailable',convergence_state:'converging'},active_operation:{id:'op-1',status:'waiting'}}, 'rev-2');
  assert.equal(failing.pass, false); assert.equal(failing.failures.length, 5);
});

test('secret values and terminal escapes are removed', () => { assert.equal(redact('\x1b[31msecret-value\x1b[0m', {INFERCRANE_API_KEY:'secret-value'}), '***'); });

test('plan mode writes JSON and summary using an existing CLI', () => {
  const directory = fs.mkdtempSync(path.join(os.tmpdir(), 'infercrane-action-'));
  const cli = path.join(directory, 'infercrane');
  fs.writeFileSync(cli, '#!/bin/sh\nprintf \'%s\\n\' \'{"name":"qwen","model":"Qwen/Qwen3-8B","runtime":"vllm","mode":"provisioned","actions":[],"cost":{"status":"unknown","reason":"not measured"}}\'\n', {mode:0o755});
  const output = path.join(directory, 'output'), summary = path.join(directory, 'summary'), artifact = path.join(directory, 'plan.json');
  const result = child.spawnSync(process.execPath, [path.join(__dirname, '..', 'index.js')], {encoding:'utf8',env:{...process.env,INPUT_MODE:'plan',INPUT_SPEC:'deployment.yaml',INPUT_CLI_PATH:cli,INPUT_OUTPUT_PATH:artifact,GITHUB_OUTPUT:output,GITHUB_STEP_SUMMARY:summary}});
  assert.equal(result.status, 0, result.stderr); assert.equal(JSON.parse(fs.readFileSync(artifact)).name, 'qwen'); assert.match(fs.readFileSync(summary,'utf8'), /InferCrane plan/);
});

test('apply is refused without explicit confirmation', () => {
  const result = child.spawnSync(process.execPath, [path.join(__dirname, '..', 'index.js')], {encoding:'utf8',env:{...process.env,INPUT_MODE:'apply',INPUT_SPEC:'deployment.yaml',INPUT_CLI_PATH:process.execPath,INPUT_CONFIRM_APPLY:'false'}});
  assert.notEqual(result.status, 0); assert.match(result.stderr, /confirm-apply/);
});

test('release check requires and records a verified passport', () => {
  const directory = fs.mkdtempSync(path.join(os.tmpdir(), 'infercrane-release-'));
  const cli = path.join(directory, 'infercrane');
  fs.writeFileSync(cli, `#!/bin/sh
if [ "$1" = "status" ]; then
  printf '%s\n' '{"deployment":{"name":"prod","active_revision_id":"rev-2"},"lifecycle_status":{"serving_state":"serving","convergence_state":"converged"}}'
else
  printf '%s\n' '{"data":[{"id":"passport-1","revision_id":"rev-2","verified":true,"complete":true,"digest":"sha256:abc","key_id":"sha256:key"}]}'
fi
`, {mode:0o755});
  const output = path.join(directory, 'output'), summary = path.join(directory, 'summary'), artifact = path.join(directory, 'evidence.json');
  const result = child.spawnSync(process.execPath, [path.join(__dirname, '..', 'index.js')], {encoding:'utf8',env:{...process.env,INPUT_MODE:'release-check',INPUT_DEPLOYMENT:'prod',INPUT_REVISION:'rev-2',INPUT_CLI_PATH:cli,INPUT_OUTPUT_PATH:artifact,GITHUB_OUTPUT:output,GITHUB_STEP_SUMMARY:summary}});
  assert.equal(result.status, 0, result.stderr); assert.equal(JSON.parse(fs.readFileSync(artifact)).passport_digest, 'sha256:abc'); assert.match(fs.readFileSync(summary,'utf8'), /sha256:abc/);
});
