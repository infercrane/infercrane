"use strict";

const assert = require("node:assert/strict");
const child = require("node:child_process");
const fs = require("node:fs");
const http = require("node:http");
const os = require("node:os");
const path = require("node:path");
const test = require("node:test");

const INDEX = path.join(__dirname, "..", "index.js");

// gateResult is what `infercrane optimize gate --output json` prints.
function gateResult(verdict, conclusion, key = "deploy/qwen.yaml") {
  return {
    schema_version: "infercrane.proof-gate/v1",
    verdict,
    conclusion,
    headline: `${verdict} headline`,
    reasons: [`${verdict} because measured`],
    campaign_id: "run-1",
    report_digest: "sha256:abc",
    markdown: `<!-- infercrane-proof-gate:${key} -->\n### ${verdict} · InferCrane proof gate\n`,
  };
}

// fakeCLI is an executable that logs its arguments and answers each command
// from the scenario file the test writes.
function fakeCLI(directory, scenario) {
  const cli = path.join(directory, "infercrane");
  fs.writeFileSync(path.join(directory, "scenario.json"), JSON.stringify(scenario));
  fs.writeFileSync(
    cli,
    `#!${process.execPath}
const fs = require("node:fs"), path = require("node:path");
const directory = ${JSON.stringify(directory)};
const args = process.argv.slice(2);
fs.appendFileSync(path.join(directory, "calls.jsonl"), JSON.stringify(args) + "\\n");
const scenario = JSON.parse(fs.readFileSync(path.join(directory, "scenario.json"), "utf8"));
const answer = scenario[args[1]];
if (args[1] === "compare") fs.writeFileSync(path.join(directory, "current-seen.yaml"), fs.readFileSync(args[2]));
if (args[1] === "gate" && args.includes("--save-report")) fs.writeFileSync(args[args.indexOf("--save-report") + 1], '{"report":{}}');
if (!answer) { process.stderr.write("usage: infercrane optimize propose|create MODEL\\n"); process.exit(2); }
process.stdout.write(JSON.stringify(answer.stdout) + "\\n");
process.exit(answer.status ?? 0);
`,
    { mode: 0o755 },
  );
  return cli;
}

function calls(directory) {
  const file = path.join(directory, "calls.jsonl");
  if (!fs.existsSync(file)) return [];
  return fs.readFileSync(file, "utf8").trim().split("\n").map((line) => JSON.parse(line));
}

// fakeGitHub records comment requests and returns existing comments.
async function fakeGitHub(existing = []) {
  const requests = [];
  const server = http.createServer((request, response) => {
    let body = "";
    request.on("data", (chunk) => (body += chunk));
    request.on("end", () => {
      requests.push({ method: request.method, url: request.url, auth: request.headers.authorization, body: body ? JSON.parse(body) : undefined });
      response.setHeader("Content-Type", "application/json");
      if (request.method === "GET") response.end(JSON.stringify(existing));
      else response.end(JSON.stringify({ id: 99, html_url: "https://github.example/pr/7#comment-99" }));
    });
  });
  await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve));
  return { url: `http://127.0.0.1:${server.address().port}`, requests, close: () => server.close() };
}

function runAction(env, cwd) {
  return new Promise((resolve) => {
    const process_ = child.spawn(process.execPath, [INDEX], { cwd, env: { ...process.env, ...env } });
    let stdout = "",
      stderr = "";
    process_.stdout.on("data", (chunk) => (stdout += chunk));
    process_.stderr.on("data", (chunk) => (stderr += chunk));
    process_.on("close", (status) => resolve({ status, stdout, stderr }));
  });
}

function workspace(prefix) {
  const directory = fs.mkdtempSync(path.join(os.tmpdir(), prefix));
  const event = path.join(directory, "event.json");
  fs.writeFileSync(event, JSON.stringify({ pull_request: { number: 7, base: { sha: "BASE" }, head: { sha: "HEAD" } } }));
  return { directory, event, output: path.join(directory, "output"), summary: path.join(directory, "summary"), artifact: path.join(directory, "gate.json") };
}

