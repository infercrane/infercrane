from __future__ import annotations

import argparse
import asyncio
import json

import uvicorn
from fastapi import FastAPI, Request
from fastapi.responses import StreamingResponse


def create_app(worker: str = "fake") -> FastAPI:
    app = FastAPI(title=f"fake-vllm-{worker}")

    @app.get("/health")
    async def health():
        return {"status": "ok", "worker": worker}

    @app.get("/v1/models")
    async def models():
        return {
            "object": "list",
            "data": [{"id": "Qwen/Qwen3-8B", "object": "model", "owned_by": "fake"}],
        }

    @app.get("/metrics")
    async def metrics():
        return (
            "vllm:num_requests_running 0\nvllm:num_requests_waiting 0\nvllm:kv_cache_usage_perc 0\n"
        )

    @app.post("/v1/chat/completions")
    async def chat(request: Request):
        payload = await request.json()
        if payload.get("stream"):

            async def chunks():
                data = {
                    "id": f"chatcmpl-{worker}",
                    "object": "chat.completion.chunk",
                    "model": payload["model"],
                    "choices": [{"index": 0, "delta": {"content": f"hello from {worker}"}}],
                }
                yield f"data: {json.dumps(data)}\n\n".encode()
                await asyncio.sleep(0)
                yield b"data: [DONE]\n\n"

            return StreamingResponse(chunks(), media_type="text/event-stream")
        return {
            "id": f"chatcmpl-{worker}",
            "object": "chat.completion",
            "model": payload["model"],
            "choices": [
                {
                    "index": 0,
                    "message": {"role": "assistant", "content": f"hello from {worker}"},
                    "finish_reason": "stop",
                }
            ],
        }

    return app


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--port", type=int, required=True)
    parser.add_argument("--worker", default="fake")
    args = parser.parse_args()
    uvicorn.run(create_app(args.worker), host="0.0.0.0", port=args.port)


if __name__ == "__main__":
    main()
