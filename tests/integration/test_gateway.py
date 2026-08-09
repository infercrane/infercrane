from __future__ import annotations

import asyncio
import json

import httpx
import pytest
from openai import AsyncOpenAI
from sqlalchemy import select

from infercrane.domain.models import DeploymentCreate, TargetCreate
from infercrane.gateway import RouteDirectory, RouteSnapshot, create_gateway_app
from infercrane.persistence.models import RequestRecordRow


class BytesStream(httpx.AsyncByteStream):
    def __init__(self, content: bytes):
        self.content = content

    async def __aiter__(self):
        yield self.content


class DisconnectingStream(httpx.AsyncByteStream):
    async def __aiter__(self):
        yield b'data: {"choices":[{"delta":{"content":"partial"}}]}\n\n'
        raise httpx.ReadError("upstream disconnected")


class HangingStream(httpx.AsyncByteStream):
    def __init__(self, started: asyncio.Event):
        self.started = started

    async def __aiter__(self):
        self.started.set()
        yield b'data: {"choices":[{"delta":{"content":"partial"}}]}\n\n'
        await asyncio.Event().wait()


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
async def test_normal_openai_sdk_uses_logical_alias(settings, routes, upstream_transport):
    upstream = httpx.AsyncClient(transport=upstream_transport)
    app = create_gateway_app(settings, routes, upstream)
    transport = httpx.ASGITransport(app=app)
    http_client = httpx.AsyncClient(transport=transport, base_url="http://test")
    sdk = AsyncOpenAI(base_url="http://test/v1", api_key="infercrane", http_client=http_client)
    async with app.router.lifespan_context(app):
        response = await sdk.chat.completions.create(model="qwen-prod", messages=[])
    await sdk.close()
    await upstream.aclose()
    assert response.model == "Qwen/Qwen3-8B"


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


@pytest.mark.asyncio
async def test_timeout_is_safe_and_accounted(settings, database, control):
    control.add_target(TargetCreate(name="gpu-a", url="http://127.0.0.1:8101"))
    deployment = control.create_deployment(
        DeploymentCreate(name="qwen-prod", model="Qwen/Qwen3-8B", targets=["gpu-a"])
    )
    routes = RouteDirectory()
    routes.put(
        RouteSnapshot(
            deployment_id=deployment.id,
            alias="qwen-prod",
            upstream_model="Qwen/Qwen3-8B",
            router_url="http://router.internal",
        )
    )

    async def timeout(_request: httpx.Request):
        raise httpx.ConnectTimeout("timeout")

    upstream = httpx.AsyncClient(transport=httpx.MockTransport(timeout))
    app = create_gateway_app(settings, routes, upstream, database=database)
    async with (
        app.router.lifespan_context(app),
        httpx.AsyncClient(transport=httpx.ASGITransport(app=app), base_url="http://test") as client,
    ):
        response = await client.post(
            "/v1/chat/completions",
            headers={"authorization": "Bearer infercrane", "x-request-id": "req_timeout"},
            json={"model": "qwen-prod", "messages": []},
        )
    await upstream.aclose()
    assert response.status_code == 504
    with database.session() as session:
        record = session.scalar(
            select(RequestRecordRow).where(RequestRecordRow.request_id == "req_timeout")
        )
        assert record is not None
        assert record.error_type == "timeout"
        assert record.status_code == 504
        assert record.retry_count == 0


@pytest.mark.asyncio
async def test_upstream_disconnect_is_not_recorded_as_success(settings, database, control):
    control.add_target(TargetCreate(name="gpu-a", url="http://127.0.0.1:8101"))
    deployment = control.create_deployment(
        DeploymentCreate(name="qwen-prod", model="Qwen/Qwen3-8B", targets=["gpu-a"])
    )
    routes = RouteDirectory()
    routes.put(
        RouteSnapshot(
            deployment_id=deployment.id,
            alias="qwen-prod",
            upstream_model="Qwen/Qwen3-8B",
            router_url="http://router.internal",
        )
    )

    async def disconnect(_request: httpx.Request):
        return httpx.Response(
            200,
            headers={"content-type": "text/event-stream"},
            stream=DisconnectingStream(),
        )

    upstream = httpx.AsyncClient(transport=httpx.MockTransport(disconnect))
    app = create_gateway_app(settings, routes, upstream, database=database)
    with pytest.raises(httpx.ReadError):
        async with (
            app.router.lifespan_context(app),
            httpx.AsyncClient(
                transport=httpx.ASGITransport(app=app), base_url="http://test"
            ) as client,
        ):
            await client.post(
                "/v1/chat/completions",
                headers={"authorization": "Bearer infercrane", "x-request-id": "req_disconnect"},
                json={"model": "qwen-prod", "messages": [], "stream": True},
            )
    await upstream.aclose()
    with database.session() as session:
        record = session.get(RequestRecordRow, "req_disconnect")
        assert record is not None
        assert record.error_type == "upstream_disconnect"
        assert record.retry_count == 0


@pytest.mark.asyncio
async def test_client_cancellation_is_accounted(settings, database, control):
    control.add_target(TargetCreate(name="gpu-a", url="http://127.0.0.1:8101"))
    deployment = control.create_deployment(
        DeploymentCreate(name="qwen-prod", model="Qwen/Qwen3-8B", targets=["gpu-a"])
    )
    routes = RouteDirectory()
    routes.put(
        RouteSnapshot(
            deployment_id=deployment.id,
            alias="qwen-prod",
            upstream_model="Qwen/Qwen3-8B",
            router_url="http://router.internal",
        )
    )
    started = asyncio.Event()

    async def hanging(_request: httpx.Request):
        return httpx.Response(200, stream=HangingStream(started))

    upstream = httpx.AsyncClient(transport=httpx.MockTransport(hanging))
    app = create_gateway_app(settings, routes, upstream, database=database)
    async with (
        app.router.lifespan_context(app),
        httpx.AsyncClient(transport=httpx.ASGITransport(app=app), base_url="http://test") as client,
    ):
        request_task = asyncio.create_task(
            client.post(
                "/v1/chat/completions",
                headers={"authorization": "Bearer infercrane", "x-request-id": "req_cancel"},
                json={"model": "qwen-prod", "messages": [], "stream": True},
            )
        )
        await started.wait()
        request_task.cancel()
        with pytest.raises(asyncio.CancelledError):
            await request_task
    await upstream.aclose()
    with database.session() as session:
        record = session.get(RequestRecordRow, "req_cancel")
        assert record is not None
        assert record.error_type == "client_cancelled"