function readMode(ws, cli, github, extra = {}) {
  return {
    INPUT_MODE: "proof-gate",
    INPUT_CAMPAIGN: "run-1",
    INPUT_CLI_PATH: cli,
    INPUT_OUTPUT_PATH: ws.artifact,
    INPUT_REPORT_PATH: path.join(ws.directory, "report.json"),
    INPUT_COMMENT_KEY: "deploy/qwen.yaml",
    INPUT_GITHUB_TOKEN: "ghs_secret_token_value",
    INPUT_PROOF_TIMEOUT: "3h",
    INPUT_ON_WATCH: "neutral",
    INPUT_COMMENT: "true",
    GITHUB_EVENT_PATH: ws.event,
    GITHUB_REPOSITORY: "acme/serving",
    GITHUB_API_URL: github.url,
    GITHUB_OUTPUT: ws.output,
    GITHUB_STEP_SUMMARY: ws.summary,
    RUNNER_TEMP: ws.directory,
    ...extra,
  };
}

test("a PASS succeeds, posts one comment, and records the verdict", async () => {
  const ws = workspace("infercrane-gate-pass-");
  const cli = fakeCLI(ws.directory, { gate: { stdout: gateResult("PASS", "success") } });
  const github = await fakeGitHub();
  try {
    const result = await runAction(readMode(ws, cli, github), ws.directory);
    assert.equal(result.status, 0, result.stderr);
    const outputs = fs.readFileSync(ws.output, "utf8");
    assert.match(outputs, /verdict<<[^\n]+\nPASS\n/);
    assert.match(outputs, /conclusion<<[^\n]+\nsuccess\n/);
    assert.match(outputs, /result<<[^\n]+\npass\n/);
    assert.match(outputs, /comment-url<<[^\n]+\nhttps:\/\/github.example\/pr\/7#comment-99\n/);
    assert.equal(JSON.parse(fs.readFileSync(ws.artifact, "utf8")).verdict, "PASS");
    assert.equal(JSON.parse(fs.readFileSync(ws.artifact, "utf8")).markdown, undefined);
    assert.match(fs.readFileSync(ws.summary, "utf8"), /### PASS · InferCrane proof gate/);
    const [list, post] = github.requests;
    assert.equal(list.method, "GET");
    assert.match(list.url, /^\/repos\/acme\/serving\/issues\/7\/comments\?per_page=100&page=1$/);
    assert.equal(list.auth, "Bearer ghs_secret_token_value");
    assert.equal(post.method, "POST");
    assert.match(post.body.body, /^<!-- infercrane-proof-gate:deploy\/qwen.yaml -->/);
    const gateCall = calls(ws.directory)[0];
    assert.deepEqual(gateCall.slice(0, 3), ["optimize", "gate", "run-1"]);
    assert.ok(gateCall.includes("--save-report") && gateCall.includes("--wait"));
  } finally {
    github.close();
  }
});

test("a BLOCK fails the check and updates the existing comment instead of adding one", async () => {
  const ws = workspace("infercrane-gate-block-");
  const cli = fakeCLI(ws.directory, { gate: { stdout: gateResult("BLOCK", "failure"), status: 2 } });
  const github = await fakeGitHub([
    { id: 5, body: "unrelated comment" },
    { id: 41, body: "<!-- infercrane-proof-gate:deploy/qwen.yaml -->\nold verdict" },
  ]);
  try {
    const result = await runAction(readMode(ws, cli, github), ws.directory);
    assert.notEqual(result.status, 0);
    assert.match(result.stderr, /proof gate BLOCK: BLOCK because measured/);
    assert.doesNotMatch(result.stderr, /ghs_secret_token_value/);
    const patch = github.requests.find((request) => request.method === "PATCH");
    assert.ok(patch, "the existing comment is edited");
    assert.equal(patch.url, "/repos/acme/serving/issues/comments/41");
    assert.equal(github.requests.filter((request) => request.method === "POST").length, 0);
    assert.match(fs.readFileSync(ws.output, "utf8"), /result<<[^\n]+\nfail\n/);
  } finally {
    github.close();
  }
});

test("WATCH is neutral by default and fails only when asked; UNKNOWN always fails", async () => {
  const github = await fakeGitHub();
  try {
    for (const [verdict, conclusion, onWatch, wantStatus] of [
      ["WATCH", "neutral", "neutral", 0],
      ["WATCH", "failure", "fail", 1],
      ["UNKNOWN", "failure", "pass", 1],
    ]) {
      const ws = workspace("infercrane-gate-watch-");
      const cli = fakeCLI(ws.directory, { gate: { stdout: gateResult(verdict, conclusion), status: conclusion === "failure" ? 2 : 0 } });
      const result = await runAction(readMode(ws, cli, github, { INPUT_ON_WATCH: onWatch, INPUT_COMMENT: "false" }), ws.directory);
      assert.equal(result.status === 0 ? 0 : 1, wantStatus, `${verdict} on-watch ${onWatch}: ${result.stderr}`);
      assert.ok(calls(ws.directory)[0].includes(onWatch));
      if (conclusion === "neutral") {
        assert.match(result.stdout, /::warning title=InferCrane proof gate::WATCH: WATCH because measured/);
        assert.match(fs.readFileSync(ws.output, "utf8"), /result<<[^\n]+\nneutral\n/);
      }
    }
    assert.equal(github.requests.length, 0, "comment: false posts nothing");
  } finally {
    github.close();
  }
});

test("a comment that cannot be posted warns but never changes the verdict", async () => {
  const ws = workspace("infercrane-gate-fork-");
  const cli = fakeCLI(ws.directory, { gate: { stdout: gateResult("PASS", "success") } });
  const result = await runAction(readMode(ws, cli, { url: "http://127.0.0.1:9" }), ws.directory);
  assert.equal(result.status, 0, result.stderr);
  assert.match(result.stdout, /::warning title=InferCrane proof gate::could not comment on the pull request/);
});

test("a CLI without the proof gate says which release it needs", async () => {
  const ws = workspace("infercrane-gate-old-cli-");
  const cli = fakeCLI(ws.directory, {});
  const result = await runAction(readMode(ws, cli, { url: "http://127.0.0.1:9" }), ws.directory);
  assert.notEqual(result.status, 0);
  assert.match(result.stderr, /has no "optimize gate"/);
});

function gitRepository(ws, baseSpec, headSpec) {
  const git = (...args) => {
    const result = child.spawnSync("git", args, { cwd: ws.directory, encoding: "utf8" });
    assert.equal(result.status, 0, result.stderr);
    return result.stdout.trim();
  };
  git("init", "-q");
  git("config", "user.email", "gate@example.invalid");
  git("config", "user.name", "gate");
  fs.mkdirSync(path.join(ws.directory, "deploy"));
  if (baseSpec !== undefined) {
    fs.writeFileSync(path.join(ws.directory, "deploy", "qwen.yaml"), baseSpec);
    git("add", "deploy/qwen.yaml");
  } else {
    fs.writeFileSync(path.join(ws.directory, "README"), "base");
    git("add", "README");
  }
  git("commit", "-q", "-m", "base");
  const base = git("rev-parse", "HEAD");
  fs.writeFileSync(path.join(ws.directory, "deploy", "qwen.yaml"), headSpec);
  return base;
}

function runMode(ws, cli, base, extra = {}) {
  return {
    ...readMode(ws, cli, { url: "http://127.0.0.1:9" }, { INPUT_COMMENT: "false" }),
    INPUT_CAMPAIGN: "",
    INPUT_SPEC: "deploy/qwen.yaml",
    INPUT_BASE_REF: base,
    INPUT_MAX_COST_USD: "25",
    INPUT_APPROVAL_WINDOW: "2h",
    INPUT_OBJECTIVE: "latency",
    INPUT_RELEASE_SERVERS: "true",
    ...extra,
  };
}

const runScenario = {
  compare: { stdout: { campaign: { id: "run-7", state: "awaiting_approval" }, created: true } },
  approve: { stdout: { campaign: { id: "run-7", state: "approved" } } },
  gate: { stdout: gateResult("PASS", "success") },
  cancel: { stdout: { campaign: { id: "run-7", state: "cancelled" } } },
};

test("spec mode proves the change against the base commit's spec, then releases the servers", async () => {
  const ws = workspace("infercrane-gate-run-");
  const base = gitRepository(ws, "name: qwen\nargs: []\n", "name: qwen\nargs: [--max-num-batched-tokens, '8192']\n");
  const cli = fakeCLI(ws.directory, runScenario);
  const result = await runAction(runMode(ws, cli, base), ws.directory);
  assert.equal(result.status, 0, result.stderr);
  const sequence = calls(ws.directory);
  assert.deepEqual(sequence.map((args) => args[1]), ["compare", "approve", "gate", "cancel"]);
  const [compare, approve, gate] = sequence;
  assert.equal(compare[3], "deploy/qwen.yaml");
  assert.equal(fs.readFileSync(path.join(ws.directory, "current-seen.yaml"), "utf8"), "name: qwen\nargs: []\n");
  assert.deepEqual(compare.slice(4, 6), ["--objective", "latency"]);
  const key = compare[compare.indexOf("--idempotency-key") + 1];
  assert.match(key, /^gate-[0-9a-f]{48}$/);
  assert.deepEqual(approve, ["optimize", "approve", "run-7", "--max-cost-usd", "25", "--expires-in", "2h", "--yes", "--output", "json"]);
  assert.equal(gate[2], "run-7");
  assert.match(fs.readFileSync(ws.output, "utf8"), /campaign-id<<[^\n]+\nrun-7\n/);

  // A retried step reuses the same run; re-running the job measures again.
  fs.rmSync(path.join(ws.directory, "calls.jsonl"));
  await runAction(runMode(ws, cli, base), ws.directory);
  const again = calls(ws.directory)[0];
  assert.equal(again[again.indexOf("--idempotency-key") + 1], key);
  fs.rmSync(path.join(ws.directory, "calls.jsonl"));
  await runAction(runMode(ws, cli, base, { GITHUB_RUN_ATTEMPT: "2" }), ws.directory);
  const rerun = calls(ws.directory)[0];
  assert.notEqual(rerun[rerun.indexOf("--idempotency-key") + 1], key);
});

test("spec mode spends nothing when the spec is unchanged, and refuses without a cap or a current setup", async () => {
  const unchanged = workspace("infercrane-gate-same-");
  const base = gitRepository(unchanged, "name: qwen\n", "name: qwen\n");
  const cli = fakeCLI(unchanged.directory, runScenario);
  let result = await runAction(runMode(unchanged, cli, base), unchanged.directory);
  assert.equal(result.status, 0, result.stderr);
  assert.equal(calls(unchanged.directory).length, 0);
  assert.match(fs.readFileSync(unchanged.output, "utf8"), /conclusion<<[^\n]+\nskipped\n/);

  const uncapped = workspace("infercrane-gate-uncapped-");
  const uncappedBase = gitRepository(uncapped, "name: qwen\n", "name: qwen\nargs: [--enforce-eager]\n");
  result = await runAction(runMode(uncapped, fakeCLI(uncapped.directory, runScenario), uncappedBase, { INPUT_MAX_COST_USD: "" }), uncapped.directory);
  assert.notEqual(result.status, 0);
  assert.match(result.stderr, /max-cost-usd is required/);
  assert.equal(calls(uncapped.directory).length, 0);

  const fresh = workspace("infercrane-gate-new-");
  const freshBase = gitRepository(fresh, undefined, "name: qwen\n");
  result = await runAction(runMode(fresh, fakeCLI(fresh.directory, runScenario), freshBase), fresh.directory);
  assert.notEqual(result.status, 0);
  assert.match(result.stderr, /is new in this pull request/);
});

test("a failed proof still releases the servers it started", async () => {
  const ws = workspace("infercrane-gate-release-");
  const base = gitRepository(ws, "name: qwen\n", "name: qwen\nargs: [--enforce-eager]\n");
  const cli = fakeCLI(ws.directory, { ...runScenario, gate: { stdout: gateResult("UNKNOWN", "failure"), status: 2 } });
  const result = await runAction(runMode(ws, cli, base), ws.directory);
  assert.notEqual(result.status, 0);
  assert.deepEqual(calls(ws.directory).map((args) => args[1]), ["compare", "approve", "gate", "cancel"]);
});
