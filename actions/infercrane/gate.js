"use strict";

// The proof gate: run InferCrane's proof of a pull request's serving change,
// or read a finished run's verdict, and turn it into this check's outcome
// and one pull-request comment. The verdict itself is decided by the CLI
// (`infercrane optimize gate`) from the run's signed report; this file only
// orchestrates the CLI, the GitHub job, and the comment.

const fs = require("node:fs");
const crypto = require("node:crypto");
const path = require("node:path");
const child = require("node:child_process");
const { redact } = require("./lib.js");

const GATE_SCHEMA = "infercrane.proof-gate/v1";
const DURATION = /^(?:\d+h)?(?:\d+m)?(?:\d+s)?$/;

function flag(value) {
  return String(value).trim().toLowerCase() === "true";
}

function duration(name, value) {
  const trimmed = String(value).trim();
  if (!trimmed || !DURATION.test(trimmed))
    throw new Error(`${name} must be a duration such as 3h, 90m, or 1h30m`);
  return trimmed;
}

// pullRequest reads the pull request this job runs for from the event file.
function pullRequest(env = process.env) {
  if (!env.GITHUB_EVENT_PATH) return {};
  try {
    const event = JSON.parse(fs.readFileSync(env.GITHUB_EVENT_PATH, "utf8"));
    const pr = event.pull_request ?? {};
    return { number: pr.number, baseSha: pr.base?.sha, headSha: pr.head?.sha };
  } catch {
    return {};
  }
}

// cliJSON runs the CLI and returns its JSON stdout. The gate prints its
// result even when the check fails, so a non-zero exit with a gate result is
// a verdict, not an error.
function cliJSON(cli, args, env = process.env) {
  const result = child.spawnSync(cli, args, { encoding: "utf8", env, maxBuffer: 8 * 1024 * 1024 });
  if (result.error) throw new Error(`could not run InferCrane CLI: ${result.error.message}`);
  const stdout = redact(result.stdout ?? "", env),
    stderr = redact(result.stderr ?? "", env);
  let parsed;
  try {
    parsed = JSON.parse(stdout);
  } catch {
    parsed = undefined;
  }
  if (result.status === 0 && parsed !== undefined) return { value: parsed, status: 0, stderr };
  if (parsed?.schema_version === GATE_SCHEMA) return { value: parsed, status: result.status, stderr };
  let message = stderr.trim() || stdout.trim() || `InferCrane exited ${result.status}`;
  if (/usage: infercrane optimize/.test(message) && ["gate", "compare"].includes(args[1]))
    message += `\nThis InferCrane CLI has no "optimize ${args[1]}". The proof gate needs a release that includes it; set version or cli-path.`;
  throw new Error(message);
}

// currentSpec returns the path of the setup that serves today: the given
// file, or the pull request's spec as it is on the base commit.
function currentSpec({ spec, currentSpecPath, baseRef, directory }) {
  if (currentSpecPath) return currentSpecPath;
  if (!baseRef)
    throw new Error(
      "no base commit to read the current setup from: run on pull_request, or set base-ref or current-spec",
    );
  if (!/^[0-9A-Za-z._/-]+$/.test(baseRef) || baseRef.startsWith("-"))
    throw new Error("base-ref must be a commit SHA or ref name");
  const relative = path.relative(process.cwd(), path.resolve(spec)).split(path.sep).join("/");
  if (relative.startsWith("..")) throw new Error("spec must be inside the checked-out repository");
  const shown = child.spawnSync("git", ["show", `${baseRef}:${relative}`], {
    encoding: "utf8",
    maxBuffer: 1024 * 1024,
  });
  if (shown.status !== 0) {
    const stderr = shown.stderr ?? "";
    if (/does not exist|exists on disk, but not in/.test(stderr))
      throw new Error(
        `${relative} is new in this pull request: nothing serves it yet, so there is no current setup to prove a regression against`,
      );
    throw new Error(
      `could not read ${relative} at ${baseRef}: check out the base commit too (actions/checkout with fetch-depth: 0), or set current-spec. ${redact(stderr.trim())}`,
    );
  }
  fs.mkdirSync(directory, { recursive: true });
  const destination = path.join(directory, "infercrane-current-spec.yaml");
  fs.writeFileSync(destination, shown.stdout, { mode: 0o600 });
  return destination;
}

