from __future__ import annotations

import json

import httpx
import pytest

from infercrane.gateway import RouteDirectory, RouteSnapshot, create_gateway_app


class BytesStream(httpx.AsyncByteStream):
    def __init__(self, content: bytes):
        self.content = content

    async def __aiter__(self):
        yield self.content


@pytest.fixture
def upstream_transport():
    async def handler(request: httpx.Request):
        payload = json.loads(request.content)
        assert payload["model"] == "Qwen/Qwen3-8B"
        if payload.get("stream"):
            return httpx.Response(
                200,
                headers={"content-type": "text/event-stream"},
                stream=BytesStream(
                    b'data: {"choices":[{"delta":{"content":"hello"}}]}\n\ndata: [DONE]\n\n'
                ),
            )
        return httpx.Response(
            200,
            headers={"content-type": "application/json"},
            stream=BytesStream(json.dumps({"model": payload["model"], "choices": []}).encode()),
        )

    return httpx.MockTransport(handler)


@pytest.fixture
def routes():
    directory = RouteDirectory()
    directory.put(
        RouteSnapshot(
            deployment_id="dep-1",
            alias="qwen-prod",
            upstream_model="Qwen/Qwen3-8B",
            router_url="http://router.internal",
        )
    )
    return directory


@pytest.mark.asyncio
async def test_alias_rewrite_and_non_streaming_forward(settings, routes, upstream_transport):
    upstream = httpx.AsyncClient(transport=upstream_transport)
    app = create_gateway_app(settings, routes, upstream)
    async with (
        app.router.lifespan_context(app),
        httpx.AsyncClient(transport=httpx.ASGITransport(app=app), base_url="http://test") as client,
    ):
        response = await client.post(
            "/v1/chat/completions",
            headers={"authorization": "Bearer infercrane"},
            json={"model": "qwen-prod", "messages": []},
        )
    await upstream.aclose()
    assert response.status_code == 200
    assert response.json()["model"] == "Qwen/Qwen3-8B"


@pytest.mark.asyncio
async def test_streaming_is_preserved(settings, routes, upstream_transport):
    upstream = httpx.AsyncClient(transport=upstream_transport)
    app = create_gateway_app(settings, routes, upstream)
    async with (
        app.router.lifespan_context(app),
        httpx.AsyncClient(transport=httpx.ASGITransport(app=app), base_url="http://test") as client,
        client.stream(
            "POST",
            "/v1/chat/completions",
            headers={"authorization": "Bearer infercrane"},
            json={"model": "qwen-prod", "messages": [], "stream": True},
        ) as response,
    ):
        body = b"".join([chunk async for chunk in response.aiter_bytes()])
    await upstream.aclose()
    assert response.status_code == 200
    assert b"data: [DONE]" in body
    assert response.headers["content-type"].startswith("text/event-stream")


@pytest.mark.asyncio
async def test_models_exposes_alias_and_auth_is_required(settings, routes, upstream_transport):
    upstream = httpx.AsyncClient(transport=upstream_transport)
    app = create_gateway_app(settings, routes, upstream)
    async with (
        app.router.lifespan_context(app),
        httpx.AsyncClient(transport=httpx.ASGITransport(app=app), base_url="http://test") as client,
    ):
        denied = await client.get("/v1/models")
        allowed = await client.get("/v1/models", headers={"authorization": "Bearer infercrane"})
    await upstream.aclose()
    assert denied.status_code == 401
    assert [item["id"] for item in allowed.json()["data"]] == ["qwen-prod"]


@pytest.mark.asyncio
async def test_unknown_alias_returns_openai_error(settings, routes, upstream_transport):
    upstream = httpx.AsyncClient(transport=upstream_transport)
    app = create_gateway_app(settings, routes, upstream)
    async with (
        app.router.lifespan_context(app),
        httpx.AsyncClient(transport=httpx.ASGITransport(app=app), base_url="http://test") as client,
    ):
        response = await client.post(
            "/v1/chat/completions",
            headers={"authorization": "Bearer infercrane"},
            json={"model": "missing", "messages": []},
        )
    await upstream.aclose()
    assert response.status_code == 404
    assert response.json()["error"]["type"] == "invalid_request_error"
