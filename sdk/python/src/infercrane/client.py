from __future__ import annotations

import asyncio
import json
import math
import os
import ssl
import time
import uuid
from collections.abc import AsyncIterator, Iterator
from typing import Any
from urllib.error import HTTPError, URLError
from urllib.parse import quote
from urllib.request import Request, urlopen

from .errors import (
    APIError,
    OperationCancelled,
    OperationFailed,
    OperationTimeout,
    StreamError,
)
from .generated.api import ControlAPI
from .generated.models import Deployment, Operation


def _control_url(value: str) -> str:
    value = value.rstrip("/")
    return value if value.endswith("/api/v1") else value + "/api/v1"


def _gateway_url(value: str) -> str:
    value = value.rstrip("/")
    return value.removesuffix("/api/v1")


class _Transport:
    def __init__(
        self,
        base_url: str,
        api_key: str,
        timeout: float,
        ssl_context: ssl.SSLContext | None = None,
    ) -> None:
        self.base_url = _control_url(base_url)
        self.api_key = api_key
        self.timeout = timeout
        self.ssl_context = ssl_context

    def request(
        self,
        method: str,
        path: str,
        *,
        body: Any | None = None,
        idempotency_key: str | None = None,
    ) -> Any:
        payload = (
            None if body is None else json.dumps(body, separators=(",", ":")).encode()
        )
        headers = {
            "Accept": "application/json",
            "Authorization": f"Bearer {self.api_key}",
            "User-Agent": "infercrane-python/1.0.0",
        }
        if payload is not None:
            headers["Content-Type"] = "application/json"
        if idempotency_key:
            headers["Idempotency-Key"] = idempotency_key
        request = Request(
            self.base_url + path, data=payload, headers=headers, method=method
        )
        try:
            with urlopen(
                request, timeout=self.timeout, context=self.ssl_context
            ) as response:
                content = response.read()
        except HTTPError as error:
            try:
                content = error.read()
            finally:
                error.close()
            try:
                detail = json.loads(content).get("error", {})
            except (json.JSONDecodeError, AttributeError):
                detail = {}
            raise APIError(
                error.code,
                detail.get("code", "http_error"),
                detail.get("message", error.reason),
                retryable=bool(detail.get("retryable")),
                remediation=detail.get("remediation", ""),
            ) from None
        except URLError as error:
            raise APIError(
                0, "transport_error", str(error.reason), retryable=True
            ) from error
        if not content:
            return None
        try:
            return json.loads(content)
        except json.JSONDecodeError as error:
            raise APIError(
                0, "invalid_response", "control plane returned invalid JSON"
            ) from error