function idempotencyKey(parts) {
  const hash = crypto.createHash("sha256");
  for (const part of parts) hash.update(String(part ?? "")).update("\0");
  return `gate-${hash.digest("hex").slice(0, 48)}`;
}

function apiBase(env) {
  const base = (env.GITHUB_API_URL || "https://api.github.com").replace(/\/+$/, "");
  const parsed = new URL(base);
  const local = ["127.0.0.1", "localhost", "[::1]"].includes(parsed.hostname);
  if (parsed.protocol !== "https:" && !(local && parsed.protocol === "http:"))
    throw new Error("GITHUB_API_URL must use HTTPS");
  return base;
}

// upsertComment keeps one comment per gate on the pull request: it edits the
// comment that starts with marker, or adds one.
async function upsertComment({ env, token, repository, number, marker, body }) {
  if (!/^[\w.-]+\/[\w.-]+$/.test(repository ?? "")) throw new Error("GITHUB_REPOSITORY is not owner/name");
  if (!Number.isInteger(number) || number < 1) throw new Error("no pull request number");
  const base = apiBase(env);
  const headers = {
    Accept: "application/vnd.github+json",
    Authorization: `Bearer ${token}`,
    "Content-Type": "application/json",
    "User-Agent": "infercrane-github-action",
    "X-GitHub-Api-Version": "2022-11-28",
  };
  const call = async (url, init = {}) => {
    const response = await fetch(url, { ...init, headers, signal: AbortSignal.timeout(30_000) });
    if (!response.ok) {
      const error = new Error(`GitHub API ${init.method ?? "GET"} returned HTTP ${response.status}`);
      error.status = response.status;
      throw error;
    }
    return response.json();
  };
  let existing;
  for (let page = 1; page <= 10 && !existing; page++) {
    const comments = await call(`${base}/repos/${repository}/issues/${number}/comments?per_page=100&page=${page}`);
    existing = comments.find((comment) => typeof comment.body === "string" && comment.body.startsWith(marker));
    if (comments.length < 100) break;
  }
  const payload = JSON.stringify({ body });
  if (existing) {
    try {
      const updated = await call(`${base}/repos/${repository}/issues/comments/${existing.id}`, { method: "PATCH", body: payload });
      return { id: updated.id, url: updated.html_url, updated: true };
    } catch (error) {
      // A comment someone else wrote with the marker cannot be edited.
      if (![403, 404].includes(error.status)) throw error;
    }
  }
  const created = await call(`${base}/repos/${repository}/issues/${number}/comments`, { method: "POST", body: payload });
  return { id: created.id, url: created.html_url, updated: false };
}

function conclusionResult(conclusion) {
  return { success: "pass", neutral: "neutral", failure: "fail", skipped: "skipped" }[conclusion] ?? "fail";
}

