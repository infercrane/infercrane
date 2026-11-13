#!/usr/bin/env python3
"""Full-screen terminal panes for recording the local PoC, one view per window.

    panes.py hardware     node CPU and memory, and CPU and memory per serving pod
    panes.py requests     tokens per second, running and waiting requests, per pod
    panes.py bottleneck   where time goes per pod, the diagnosis, and what to try
    panes.py results      campaign progress and each setup against the current one

Every number comes from a real source: pod /metrics through the Kubernetes API,
container stats from the kind node, and the InferCrane control API. Missing
values show as "-". The laptop has no GPU, and the hardware pane says so.
"""

from __future__ import annotations

import importlib.util
import json
import shutil
import subprocess
import sys
import time
from pathlib import Path

HERE = Path(__file__).resolve().parent
spec = importlib.util.spec_from_file_location("poclive", HERE / "live.py")
live = importlib.util.module_from_spec(spec)
sys.modules["poclive"] = live
spec.loader.exec_module(live)
monitor = live.monitor

B, DIM, R = "\x1b[1m", "\x1b[2m", "\x1b[0m"
GREEN, AMBER, RED, PURPLE, BLUE, GRAY = "\x1b[38;5;36m", "\x1b[38;5;214m", "\x1b[38;5;203m", "\x1b[38;5;141m", "\x1b[38;5;75m", "\x1b[38;5;245m"
STAGES = ["proposed", "provisioning", "ready", "measuring", "validating", "ranked", "qualified"]
STATE_WORDS = {"proposed": "queued", "provisioning": "starting a server", "ready": "server ready", "measuring": "measuring vs your current setup",
               "validating": "checking answer quality", "ranked": "measured", "qualified": "winner", "rejected": "rejected", "inconclusive": "no clear result",
               "failed": "failed", "cancelled": "cancelled", "cleaned": "server removed"}
REASON_WORDS = {"measured_no_measurable_difference": "no real difference", "release_guard_rejected": "failed the safety check", "quality_failed": "answers got worse"}


def width() -> int:
    return shutil.get_terminal_size((80, 24)).columns


def bar(fraction: float | None, size: int, color: str) -> str:
    if fraction is None:
        return GRAY + "·" * size + R
    filled = max(0, min(size, round(fraction * size)))
    return color + "█" * filled + R + DIM + "░" * (size - filled) + R


def fmt(value: float | None, digits: int = 0, unit: str = "") -> str:
    return "-" if value is None else f"{value:,.{digits}f}{unit}"


def draw(title: str, subtitle: str, lines: list[str]) -> None:
    out = ["\x1b[H\x1b[2J", f"{B}{title}{R}  {DIM}{time.strftime('%H:%M:%S')}{R}", f"{DIM}{subtitle}{R}", ""] + lines
    sys.stdout.write("\n".join(out) + "\n")
    sys.stdout.flush()


def labels() -> dict:
    try:
        _, _, mapping = live.collect_campaign()
        return mapping
    except Exception:  # noqa: BLE001 - control plane not reachable yet
        return {}


def pod_label(name: str, mapping: dict) -> str:
    for service, label in mapping.items():
        if name.startswith(service):
            return label
    return name.removeprefix("infercrane-")[:12]


def container_stats() -> dict:
    completed = subprocess.run(["docker", "exec", live.NODE, "crictl", "stats", "-o", "json"], capture_output=True, text=True, timeout=10)
    usage = {}
    for stat in json.loads(completed.stdout or "{}").get("stats", []):
        attributes = stat.get("attributes", {})
        pod = attributes.get("labels", {}).get("io.kubernetes.pod.name", "")
        cpu = ((stat.get("cpu") or {}).get("usageCoreNanoSeconds") or {}).get("value")
        memory = ((stat.get("memory") or {}).get("workingSetBytes") or {}).get("value")
        if pod:
            usage[pod] = {"cpu_ns": float(cpu) if cpu is not None else None, "memory": float(memory) if memory is not None else None}
    return usage


