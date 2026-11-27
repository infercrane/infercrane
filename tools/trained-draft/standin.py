"""A stand-in for vLLM's OpenAI server in the trained-draft dry run ($0, no GPU).

It answers /health, /v1/models, /metrics and /v1/chat/completions (streamed or not,
with return_token_ids) with deterministic text paced at a chosen speed, and keeps
vLLM's speculation counters when told to accept drafts. It proves the harness's
plumbing; none of its numbers is a measurement.
"""

from __future__ import annotations

import argparse
import hashlib
import json
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

DRAFT_TOKENS = 3


class State:
    def __init__(self, model: str, behaviour: dict):
        self.model = model
        self.tokens_per_second = float(behaviour.get("tokens_per_second", 2000))
        self.answer_tokens = int(behaviour.get("answer_tokens", 0))
        self.accept = behaviour.get("accept")
        self.asleep = False
        self.lock = threading.Lock()
        self.drafts = self.draft_tokens = self.accepted = 0
        self.per_position = [0] * DRAFT_TOKENS

    def speculate(self, tokens: int) -> None:
        """Counts as vLLM would for tokens written at a fixed per-position acceptance."""
        if self.accept is None:
            return
        with self.lock:
            written = 0
            while written < tokens:
                self.drafts += 1
                self.draft_tokens += DRAFT_TOKENS
                accepted = 0
                for position in range(DRAFT_TOKENS):
                    # A deterministic pattern with the configured share.
                    if (self.drafts * (position + 3)) % 100 < self.accept * 100:
                        accepted += 1
                        self.per_position[position] += 1
                    else:
                        break
                self.accepted += accepted
                written += accepted + 1

    def metrics(self) -> str:
        lines = ["# stand-in", 'vllm:num_requests_running{model_name="%s"} 0' % self.model]
        if self.accept is not None:
            lines += [
                'vllm:spec_decode_num_drafts_total{model_name="%s"} %d' % (self.model, self.drafts),
                'vllm:spec_decode_num_draft_tokens_total{model_name="%s"} %d' % (self.model, self.draft_tokens),
                'vllm:spec_decode_num_accepted_tokens_total{model_name="%s"} %d' % (self.model, self.accepted),
            ]
            lines += ['vllm:spec_decode_num_accepted_tokens_per_pos_total{model_name="%s",position="%d"} %d' % (self.model, i, n) for i, n in enumerate(self.per_position)]
        return "\n".join(lines) + "\n"


def answer_for(content: str, limit: int, fixed: int) -> list[str]:
    seed = hashlib.sha256(content.encode()).hexdigest()
    count = min(limit, fixed or 16 + int(seed[:4], 16) % 48)
    words = [f"w{seed[i % 60:i % 60 + 4]}" for i in range(count - 1)]
    return words + ["Answer: 42"] if count > 1 else ["Answer: 42"]


def make_handler(state: State):
    class Handler(BaseHTTPRequestHandler):
        protocol_version = "HTTP/1.1"

        def log_message(self, *_):
            pass

        def send(self, code: int, body: str, kind: str = "application/json") -> None:
            data = body.encode()
            self.send_response(code)
            self.send_header("Content-Type", kind)
            self.send_header("Content-Length", str(len(data)))
            self.end_headers()
            self.wfile.write(data)

        def do_GET(self):
            if self.path == "/health":
                self.send(200, "{}")
            elif self.path == "/v1/models":
                self.send(200, json.dumps({"object": "list", "data": [{"id": state.model, "object": "model"}]}))
            elif self.path == "/metrics":
                self.send(200, state.metrics(), "text/plain")
            else:
                self.send(404, "{}")

        def do_POST(self):
            if self.path.startswith("/sleep") or self.path == "/wake_up":
                self.rfile.read(int(self.headers.get("Content-Length", 0)))
                state.asleep = self.path.startswith("/sleep")
                self.send(200, "{}")
                return
            body = json.loads(self.rfile.read(int(self.headers.get("Content-Length", 0))) or b"{}")
            if state.asleep:
                self.send(503, json.dumps({"error": "asleep"}))
                return
            if self.path != "/v1/chat/completions" or body.get("model") != state.model:
                self.send(404, json.dumps({"error": "unknown model or path"}))
                return
            content = body["messages"][-1]["content"]
            words = answer_for(content, int(body.get("max_tokens") or 64), state.answer_tokens)
            prompt_ids = [int(hashlib.sha256(f"{content}:{i}".encode()).hexdigest()[:6], 16) % 151000 for i in range(8 + len(content) // 20)]
            answer_ids = [int(hashlib.sha256(w.encode()).hexdigest()[:6], 16) % 151000 for w in words]
            state.speculate(len(words))
            if not body.get("stream"):
                time.sleep(len(words) / state.tokens_per_second)
                payload = {"id": "standin", "object": "chat.completion", "model": state.model,
                           "choices": [{"index": 0, "message": {"role": "assistant", "content": " ".join(words)}, "finish_reason": "stop"}],
                           "usage": {"prompt_tokens": len(prompt_ids), "completion_tokens": len(words), "total_tokens": len(prompt_ids) + len(words)}}
                if body.get("return_token_ids"):
                    payload["prompt_token_ids"] = prompt_ids
                    payload["choices"][0]["token_ids"] = answer_ids
                self.send(200, json.dumps(payload))
                return
            self.send_response(200)
            self.send_header("Content-Type", "text/event-stream")
            self.send_header("Transfer-Encoding", "chunked")
            self.end_headers()

            def chunk(payload: str) -> None:
                data = payload.encode()
                self.wfile.write(f"{len(data):X}\r\n".encode() + data + b"\r\n")
                self.wfile.flush()

            for index, word in enumerate(words):
                # Pace in groups of eight tokens: one sleep per token would put the
                # laptop's timer jitter, not the configured speed, into every run.
                if index % 8 == 0:
                    time.sleep(min(8, len(words) - index) / state.tokens_per_second)
                piece = word if index == 0 else " " + word
                chunk("data: " + json.dumps({"choices": [{"index": 0, "delta": {"content": piece}}]}) + "\n\n")
            chunk("data: " + json.dumps({"choices": [], "usage": {"prompt_tokens": len(prompt_ids), "completion_tokens": len(words)}}) + "\n\n")
            chunk("data: [DONE]\n\n")
            self.wfile.write(b"0\r\n\r\n")

    return Handler


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--port", type=int, required=True)
    parser.add_argument("--model", required=True)
    parser.add_argument("--behaviour", default="{}")
    options = parser.parse_args()
    # The default listen queue of 5 refuses most of 64 dials at once (the poc-modal stand-in's lesson).
    ThreadingHTTPServer.request_queue_size = 2048
    server = ThreadingHTTPServer(("127.0.0.1", options.port), make_handler(State(options.model, json.loads(options.behaviour))))
    server.daemon_threads = True
    server.serve_forever()


if __name__ == "__main__":
    main()
