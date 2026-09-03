#!/usr/bin/env python3
"""Translate SGLang readiness into RunPod Load Balancer health semantics."""

from __future__ import annotations

import http.server
import urllib.error
import urllib.request

LISTEN_HOST = "0.0.0.0"
LISTEN_PORT = 30001
UPSTREAM_HEALTH_URL = "http://127.0.0.1:30000/health"
UPSTREAM_TIMEOUT_SECONDS = 3.0


def make_handler(
    upstream_url: str = UPSTREAM_HEALTH_URL,
    timeout_seconds: float = UPSTREAM_TIMEOUT_SECONDS,
) -> type[http.server.BaseHTTPRequestHandler]:
    class HealthHandler(http.server.BaseHTTPRequestHandler):
        def do_GET(self) -> None:  # noqa: N802 - BaseHTTPRequestHandler API
            self._serve(include_body=True)

        def do_HEAD(self) -> None:  # noqa: N802 - BaseHTTPRequestHandler API
            self._serve(include_body=False)

        def _serve(self, *, include_body: bool) -> None:
            if self.path != "/ping":
                self._empty_response(http.HTTPStatus.NOT_FOUND)
                return

            try:
                with urllib.request.urlopen(upstream_url, timeout=timeout_seconds) as response:
                    ready = response.status == http.HTTPStatus.OK
            except urllib.error.HTTPError as error:
                error.close()
                ready = False
            except (OSError, urllib.error.URLError):
                ready = False

            # RunPod Load Balancer treats 200 as ready and 204 as initializing.
            # SGLang returns 503 until model/JIT/graph warmup has completed.
            status = http.HTTPStatus.OK if ready else http.HTTPStatus.NO_CONTENT
            self._empty_response(status, include_body=include_body)

        def _empty_response(
            self,
            status: http.HTTPStatus,
            *,
            include_body: bool = False,
        ) -> None:
            self.send_response(status)
            self.send_header("Cache-Control", "no-store")
            self.send_header("Content-Length", "0")
            self.end_headers()
            if include_body:
                self.wfile.write(b"")

        def log_message(self, format: str, *args: object) -> None:
            # Health probes are intentionally quiet and never log request data.
            return

    return HealthHandler


def main() -> None:
    server = http.server.ThreadingHTTPServer(
        (LISTEN_HOST, LISTEN_PORT),
        make_handler(),
    )
    server.serve_forever()


if __name__ == "__main__":
    main()