def hardware() -> None:
    previous, previous_time = {}, 0.0
    while True:
        mapping = labels()
        node = live.collect_node()
        now = time.time()
        try:
            stats = container_stats()
        except Exception:  # noqa: BLE001
            stats = {}
        cores = node.get("cpus") or 1
        lines = [f"{B}Node{R}  {node.get('kind', 'node')}",
                 f"  CPU     {bar(node.get('cpu_percent', 0) / 100 / cores, 30, AMBER)}  {fmt(node.get('cpu_percent', 0) / cores, 0, '%')} of {cores} cores",
                 f"  Memory  {bar((node.get('memory_percent') or 0) / 100, 30, BLUE)}  {node.get('memory', '-')}", "", f"{B}Per serving pod{R}"]
        for pod, value in sorted(stats.items()):
            if not pod.startswith("infercrane-") or len(pod) < 30:
                continue
            last = previous.get(pod, {}).get("cpu_ns")
            used = (value["cpu_ns"] - last) / 1e9 / (now - previous_time) if value["cpu_ns"] is not None and last is not None and now > previous_time else None
            memory_gb = value["memory"] / 1e9 if value["memory"] else None
            lines.append(f"  {pod_label(pod, mapping)[:34]:<34}")
            lines.append(f"    CPU {bar(None if used is None else used / cores, 22, AMBER)} {fmt(used, 1)} cores   mem {fmt(memory_gb, 2, ' GB')}")
        lines += ["", f"{DIM}No GPU on this laptop: these are CPU numbers. On a GPU cluster this pane shows GPU, tensor-core and memory-bandwidth activity.{R}"]
        draw("Hardware tracker", "kind node and serving pods, live", lines)
        previous, previous_time = stats, now
        time.sleep(2)


def pod_rows(state: dict) -> dict:
    state["pods"] = live.collect_pods(state.get("pods", {}))
    return state["pods"]


def requests() -> None:
    state: dict = {}
    while True:
        mapping = labels()
        lines = []
        for name, pod in pod_rows(state).items():
            series = pod.get("series") or []
            sample = series[-1] if series else {}
            history = [s.get("output_tokens") or 0 for s in series]
            waiting = sample.get("waiting")
            lines += [f"{B}{pod_label(name, mapping)}{R} {DIM}{sample.get('engine', pod.get('phase', ''))}{R}",
                      f"  output  {GREEN}{monitor.sparkline(history, 28)}{R} {fmt(sample.get('output_tokens'), 1)} tok/s",
                      f"  prompt  {fmt(sample.get('prompt_tokens'), 1)} tok/s   running {B}{fmt(sample.get('running'))}{R}   waiting {(RED if (waiting or 0) > 0 else '')}{fmt(waiting)}{R}", ""]
        draw("Live requests", "what each serving pod is doing right now", lines or ["no serving pods yet"])
        time.sleep(2)


def diagnose(sample: dict) -> tuple[str, str]:
    prefill, decode, waiting = sample.get("prefill") or 0, sample.get("decode") or 0, sample.get("waiting") or 0
    total = prefill + decode
    if total < 0.05 and not waiting:
        return "idle", "-"
    if waiting and total and decode / total > 0.6:
        return "busy writing answers, and requests are waiting in line", "serve more requests at once; move less memory per token"
    if total and prefill / total > 0.6:
        return "busy reading long prompts", "read prompts in bigger batches; reuse shared prompt prefixes"
    if total and decode / total > 0.6:
        return "busy writing answers", "move less memory per token (compact KV cache), speculative decoding"
    return "split between reading prompts and writing answers", "smooth the mix (chunked prompt reading)"


def bottleneck() -> None:
    state: dict = {}
    while True:
        mapping = labels()
        lines = []
        for name, pod in pod_rows(state).items():
            series = pod.get("series") or []
            recent = series[-5:]
            sample = {key: sum((s.get(key) or 0) for s in recent) / max(1, len(recent)) for key in ("prefill", "decode", "waiting", "running")}
            total = sample["prefill"] + sample["decode"]
            verdict, advice = diagnose(sample)
            kv = series[-1].get("kv_usage") if series else None
            lines += [f"{B}{pod_label(name, mapping)}{R}",
                      f"  reading prompts {bar(sample['prefill'] / total if total else None, 22, PURPLE)} {fmt(100 * sample['prefill'] / total if total else None, 0, '%')}",
                      f"  writing answers {bar(sample['decode'] / total if total else None, 22, GREEN)} {fmt(100 * sample['decode'] / total if total else None, 0, '%')}",
                      f"  waiting in line {fmt(sample['waiting'], 1)} requests",
                      f"  {AMBER}what's slow  {R} {verdict}", f"  {GREEN}worth trying {R} {advice}", ""]
        draw("Bottleneck analysis", "where each server spends its time (last 10 seconds)", lines or ["no serving pods yet"])
        time.sleep(2)


def product_watch(campaign_id: str) -> None:
    """Hands the pane to the real CLI command a customer would run."""
    cli = live.ROOT / ".infercrane" / "poc" / "bin" / "infercrane-watch"
    if not cli.exists():
        return
    import os
    key = json.loads(live.STATE_FILE.read_text())["api_key"]
    env = dict(os.environ, INFERCRANE_URL=live.API, INFERCRANE_API_KEY=key, INFERCRANE_CONTEXT="")
    os.execve(str(cli), [str(cli), "optimize", "watch", campaign_id], env)