async function proofGate({ input, cli, env = process.env, output, summary, warn }) {
  const campaign = input("campaign").trim(),
    spec = input("spec").trim();
  if (Boolean(campaign) === Boolean(spec))
    throw new Error("proof-gate needs campaign (read a run's verdict) or spec (prove this pull request's change), not both");
  const pr = pullRequest(env);
  const number = input("pull-request").trim() ? Number(input("pull-request").trim()) : pr.number;
  const commentKey = input("comment-key").trim() || spec || "default";
  const artifactPath = input("output-path", "infercrane-delivery.json"),
    reportPath = input("report-path", "infercrane-proof-report.json");
  const wait = duration("proof-timeout", input("proof-timeout", "3h"));
  let runID = campaign,
    started = false;

  if (spec) {
    const directory = path.join(env.RUNNER_TEMP ?? require("node:os").tmpdir(), "infercrane-proof-gate");
    const current = currentSpec({ spec, currentSpecPath: input("current-spec").trim(), baseRef: input("base-ref").trim() || pr.baseSha, directory });
    const currentBytes = fs.readFileSync(current),
      proposedBytes = fs.readFileSync(spec);
    if (currentBytes.equals(proposedBytes)) {
      output("result", "skipped");
      output("conclusion", "skipped");
      output("verdict", "");
      summary(`## InferCrane proof gate · \`${commentKey}\`\n\n${spec} is unchanged from the current setup, so there is nothing to prove and nothing was spent.`);
      return;
    }
    const maxCost = Number(input("max-cost-usd").trim());
    if (!(maxCost > 0))
      throw new Error("max-cost-usd is required to run a proof: it caps what both setups' GPU servers may spend");
    const objective = input("objective", "interactive").trim() || "interactive";
    const compareArgs = ["optimize", "compare", current, spec, "--objective", objective, "--output", "json"];
    for (const [name, flagName] of [["profile", "--profile"], ["workload-fingerprint", "--workload-fingerprint"], ["quality-check", "--quality-check"], ["eval-set", "--eval-set"]]) {
      const value = input(name).trim();
      if (value) compareArgs.push(flagName, value);
    }
    // A retried step reuses its run; re-running the job measures again.
    compareArgs.push("--idempotency-key", idempotencyKey([env.GITHUB_REPOSITORY, number, env.GITHUB_RUN_ATTEMPT ?? "1", currentBytes, proposedBytes, ...compareArgs.slice(4)]));
    const created = cliJSON(cli, compareArgs, env).value;
    runID = created.campaign?.id;
    if (!runID) throw new Error("the control plane returned no run ID");
    started = true;
    output("campaign-id", runID);
    if (created.campaign.state === "awaiting_approval")
      cliJSON(cli, ["optimize", "approve", runID, "--max-cost-usd", String(maxCost), "--expires-in", duration("approval-window", input("approval-window", "3h")), "--yes", "--output", "json"], env);
  }

  let gate;
  try {
    const args = ["optimize", "gate", runID, "--wait", wait, "--on-watch", input("on-watch", "neutral").trim() || "neutral", "--comment-key", commentKey, "--save-report", reportPath, "--output", "json"];
    if (input("setup").trim()) args.push("--setup", input("setup").trim());
    if (flag(input("require-signature", "false"))) args.push("--require-signature");
    if (input("public-key").trim()) args.push("--public-key", input("public-key").trim());
    gate = cliJSON(cli, args, env).value;
  } finally {
    if (started && flag(input("release-servers", "true"))) {
      try {
        cliJSON(cli, ["optimize", "cancel", runID, "--output", "json"], env);
      } catch (error) {
        warn(`could not release the servers of run ${runID}: ${error.message}. They stop when its approval ends.`);
      }
    }
  }

  const { markdown, ...result } = gate;
  fs.writeFileSync(artifactPath, `${JSON.stringify(result, null, 2)}\n`, { mode: 0o600 });
  output("verdict", result.verdict);
  output("conclusion", result.conclusion);
  output("campaign-id", result.campaign_id ?? runID);
  output("report-digest", result.report_digest ?? "");
  output("result", conclusionResult(result.conclusion));
  summary(markdown);

  if (flag(input("comment", "true"))) {
    const token = input("github-token").trim();
    if (!token || !number) warn("no pull request or token to comment on; the verdict is in the job summary");
    else {
      try {
        const marker = markdown.split("\n", 1)[0];
        const comment = await upsertComment({ env, token, repository: env.GITHUB_REPOSITORY, number, marker, body: markdown });
        output("comment-url", comment.url ?? "");
      } catch (error) {
        warn(`could not comment on the pull request (${redact(error.message, env)}); a pull request from a fork gets a read-only token. The verdict is in the job summary.`);
      }
    }
  }

  const why = (result.reasons ?? [])[0] ?? result.headline;
  if (result.conclusion === "neutral") warn(`WATCH: ${why}`);
  if (result.conclusion !== "success" && result.conclusion !== "neutral")
    throw new Error(`proof gate ${result.verdict}: ${why}`);
}

module.exports = { currentSpec, idempotencyKey, proofGate, pullRequest, upsertComment };