class InferCrane:
    def __init__(
        self,
        *,
        api_key: str | None = None,
        base_url: str | None = None,
        gateway_url: str | None = None,
        timeout: float = 30.0,
        poll_interval: float = 1.0,
        ca_file: str | None = None,
        cert_file: str | None = None,
        key_file: str | None = None,
    ) -> None:
        resolved_key = api_key or os.getenv("INFERCRANE_API_KEY", "")
        if not resolved_key:
            raise ValueError("api_key or INFERCRANE_API_KEY is required")
        resolved_url = base_url or os.getenv(
            "INFERCRANE_CONTROL_URL", "http://127.0.0.1:18000"
        )
        if timeout <= 0 or poll_interval <= 0:
            raise ValueError("timeout and poll_interval must be positive")
        if bool(cert_file) != bool(key_file):
            raise ValueError("cert_file and key_file must be configured together")
        ssl_context = None
        if ca_file or cert_file:
            ssl_context = ssl.create_default_context(cafile=ca_file)
            if cert_file and key_file:
                ssl_context.load_cert_chain(certfile=cert_file, keyfile=key_file)
        self._ssl_context = ssl_context
        self._transport = _Transport(resolved_url, resolved_key, timeout, ssl_context)
        self.api = ControlAPI(self._transport)
        self.gateway_url = _gateway_url(gateway_url or resolved_url)
        self.api_key = resolved_key
        self.timeout = timeout
        self.poll_interval = poll_interval

    def deploy(
        self,
        *,
        model: str,
        name: str | None = None,
        endpoint_name: str | None = None,
        cloud: str,
        gpu: str,
        provider_adapter: str = "",
        runtime: str = "vllm",
        compute_mode: str = "elastic",
        min_replicas: int = 1,
        max_replicas: int = 1,
        region: str = "",
        model_revision: str = "",
        runtime_version: str = "",
        runtime_args: list[str] | None = None,
        workload: dict[str, Any] | None = None,
        idempotency_key: str | None = None,
    ) -> Operation:
        deployment_name = name or model.rsplit("/", 1)[-1].lower().replace("_", "-")
        body: dict[str, Any] = {
            "name": deployment_name,
            "endpoint_name": endpoint_name or deployment_name,
            "model": model,
            "runtime": runtime,
            "cloud": cloud,
            "gpu": gpu,
            "compute_mode": compute_mode,
            "min_replicas": min_replicas,
            "max_replicas": max_replicas,
        }
        if region:
            body["region"] = region
        if provider_adapter:
            body["provider_adapter"] = provider_adapter
        if model_revision:
            body["model_revision"] = model_revision
        if runtime_version:
            body["runtime_version"] = runtime_version
        if runtime_args:
            body["runtime_args"] = runtime_args
        if workload is not None:
            body["workload"] = workload
        result = self.api.create_deployment(
            body=body, idempotency_key=idempotency_key or f"sdk-deploy-{uuid.uuid4()}"
        )
        return Operation.from_dict(result["operation"])

    def get_operation(self, operation_id: str) -> Operation:
        return Operation.from_dict(self.api.get_operation(operation_id))

    def control_plane_instances(self) -> list[dict[str, Any]]:
        """Return live HA members and their mixed-version protocol intervals."""
        response = self.api.list_control_plane_instances()
        return list(response.get("data", []))

    def capture_recipe(
        self, deployment: str, *, name: str, version: str, benchmark_id: str = ""
    ) -> dict[str, Any]:
        body = {"name": name, "version": version}
        if benchmark_id:
            body["benchmark_id"] = benchmark_id
        return self.api.capture_recipe(deployment, body=body)["recipe"]

    def recipes(self, query: str = "", *, limit: int = 20) -> list[dict[str, Any]]:
        if limit < 1 or limit > 100:
            raise ValueError("limit must be between 1 and 100")
        return self._transport.request(
            "GET", f"/recipes?query={quote(query, safe='')}&limit={limit}"
        )["data"]

    def lab(
        self,
        model_identity: str,
        *,
        max_ttft_p95_ms: float | None = None,
        workload_digest: str = "",
    ) -> dict[str, Any]:
        body: dict[str, Any] = {"model_identity": model_identity}
        if max_ttft_p95_ms is not None:
            body["max_ttft_p95_ms"] = max_ttft_p95_ms
        if workload_digest:
            body["workload_digest"] = workload_digest
        return self.api.evaluate_lab(body=body)["evaluation"]

    def wait(self, operation_id: str, *, timeout: float | None = None) -> Operation:
        limit = self.timeout if timeout is None else timeout
        if limit <= 0:
            raise ValueError("timeout must be positive")
        deadline = time.monotonic() + limit
        while True:
            operation = self.get_operation(operation_id)
            if operation.status == "succeeded":
                return operation
            if operation.status == "failed":
                raise OperationFailed(operation)
            if operation.status == "cancelled":
                raise OperationCancelled(operation)
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                raise OperationTimeout(operation_id, limit)
            time.sleep(min(self.poll_interval, remaining))

    def cancel(self, operation_id: str) -> None:
        self.api.cancel_operation(operation_id)

    def get_deployment(self, name: str) -> Deployment:
        result = self.api.get_deployment(name)
        return Deployment.from_dict(result["deployment"])

    def delete(self, name: str, *, idempotency_key: str | None = None) -> Operation:
        result = self.api.delete_deployment(
            name, idempotency_key=idempotency_key or f"sdk-delete-{uuid.uuid4()}"
        )
        return Operation.from_dict(result["operation"])

    def set_slo_policy(self, deployment: str, **thresholds: float) -> dict[str, Any]:
        allowed = {
            "max_ttft_p95_ms",
            "max_latency_p95_ms",
            "max_error_rate",
            "min_output_tokens_second",
            "max_hourly_cost",
        }
        if not thresholds or set(thresholds) - allowed:
            raise ValueError("provide only supported SLO thresholds")
        if (
            any(not math.isfinite(value) or value < 0 for value in thresholds.values())
            or thresholds.get("max_error_rate", 0) > 1
        ):
            raise ValueError(
                "SLO thresholds must be finite, nonnegative, and error rate cannot exceed 1"
            )
        return self._transport.request(
            "PUT",
            f"/deployments/{quote(deployment, safe='')}/slo-policy",
            body=thresholds,
        )["policy"]

    def get_slo_policy(self, deployment: str) -> dict[str, Any]:
        return self._transport.request(
            "GET", f"/deployments/{quote(deployment, safe='')}/slo-policy"
        )["policy"]

    def delete_slo_policy(self, deployment: str) -> None:
        self._transport.request(
            "DELETE", f"/deployments/{quote(deployment, safe='')}/slo-policy"
        )

    def recommend(self, deployment: str) -> dict[str, Any]:
        return self._transport.request(
            "POST",
            f"/deployments/{quote(deployment, safe='')}/recommendations",
            body={},
        )["recommendation"]

    def recommendations(
        self, deployment: str, *, limit: int = 20
    ) -> list[dict[str, Any]]:
        if limit < 1 or limit > 100:
            raise ValueError("limit must be between 1 and 100")
        return self._transport.request(
            "GET",
            f"/deployments/{quote(deployment, safe='')}/recommendations?limit={limit}",
        )["data"]

    def stream_chat(
        self, deployment: str, messages: list[dict[str, str]], **parameters: Any
    ) -> Iterator[dict[str, Any]]:
        payload = {
            "model": deployment,
            "messages": messages,
            "stream": True,
            **parameters,
        }
        request = Request(
            self.gateway_url + "/v1/chat/completions",
            data=json.dumps(payload, separators=(",", ":")).encode(),
            headers={
                "Accept": "text/event-stream",
                "Authorization": f"Bearer {self.api_key}",
                "Content-Type": "application/json",
                "User-Agent": "infercrane-python/1.0.0",
            },
            method="POST",
        )
        try:
            response = urlopen(request, timeout=self.timeout, context=self._ssl_context)
        except HTTPError as error:
            try:
                detail = error.read().decode(errors="replace")
            finally:
                error.close()
            raise APIError(error.code, "inference_error", detail) from None
        completed = False
        try:
            for raw in response:
                line = raw.decode("utf-8").strip()
                if not line or line.startswith(":"):
                    continue
                if not line.startswith("data:"):
                    raise StreamError("inference stream contained an invalid SSE field")
                data = line[5:].strip()
                if data == "[DONE]":
                    completed = True
                    break
                try:
                    yield json.loads(data)
                except json.JSONDecodeError as error:
                    raise StreamError(
                        "inference stream contained invalid JSON"
                    ) from error
        finally:
            response.close()
        if not completed:
            raise StreamError("inference stream ended before [DONE]")


