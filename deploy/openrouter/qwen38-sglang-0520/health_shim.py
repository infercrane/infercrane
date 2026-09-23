#!/usr/bin/env python3
"""Translate provider readiness into RunPod load-balancer semantics."""

from __future__ import annotations

import http.server
import urllib.error
import urllib.request


class HealthHandler(http.server.BaseHTTPRequestHandler):
    def do_GET(self) -> None:  # noqa: N802
        if self.path != "/ping":
            self.send_response(http.HTTPStatus.NOT_FOUND)
            self.end_headers()
            return
        try:
            with urllib.request.urlopen(
                "http://127.0.0.1:8080/readyz", timeout=3
            ) as response:
                ready = response.status == http.HTTPStatus.OK
        except (OSError, urllib.error.URLError):
            ready = False
        self.send_response(
            http.HTTPStatus.OK if ready else http.HTTPStatus.NO_CONTENT
        )
        self.send_header("Cache-Control", "no-store")
        self.send_header("Content-Length", "0")
        self.end_headers()

    def log_message(self, format: str, *args: object) -> None:
        return


http.server.ThreadingHTTPServer(("0.0.0.0", 30001), HealthHandler).serve_forever()
