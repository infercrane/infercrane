"""A stand-in for `vllm serve` in a dry run: the same start-up log lines about the fused-MoE
kernel, /health, and streamed chat completions with usage. It writes one word a step at a
fixed pace, 4% faster when the tuned folder holds its file, so the dry run walks the whole
judging path. Nothing it reports is a measurement.

Usage: stand_in_server.py PORT MODEL (environment: VLLM_TUNED_CONFIG_FOLDER, STAND_IN_MOE_FILE)
"""

from __future__ import annotations

import json
import os
import re
import sys
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

port, model = int(sys.argv[1]), sys.argv[2]
name = os.environ.get("STAND_IN_MOE_FILE", "E=256,N=512,device_name=NVIDIA_H100_80GB_HBM3,dtype=fp8_w8a8,block_shape=[128,128].json")
folder = os.environ.get("VLLM_TUNED_CONFIG_FOLDER")
# A dry run's folder is a temporary one; the log names it as the container's mount would.
reported = os.environ.get("STAND_IN_REPORTED_FOLDER") or folder
shipped = "/stand-in/vllm/model_executor/layers/fused_moe/configs"
print("INFO 11-26 00:00:00 [fp8.py:502] Using TRITON Fp8 MoE backend out of potential backends: ['TRITON', 'DEEPGEMM'].", flush=True)
tuned = bool(folder) and os.path.exists(os.path.join(folder, name))
if tuned:
    print(f"INFO 11-26 00:00:01 [fused_moe.py:1240] Using configuration from {os.path.join(reported, name)} for MoE layer.", flush=True)
else:
    paths = ([os.path.join(reported, name)] if folder else []) + [os.path.join(shipped, name)]
    print(f"WARNING 11-26 00:00:01 [fused_moe.py:1254] Using default MoE config. Performance might be sub-optimal! Config file not found at {', '.join(paths)}", flush=True)
# Four words a chunk every 20 ms keeps a laptop's threads from setting the pace.
STEP, PER_CHUNK = 0.020 / (1.04 if tuned else 1.0), 4
QUESTION = re.compile(r"What is (\d+) modulo 7")


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *args) -> None:  # quiet
        return

    def do_GET(self) -> None:
        body = b"{}" if self.path == "/health" else b"not found"
        self.send_response(200 if self.path == "/health" else 404)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_POST(self) -> None:
        request = json.loads(self.rfile.read(int(self.headers.get("Content-Length", "0"))) or b"{}")
        content = request["messages"][-1]["content"]
        found = QUESTION.search(content)
        words = (["Working.", f"Answer: {int(found.group(1)) % 7}"] if found else ["word"] * int(request.get("max_tokens") or 16))
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.send_header("Connection", "close")
        self.end_headers()
        try:
            for start in range(0, len(words), PER_CHUNK):
                time.sleep(STEP)
                chunk = {"choices": [{"index": 0, "delta": {"content": " ".join(words[start:start + PER_CHUNK]) + " "}}]}
                self.wfile.write(b"data: " + json.dumps(chunk).encode() + b"\n\n")
                self.wfile.flush()
            usage = {"choices": [], "usage": {"prompt_tokens": len(content.split()), "completion_tokens": len(words)}}
            self.wfile.write(b"data: " + json.dumps(usage).encode() + b"\n\ndata: [DONE]\n\n")
            self.wfile.flush()
        except (BrokenPipeError, ConnectionResetError):
            return


ThreadingHTTPServer.daemon_threads = True
ThreadingHTTPServer.request_queue_size = 1024
ThreadingHTTPServer(("127.0.0.1", port), Handler).serve_forever()
