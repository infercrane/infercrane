"""Closed-loop load for one server: N workers, each sending the next prompt when its last
answer ends. Standard library only, so it runs in the engine image and on a laptop.

The measurement is the map's (tools/fast-track-map/loadgen.py on feat/fast-track-screen):
every request is one user message at temperature 0 with reasoning switched off, streamed
with usage; a warm-up runs first; output tokens per second counts the tokens that arrived
inside the window, spread over each answer's chunks; first-token and per-token times are
of requests sent inside the window. A failed request is counted, never retried.
"""

from __future__ import annotations

import http.client
import itertools
import json
import math
import threading
import time
from dataclasses import dataclass


@dataclass(frozen=True)
class Target:
    host: str
    port: int
    model: str


def chat_body(model: str, content: str, max_tokens: int) -> dict:
    return {"model": model, "messages": [{"role": "user", "content": content}], "max_tokens": max_tokens, "temperature": 0,
            "stream": True, "stream_options": {"include_usage": True}, "chat_template_kwargs": {"enable_thinking": False}}


def stream_request(target: Target, body: dict, stop: threading.Event | None = None, timeout: float = 600.0) -> dict:
    """One streamed request: when each content chunk arrived, the text, and the usage."""
    record = {"sent": time.perf_counter(), "chunks": [], "text": [], "tokens": None, "status": None, "error": None, "aborted": False}
    connection = http.client.HTTPConnection(target.host, target.port, timeout=timeout)
    try:
        connection.request("POST", "/v1/chat/completions", body=json.dumps(body).encode(),
                           headers={"Content-Type": "application/json", "Accept": "text/event-stream"})
        response = connection.getresponse()
        record["status"] = response.status
        if response.status != 200:
            record["error"] = response.read(500).decode(errors="replace")
        else:
            for line in response:
                if not line.startswith(b"data:"):
                    continue
                raw = line[5:].strip()
                if raw == b"[DONE]":
                    break
                try:
                    event = json.loads(raw)
                except ValueError:
                    continue
                if event.get("usage"):
                    record["tokens"] = event["usage"].get("completion_tokens")
                for choice in event.get("choices") or []:
                    piece = (choice.get("delta") or {}).get("content")
                    if piece:
                        record["chunks"].append(time.perf_counter())
                        record["text"].append(piece)
                if stop is not None and stop.is_set():
                    record["aborted"] = True
                    break
    except Exception as failure:  # noqa: BLE001 - a failed request is a counted error
        record["error"] = f"{type(failure).__name__}: {str(failure)[:200]}"
    finally:
        record["end"] = time.perf_counter()
        connection.close()
    record["text"] = "".join(record["text"])
    record["ok"] = record["status"] == 200 and record["error"] is None and not record["aborted"] and bool(record["tokens"])
    return record


def tokens_in_window(record: dict, start: float, end: float) -> float:
    """The answer's tokens spread evenly over its content chunks, counted inside [start, end)."""
    chunks, tokens = record["chunks"], record["tokens"] or 0
    if not chunks or tokens <= 0:
        return 0.0
    return tokens * sum(1 for stamp in chunks if start <= stamp < end) / len(chunks)


def percentile(values: list[float], share: float) -> float | None:
    if not values:
        return None
    ordered = sorted(values)
    return ordered[min(len(ordered) - 1, max(0, math.ceil(share * len(ordered)) - 1))]


def summarize(records: list[dict], start: float, end: float) -> dict:
    window = end - start
    sent_in = [r for r in records if start <= r["sent"] < end and not (r["aborted"] and r["error"] is None)]
    good = [r for r in sent_in if r["ok"]]
    ttft = [(r["chunks"][0] - r["sent"]) * 1000 for r in good if r["chunks"]]
    tpot = [(r["chunks"][-1] - r["chunks"][0]) * 1000 / (r["tokens"] - 1) for r in good if r["chunks"] and r["tokens"] > 1]
    latency = [(r["end"] - r["sent"]) * 1000 for r in good]
    third = window / 3
    return {
        "window_seconds": round(window, 3),
        "output_token_throughput": round(sum(tokens_in_window(r, start, end) for r in records) / window, 2),
        "output_token_throughput_by_third": [round(sum(tokens_in_window(r, start + i * third, start + (i + 1) * third) for r in records) / third, 2) for i in range(3)],
        "requests": len(sent_in), "failed": len(sent_in) - len(good),
        "ttft_p95_ms": _round(percentile(ttft, 0.95)), "tpot_p95_ms": _round(percentile(tpot, 0.95), 3), "latency_p95_ms": _round(percentile(latency, 0.95)),
        "error_examples": [str(r["error"])[:200] for r in sent_in if not r["ok"]][:3],
    }


def _round(value: float | None, digits: int = 1) -> float | None:
    return None if value is None else round(value, digits)


def run_level(target: Target, prompts: list[dict], concurrency: int, warmup_seconds: float, window_seconds: float,
              max_tokens: int, cooldown_max_seconds: float = 30.0) -> dict:
    """Holds `concurrency` requests at once over the prompts in order, measures the window
    after the warm-up, and keeps the same load until every request sent inside the window
    has ended (at most cooldown_max_seconds)."""
    counter, ids = itertools.count(), itertools.count()
    lock = threading.Lock()
    records: list[dict] = []
    in_flight: dict[int, float] = {}
    stop = threading.Event()

    def worker() -> None:
        while not stop.is_set():
            with lock:
                prompt = prompts[next(counter) % len(prompts)]
                number = next(ids)
                in_flight[number] = time.perf_counter()
            record = stream_request(target, chat_body(target.model, prompt["content"], prompt.get("max_tokens") or max_tokens), stop)
            with lock:
                in_flight.pop(number, None)
                records.append(record)
            if not record["ok"] and not record["aborted"]:
                time.sleep(0.2)  # a refusing server must not turn the loop into a busy spin

    origin = time.perf_counter()
    start, end = origin + warmup_seconds, origin + warmup_seconds + window_seconds
    threads = [threading.Thread(target=worker, daemon=True) for _ in range(concurrency)]
    for thread in threads:
        thread.start()
    time.sleep(max(0.0, end - time.perf_counter()))
    deadline = end + cooldown_max_seconds
    while time.perf_counter() < deadline:
        with lock:
            if not any(sent < end for sent in in_flight.values()):
                break
        time.sleep(0.05)
    with lock:
        unfinished = sum(1 for sent in in_flight.values() if sent < end)
    stop.set()
    for thread in threads:
        thread.join(timeout=15)
    with lock:
        taken = list(records)
    summary = summarize(taken, start, end)
    summary.update(concurrency=concurrency, warmup_seconds=warmup_seconds, unfinished_after_cooldown=unfinished,
                   prompts_used=next(counter), prompts_available=len(prompts))
    return summary


def answer_all(target: Target, items: list[dict], concurrency: int) -> list[dict]:
    """Asks every item once, `concurrency` at a time, and keeps each answer's text."""
    queue = list(enumerate(items))
    lock = threading.Lock()
    answers: list[dict | None] = [None] * len(items)

    def worker() -> None:
        while True:
            with lock:
                if not queue:
                    return
                index, item = queue.pop(0)
            record = stream_request(target, chat_body(target.model, item["prompt"], item["max_tokens"]))
            answers[index] = {"id": item["id"], "ok": record["ok"], "text": record["text"]}

    threads = [threading.Thread(target=worker, daemon=True) for _ in range(concurrency)]
    for thread in threads:
        thread.start()
    for thread in threads:
        thread.join()
    return [answer for answer in answers if answer is not None]