def results() -> None:
    while True:
        campaign_id = json.loads(live.STATE_FILE.read_text()).get("campaign")
        if campaign_id and not json.loads(live.STATE_FILE.read_text()).get("result"):
            product_watch(campaign_id)
        current = json.loads(live.STATE_FILE.read_text())
        card = current.get("result")
        if card:
            draw("Result", "measured on your replayed traffic", [f"{B}{line}{R}" if line.startswith("Decision") else line for line in card])
            time.sleep(5)
            continue
        try:
            campaign, rows, _ = live.collect_campaign()
        except Exception as error:  # noqa: BLE001
            draw("Results", f"waiting for the control plane: {error}", [])
            time.sleep(3)
            continue
        plan = current.get("plan") or []
        lines = []
        if not campaign:
            if plan:
                lines.append(f"{B}Planned setups{R}")
                lines += [f"  • {item['label']}" for item in plan]
            draw("Results", "each setup is measured against your current setup on the same traffic", lines or ["waiting for this run's campaign…"])
            time.sleep(3)
            continue
        lines.append(f"{B}Progress{R}")
        for candidate in campaign.get("candidates", []):
            index = STAGES.index(candidate["state"]) if candidate.get("state") in STAGES else -1
            failed = candidate.get("state") in ("rejected", "failed", "inconclusive", "cancelled")
            track = "".join((RED if failed and i >= max(index, 0) else GREEN if i < index else AMBER if i == index else DIM) + "■" + R for i in range(len(STAGES)))
            status = STATE_WORDS.get(candidate.get("state", ""), candidate.get("state", ""))
            reason = REASON_WORDS.get(candidate.get("failure_code") or "", (candidate.get("failure_code") or "").replace("_", " "))
            lines.append(f"  {track} {candidate.get('label', '')[:36]:<36} {status}{(' · ' + reason) if reason else ''}")
        lines.append("")
        if rows:
            peak = max((row.get("output_tps") or 0) for row in rows) or 1
            base = rows[0].get("output_tps")
            lines.append(f"{B}Measured so far{R}")
            for index, row in enumerate(rows):
                tps = row.get("output_tps")
                if index == 0:
                    verdict = f"{GRAY}your current setup{R}"
                elif tps and base:
                    change = (tps / base - 1) * 100
                    color = GREEN if change > 0 else RED
                    verdict = f"{color}{change:+.0f}% throughput{R}"
                else:
                    verdict = f"{DIM}measuring…{R}"
                lines.append(f"  {row['label'][:36]:<36} {bar((tps or 0) / peak, 16, GREEN if index else GRAY)} {fmt(tps, 1)} tokens/s · {verdict}")
        draw("Results", "each setup is measured against your current setup on the same traffic", lines)
        time.sleep(3)


TIMING = __import__("re").compile(r"task (\d+) \|\s+(prompt eval time|eval time|total time) =\s+([\d.]+) ms /\s+(\d+) tokens")


def request_timings(pod: str) -> list[dict]:
    """Per-request prompt and answer times from the server's own timing log."""
    try:
        text = live.kubectl("-n", live.NAMESPACE, "logs", pod, "--since=120s", timeout=6)
    except Exception:  # noqa: BLE001
        return []
    tasks: dict[str, dict] = {}
    order: list[str] = []
    for match in TIMING.finditer(text):
        task, kind, ms, tokens = match.group(1), match.group(2), float(match.group(3)), int(match.group(4))
        if task not in tasks:
            tasks[task] = {}
            order.append(task)
        key = {"prompt eval time": "prompt", "eval time": "answer", "total time": "total"}[kind]
        tasks[task][key] = (ms, tokens)
    return [dict(task=t, **tasks[t]) for t in order if "total" in tasks[t]][-8:]


def engine() -> None:
    state: dict = {}
    cache_last: dict = {}
    while True:
        mapping = labels()
        lines = []
        for name, pod in pod_rows(state).items():
            series = pod.get("series") or []
            label = pod_label(name, mapping)
            try:
                values = monitor.parse_metrics(live.kubectl("get", "--raw", f"/api/v1/namespaces/{live.NAMESPACE}/pods/{name}:8080/proxy/metrics", timeout=5))
            except Exception:  # noqa: BLE001
                values = {}
            total, cached = values.get("llamacpp:prompt_tokens_total"), values.get("llamacpp:prompt_tokens_cached_total")
            hit = cached / total if total and cached is not None else None
            queue = [s.get("waiting") or 0 for s in series]
            lines.append(f"{B}{label}{R}")
            lines.append(f"  prompt cache hits {bar(hit, 18, BLUE)} {fmt(None if hit is None else hit * 100, 0, '%')} of prompt tokens reused")
            lines.append(f"  queue depth       {AMBER}{monitor.sparkline(queue, 24)}{R} now {fmt(queue[-1] if queue else None)}")
            timings = request_timings(name)
            if timings:
                longest = max(t["total"][0] for t in timings) or 1
                lines.append(f"  {DIM}last requests: {PURPLE}■{R}{DIM} reading prompt  {GREEN}■{R}{DIM} writing answer  (server timing log){R}")
                for t in timings:
                    prompt_ms, prompt_tokens = t.get("prompt", (0.0, 0))
                    answer_ms, answer_tokens = t.get("answer", (0.0, 0))
                    width = 26
                    a = round(prompt_ms / longest * width)
                    b = round(answer_ms / longest * width)
                    lines.append(f"  #{t['task']:>5} {PURPLE}{'█' * a}{R}{GREEN}{'█' * b}{R}{' ' * max(0, width - a - b)} {prompt_tokens:>5} in / {answer_tokens:>3} out · {t['total'][0] / 1000:5.1f} s")
            lines.append("")
        draw("Engine internals", "per-request timing, prompt cache and queue, straight from each server", lines or ["no serving pods yet"])
        time.sleep(3)


