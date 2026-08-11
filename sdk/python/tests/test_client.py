import asyncio
import json
import os
import sys
import threading
import unittest
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", "src"))

from infercrane import APIError, AsyncInferCrane, InferCrane, OperationFailed, OperationTimeout, StreamError


class Handler(BaseHTTPRequestHandler):
    operations = []
    requests = []

    def log_message(self, *_):
        pass

    def _json(self, status, body):
        encoded = json.dumps(body).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(encoded)))
        self.end_headers()
        self.wfile.write(encoded)

    def do_GET(self):
        if self.path == "/api/v1/operations/op-1":
            value = self.operations.pop(0) if len(self.operations) > 1 else self.operations[0]
            self._json(200, value)
        elif self.path == "/api/v1/operations/fail":
            self._json(200, {"id": "fail", "kind": "deployment.converge", "status": "failed", "progress": 55, "message": "capacity denied", "error_code": "provider_denied"})
        elif self.path.endswith("/slo-policy"):
            self._json(200, {"policy": {"max_ttft_p95_ms": 250}})
        elif "/recommendations?" in self.path:
            self._json(200, {"data": [{"status": "recommended"}]})
        elif self.path == "/api/v1/system/instances":
            self._json(200, {"data": [{"id": "node-a", "binary_version": "1.6.0", "protocol_min": 1, "protocol_max": 2}]})
        else:
            self._json(404, {"error": {"code": "not_found", "message": "missing", "retryable": False, "remediation": "check the name"}})

    def do_POST(self):
        length = int(self.headers.get("Content-Length", "0"))
        body = json.loads(self.rfile.read(length) or b"{}")
        self.requests.append((self.path, self.headers.get("Idempotency-Key"), body))
        if self.path == "/api/v1/deployments":
            self._json(202, {"operation": {"id": "op-1", "kind": "deployment.converge", "status": "pending", "progress": 0}})
        elif self.path.endswith("/recommendations"):
            self._json(201, {"recommendation": {"status": "recommended", "selected_evidence_id": "bench-1"}})
        elif self.path == "/v1/chat/completions":
            self.send_response(200)
            self.send_header("Content-Type", "text/event-stream")
            self.end_headers()
            self.wfile.write(b'data: {"choices":[{"delta":{"content":"hello"}}]}\n\ndata: [DONE]\n\n')
        else:
            self._json(202, {"status": "cancellation_requested"})

    def do_PUT(self):
        length = int(self.headers.get("Content-Length", "0"))
        body = json.loads(self.rfile.read(length) or b"{}")
        self.requests.append((self.path, None, body))
        self._json(200, {"policy": body})

    def do_DELETE(self):
        self.requests.append((self.path, None, None))
        self.send_response(204)
        self.end_headers()


class ClientTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        cls.thread = threading.Thread(target=cls.server.serve_forever, daemon=True)
        cls.thread.start()
        cls.url = f"http://127.0.0.1:{cls.server.server_port}"

    @classmethod
    def tearDownClass(cls):
        cls.server.shutdown()

    def setUp(self):
        Handler.requests = []
        Handler.operations = [{"id": "op-1", "kind": "deployment.converge", "status": "running", "progress": 40}, {"id": "op-1", "kind": "deployment.converge", "status": "succeeded", "progress": 100}]
        self.client = InferCrane(api_key="test-secret", base_url=self.url, timeout=1, poll_interval=0.001)

    def test_deploy_uses_explicit_idempotency_and_waits(self):
        operation = self.client.deploy(model="Qwen/Qwen3-8B", name="qwen", cloud="runpod", gpu="L40S", provider_adapter="skypilot", idempotency_key="stable-key")
        self.assertEqual(operation.id, "op-1")
        self.assertEqual(Handler.requests[0][1], "stable-key")
        self.assertEqual(Handler.requests[0][2]["provider_adapter"], "skypilot")
        self.assertEqual(self.client.wait(operation.id).status, "succeeded")

    def test_deploy_preserves_portable_runtime_workload(self):
        workload = {"image": "registry.example/runtime@sha256:" + "a" * 64, "command": ["serve", "--model", "${MODEL}"], "protocol": "openai", "port": 8000, "readiness_path": "/health", "models_path": "/v1/models", "metrics_path": "/metrics", "cancellation": "http-disconnect", "drain": "connection", "shutdown_grace_seconds": 30}
        self.client.deploy(model="org/model", cloud="runpod", gpu="L40S", runtime="custom-oci", runtime_version="1", runtime_args=["--safe"], workload=workload)
        body = Handler.requests[0][2]
        self.assertEqual(body["workload"], workload)
        self.assertEqual(body["runtime_args"], ["--safe"])
        self.assertEqual(body["runtime_version"], "1")

    def test_timeout_does_not_cancel_operation(self):
        Handler.operations = [{"id": "op-1", "kind": "deployment.converge", "status": "waiting", "progress": 55}]
        with self.assertRaises(OperationTimeout) as caught:
            self.client.wait("op-1", timeout=0.003)
        self.assertEqual(caught.exception.operation_id, "op-1")

    def test_control_plane_instances_exposes_protocol_evidence(self):
        instances = self.client.control_plane_instances()
        self.assertEqual(instances[0]["id"], "node-a")
        self.assertEqual(instances[0]["protocol_max"], 2)
        self.assertFalse(any(path.endswith("/cancel") for path, _, _ in Handler.requests))

    def test_terminal_failure_is_typed(self):
        with self.assertRaises(OperationFailed) as caught:
            self.client.wait("fail")
        self.assertIn("provider_denied", str(caught.exception))

    def test_api_error_is_typed(self):
        with self.assertRaises(APIError) as caught:
            self.client.get_deployment("missing")
        self.assertEqual(caught.exception.code, "not_found")
        self.assertEqual(caught.exception.remediation, "check the name")

    def test_stream_is_incremental_and_requires_done(self):
        chunks = list(self.client.stream_chat("qwen", [{"role": "user", "content": "hello"}]))
        self.assertEqual(chunks[0]["choices"][0]["delta"]["content"], "hello")

    def test_async_wait(self):
        async def run():
            client = AsyncInferCrane(api_key="test-secret", base_url=self.url, timeout=1, poll_interval=0.001)
            return await client.wait("op-1")
        self.assertEqual(asyncio.run(run()).status, "succeeded")

    def test_slo_and_recommendation_helpers_preserve_explicit_evidence_contract(self):
        policy = self.client.set_slo_policy("qwen prod", max_ttft_p95_ms=250, max_error_rate=0.01)
        self.assertEqual(policy["max_ttft_p95_ms"], 250)
        self.assertEqual(self.client.get_slo_policy("qwen prod")["max_ttft_p95_ms"], 250)
        recommendation = self.client.recommend("qwen prod")
        self.assertEqual(recommendation["selected_evidence_id"], "bench-1")
        self.assertEqual(self.client.recommendations("qwen prod", limit=1)[0]["status"], "recommended")
        self.client.delete_slo_policy("qwen prod")
        self.assertEqual(Handler.requests[0][0], "/api/v1/deployments/qwen%20prod/slo-policy")
        with self.assertRaises(ValueError):
            self.client.set_slo_policy("qwen", max_ttft_p95_ms=float("nan"))


if __name__ == "__main__":
    unittest.main()
