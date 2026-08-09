from __future__ import annotations

import asyncio
import json
import uuid
from contextlib import asynccontextmanager
from typing import Any

import httpx
from fastapi import FastAPI, Request
from fastapi.responses import JSONResponse, StreamingResponse

from infercrane.settings import Settings

from .routes import RouteDirectory

_HOP_BY_HOP = {
    "connection",
    "keep-alive",
    "proxy-authenticate",
    "proxy-authorization",
    "te",
    "trailers",
    "transfer-encoding",
    "upgrade",
    "content-length",
    "host",
}


def openai_error(message: str, status_code: int, error_type: str = "server_error") -> JSONResponse:
    return JSONResponse(
        status_code=status_code,
        content={"error": {"message": message, "type": error_type, "param": None, "code": None}},
    )


def create_gateway_app(
    settings: Settings,
    routes: RouteDirectory,
    client: httpx.AsyncClient | None = None,
    reconciler: Any | None = None,
) -> FastAPI:
    owned_client = client is None

    @asynccontextmanager
    async def lifespan(app: FastAPI):
        app.state.client = client or httpx.AsyncClient(
            timeout=httpx.Timeout(
                settings.upstream_read_timeout_seconds,
                connect=settings.upstream_connect_timeout_seconds,
                write=60,
                pool=10,
            ),
            limits=httpx.Limits(max_connections=512, max_keepalive_connections=128),
            transport=httpx.AsyncHTTPTransport(retries=0),
        )
        stop = asyncio.Event()
        reconcile_task = (
            asyncio.create_task(reconciler.run(stop), name="infercrane-reconciler")
            if reconciler
            else None
        )
        try:
            yield
        finally:
            stop.set()
            if reconcile_task:
                await reconcile_task
            if owned_client:
                await app.state.client.aclose()

    app = FastAPI(title="Inference deployment gateway", lifespan=lifespan)

    @app.get("/health")
    async def health():
        return {"status": "ok", "deployments": len(routes.list())}

    @app.get("/v1/models")
    async def models(request: Request):
        auth_error = _authorize(request, settings)
        if auth_error:
            return auth_error
        return {
            "object": "list",
            "data": [
                {"id": route.alias, "object": "model", "owned_by": "deployment"}
                for route in routes.list()
            ],
        }

    @app.post("/v1/chat/completions")
    async def chat_completions(request: Request):
        auth_error = _authorize(request, settings)
        if auth_error:
            return auth_error
        try:
            payload = await request.json()
        except (json.JSONDecodeError, UnicodeDecodeError):
            return openai_error("Invalid JSON body", 400, "invalid_request_error")
        if not isinstance(payload, dict) or not isinstance(payload.get("model"), str):
            return openai_error("The 'model' field is required", 400, "invalid_request_error")
        route = routes.get(payload["model"])
        if route is None:
            return openai_error(
                f"Unknown model alias: {payload['model']}", 404, "invalid_request_error"
            )

        forwarded = dict(payload)
        forwarded["model"] = route.upstream_model
        request_id = request.headers.get("x-request-id") or f"req_{uuid.uuid4().hex}"
        headers = {
            key: value
            for key, value in request.headers.items()
            if key.lower() not in _HOP_BY_HOP and key.lower() != "content-type"
        }
        headers["content-type"] = "application/json"
        headers["x-request-id"] = request_id
        upstream_request = request.app.state.client.build_request(
            "POST",
            f"{route.router_url.rstrip('/')}/v1/chat/completions",
            json=forwarded,
            headers=headers,
        )
        try:
            upstream = await request.app.state.client.send(upstream_request, stream=True)
        except httpx.TimeoutException:
            return openai_error("Inference upstream timed out", 504)
        except httpx.HTTPError:
            return openai_error("Inference upstream is unavailable", 503)

        response_headers = {
            key: value for key, value in upstream.headers.items() if key.lower() not in _HOP_BY_HOP
        }
        response_headers["x-request-id"] = request_id

        async def body():
            try:
                async for chunk in upstream.aiter_raw():
                    if await request.is_disconnected():
                        break
                    yield chunk
            finally:
                await upstream.aclose()

        return StreamingResponse(
            body(),
            status_code=upstream.status_code,
            headers=response_headers,
            media_type=upstream.headers.get("content-type"),
        )

    return app


def _authorize(request: Request, settings: Settings) -> JSONResponse | None:
    expected = f"Bearer {settings.api_key}"
    if request.headers.get("authorization") != expected:
        response = openai_error("Invalid API key", 401, "authentication_error")
        response.headers["www-authenticate"] = "Bearer"
        return response
    return None
