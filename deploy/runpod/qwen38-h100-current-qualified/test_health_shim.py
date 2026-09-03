from __future__ import annotations

import http.client
import http.server
import importlib.util
import pathlib
import socket
import threading
import unittest


PACKAGE_DIR = pathlib.Path(__file__).resolve().parent
SPEC = importlib.util.spec_from_file_location("health_shim", PACKAGE_DIR / "health_shim.py")
assert SPEC is not None and SPEC.loader is not None
health_shim = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(health_shim)


def free_port() -> int:
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        return int(sock.getsockname()[1])


class UpstreamHandler(http.server.BaseHTTPRequestHandler):
    status = http.HTTPStatus.OK

    def do_GET(self) -> None:  # noqa: N802
        self.send_response(self.status)
        self.send_header("Content-Length", "0")
        self.end_headers()

    def log_message(self, format: str, *args: object) -> None:
        return


class HealthShimTest(unittest.TestCase):
    def setUp(self) -> None:
        UpstreamHandler.status = http.HTTPStatus.OK
        self.upstream_port = free_port()
        self.upstream = http.server.ThreadingHTTPServer(
            ("127.0.0.1", self.upstream_port), UpstreamHandler
        )
        self.upstream_thread = threading.Thread(target=self.upstream.serve_forever)
        self.upstream_thread.start()

        self.shim_port = free_port()
        handler = health_shim.make_handler(
            f"http://127.0.0.1:{self.upstream_port}/health", 0.5
        )
        self.shim = http.server.ThreadingHTTPServer(("127.0.0.1", self.shim_port), handler)
        self.shim_thread = threading.Thread(target=self.shim.serve_forever)
        self.shim_thread.start()

    def tearDown(self) -> None:
        self.shim.shutdown()
        self.shim.server_close()
        self.shim_thread.join()
        if self.upstream is not None:
            self.upstream.shutdown()
            self.upstream.server_close()
            self.upstream_thread.join()

    def request(self, path: str) -> int:
        connection = http.client.HTTPConnection("127.0.0.1", self.shim_port, timeout=2)
        connection.request("GET", path)
        response = connection.getresponse()
        response.read()
        connection.close()
        return response.status

    def test_ready_only_when_sglang_is_ready(self) -> None:
        UpstreamHandler.status = http.HTTPStatus.OK
        self.assertEqual(self.request("/ping"), http.HTTPStatus.OK)

        UpstreamHandler.status = http.HTTPStatus.SERVICE_UNAVAILABLE
        self.assertEqual(self.request("/ping"), http.HTTPStatus.NO_CONTENT)

    def test_unavailable_upstream_is_initializing(self) -> None:
        self.upstream.shutdown()
        self.upstream.server_close()
        self.upstream_thread.join()
        self.upstream = None

        self.assertEqual(self.request("/ping"), http.HTTPStatus.NO_CONTENT)

    def test_unknown_path_is_not_healthy(self) -> None:
        self.assertEqual(self.request("/health"), http.HTTPStatus.NOT_FOUND)


if __name__ == "__main__":
    unittest.main()