class AsyncInferCrane:
    def __init__(self, **options: Any) -> None:
        self._sync = InferCrane(**options)
        self.api = self._sync.api

    async def deploy(self, **options: Any) -> Operation:
        return await asyncio.to_thread(self._sync.deploy, **options)

    async def capture_recipe(self, deployment: str, **options: Any) -> dict[str, Any]:
        return await asyncio.to_thread(self._sync.capture_recipe, deployment, **options)

    async def recipes(
        self, query: str = "", *, limit: int = 20
    ) -> list[dict[str, Any]]:
        return await asyncio.to_thread(self._sync.recipes, query, limit=limit)

    async def lab(self, model_identity: str, **options: Any) -> dict[str, Any]:
        return await asyncio.to_thread(self._sync.lab, model_identity, **options)

    async def get_operation(self, operation_id: str) -> Operation:
        return await asyncio.to_thread(self._sync.get_operation, operation_id)

    async def control_plane_instances(self) -> list[dict[str, Any]]:
        return await asyncio.to_thread(self._sync.control_plane_instances)

    async def wait(
        self, operation_id: str, *, timeout: float | None = None
    ) -> Operation:
        return await asyncio.to_thread(self._sync.wait, operation_id, timeout=timeout)

    async def cancel(self, operation_id: str) -> None:
        await asyncio.to_thread(self._sync.cancel, operation_id)

    async def get_deployment(self, name: str) -> Deployment:
        return await asyncio.to_thread(self._sync.get_deployment, name)

    async def delete(
        self, name: str, *, idempotency_key: str | None = None
    ) -> Operation:
        return await asyncio.to_thread(
            self._sync.delete, name, idempotency_key=idempotency_key
        )

    async def set_slo_policy(
        self, deployment: str, **thresholds: float
    ) -> dict[str, Any]:
        return await asyncio.to_thread(
            self._sync.set_slo_policy, deployment, **thresholds
        )

    async def get_slo_policy(self, deployment: str) -> dict[str, Any]:
        return await asyncio.to_thread(self._sync.get_slo_policy, deployment)

    async def delete_slo_policy(self, deployment: str) -> None:
        await asyncio.to_thread(self._sync.delete_slo_policy, deployment)

    async def recommend(self, deployment: str) -> dict[str, Any]:
        return await asyncio.to_thread(self._sync.recommend, deployment)

    async def recommendations(
        self, deployment: str, *, limit: int = 20
    ) -> list[dict[str, Any]]:
        return await asyncio.to_thread(
            self._sync.recommendations, deployment, limit=limit
        )

    async def stream_chat(
        self, deployment: str, messages: list[dict[str, str]], **parameters: Any
    ) -> AsyncIterator[dict[str, Any]]:
        iterator = self._sync.stream_chat(deployment, messages, **parameters)
        sentinel = object()
        try:
            while True:
                item = await asyncio.to_thread(next, iterator, sentinel)
                if item is sentinel:
                    break
                yield item
        finally:
            await asyncio.to_thread(iterator.close)
