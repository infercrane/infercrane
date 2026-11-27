"""A stand-in for vLLM's hidden-state extraction server, for the trained-draft dry run ($0).

It answers what speculators' trainer asks a vLLM server started with `extract_hidden_states` and
`ExampleHiddenStatesConnector` (vLLM 0.34, docs/features/speculative_decoding/extract_hidden_states.md):
POST /v1/completions with the prompt as token ids and max_tokens 1, answered with the prompt's
token ids and `kv_transfer_params.hidden_states_path`, a safetensors file holding `hidden_states`
[tokens, layers, hidden] and `token_ids`. The hidden states are the real model's, computed with
transformers on this machine: the input to each listed layer, and for the last id the final
layer's output before its norm, as vLLM writes them. Slow, and only for proving the trainer's
online path before a GPU run.
"""

from __future__ import annotations

import argparse
import json
import threading
import uuid
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--port", type=int, required=True)
    parser.add_argument("--model", required=True)
    parser.add_argument("--served", required=True)
    parser.add_argument("--layers", required=True, help="comma-separated layer ids; the last may equal the layer count")
    parser.add_argument("--storage", required=True)
    options = parser.parse_args()

    import torch
    from safetensors.torch import save_file
    from transformers import AutoModelForCausalLM

    device = "mps" if torch.backends.mps.is_available() else "cpu"
    model = AutoModelForCausalLM.from_pretrained(options.model, dtype=torch.bfloat16).to(device).eval()
    layers = [int(x) for x in options.layers.split(",")]
    count = len(model.model.layers)
    storage = Path(options.storage)
    storage.mkdir(parents=True, exist_ok=True)
    lock = threading.Lock()
    final: dict = {}
    model.model.layers[-1].register_forward_hook(lambda _m, _i, output: final.__setitem__("h", output[0] if isinstance(output, tuple) else output))

    def extract(token_ids: list[int]) -> str:
        with lock, torch.no_grad():
            tokens = torch.tensor([token_ids], device=device)
            result = model(input_ids=tokens, output_hidden_states=True)
            picked = [final["h"][0] if layer == count else result.hidden_states[layer][0] for layer in layers]
            path = storage / f"{uuid.uuid4().hex}.safetensors"
            save_file({"hidden_states": torch.stack(picked, dim=1).to(torch.bfloat16).cpu().contiguous(), "token_ids": tokens[0].cpu().to(torch.long)}, str(path))
            return str(path)

    class Handler(BaseHTTPRequestHandler):
        protocol_version = "HTTP/1.1"

        def log_message(self, *_):
            pass

        def send(self, code: int, body: dict) -> None:
            data = json.dumps(body).encode()
            self.send_response(code)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(data)))
            self.end_headers()
            self.wfile.write(data)

        def do_GET(self):
            if self.path == "/health":
                self.send(200, {})
            elif self.path == "/v1/models":
                self.send(200, {"object": "list", "data": [{"id": options.served, "object": "model", "created": 0, "owned_by": "standin"}]})
            else:
                self.send(404, {})

        def do_POST(self):
            body = json.loads(self.rfile.read(int(self.headers.get("Content-Length", 0))) or b"{}")
            if self.path != "/v1/completions" or body.get("model") != options.served or not isinstance(body.get("prompt"), list):
                self.send(400, {"error": {"message": "the stand-in answers token-id completions for the served model only"}})
                return
            token_ids = [int(x) for x in body["prompt"]]
            path = extract(token_ids)
            self.send(200, {
                "id": "cmpl-standin", "object": "text_completion", "created": 0, "model": options.served,
                "choices": [{"index": 0, "text": "", "finish_reason": "length", "logprobs": None, "prompt_token_ids": token_ids, "token_ids": [0]}],
                "usage": {"prompt_tokens": len(token_ids), "completion_tokens": 1, "total_tokens": len(token_ids) + 1},
                "kv_transfer_params": {"hidden_states_path": path},
            })

    ThreadingHTTPServer.request_queue_size = 512
    server = ThreadingHTTPServer(("127.0.0.1", options.port), Handler)
    server.daemon_threads = True
    server.serve_forever()


if __name__ == "__main__":
    main()