def method() -> None:
    """The measurement method, live: which side is under load, then the evidence."""
    state: dict = {}
    timeline: list[str] = []
    while True:
        mapping = labels()
        try:
            campaign, rows, _ = live.collect_campaign()
        except Exception:  # noqa: BLE001
            campaign, rows = None, []
        lines = [f"{B}Rule{R}  the current setup (C) and the candidate (X) take turns on the same replayed traffic:",
                 f"      warm-up, then {B}C X C X C X C{R}. A candidate wins only if it beats C by more than the noise,",
                 f"      and the comparison is void if C itself drifts more than 5% between its runs.", ""]
        measuring = [c for c in (campaign or {}).get("candidates", []) if c.get("state") == "measuring"]
        pods = pod_rows(state)
        if measuring:
            target = measuring[0]
            control_label = ((campaign or {}).get("candidates") or [{}])[0].get("label", "current setup")
            loads = {}
            for name, pod in pods.items():
                sample = (pod.get("series") or [{}])[-1]
                loads[pod_label(name, mapping)] = (sample.get("output_tokens") or 0) + (sample.get("prompt_tokens") or 0)
            active = max(loads, key=loads.get) if loads and max(loads.values()) > 1 else None
            arm = "C" if active == control_label else ("X" if active == target.get("label") else None)
            if arm and (not timeline or timeline[-1] != arm):
                timeline.append(arm)
            lines.append(f"{B}Now measuring{R} {target.get('label')}")
            lines.append("  order so far  " + " ".join((GRAY + "C" + R) if a == "C" else (GREEN + "X" + R) for a in timeline) + (f"   {AMBER}← running {arm}{R}" if arm else ""))
            lines.append("")
        else:
            timeline = []
        for row in rows[1:]:
            measured = row.get("measured") or {}
            runs = measured.get("runs") or []
            control = measured.get("control") or {}
            comparison = (measured.get("comparison") or {}).get("output_token_throughput") or {}
            if not runs:
                continue
            lines.append(f"{B}Evidence{R} {row['label']}")
            for run in runs:
                arm = run.get("arm", "")
                tag = GRAY + "C" + R if arm.startswith("control") else GREEN + "X" + R if arm.startswith("candidate") else DIM + arm[:6] + R
                lines.append(f"  {tag} {fmt(run.get('output_token_throughput'), 1):>7} tok/s · p95 latency {fmt(run.get('latency_p95_ms')):>6} ms · {run.get('requests', 0)} requests")
            drift, limit = control.get("max_adjacent_output_throughput_drift"), control.get("drift_limit")
            if drift is not None:
                lines.append(f"  control drift {fmt(drift * 100, 1, '%')} (limit {fmt((limit or 0.05) * 100, 0, '%')}) {'valid' if drift <= (limit or 0.05) else RED + 'comparison void' + R}")
            if comparison:
                lines.append(f"  throughput delta {comparison.get('delta', 0) * 100:+.1f}% vs noise floor ±{comparison.get('noise_floor', 0) * 100:.1f}% "
                             f"(control spread {comparison.get('control_spread', 0) * 100:.1f}%, candidate uncertainty {comparison.get('candidate_uncertainty', 0) * 100:.1f}%)")
                decision = comparison.get("decision", "")
                color = GREEN if decision == "improved" else RED if decision == "regressed" else GRAY
                lines.append(f"  decision {color}{decision.replace('_', ' ')}{R}")
            lines.append("")
        if not campaign:
            lines.append(f"{DIM}waiting for this run's campaign…{R}")
        draw("Measurement method", "how InferCrane decides, with the numbers behind each decision", lines)
        time.sleep(2)


def main() -> int:
    views = {"hardware": hardware, "requests": requests, "bottleneck": bottleneck, "results": results, "engine": engine, "method": method}
    if len(sys.argv) != 2 or sys.argv[1] not in views:
        print(__doc__)
        return 2
    try:
        views[sys.argv[1]]()
    except KeyboardInterrupt:
        return 0
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
