from __future__ import annotations

import json
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import parse_qs, urlparse

from . import __version__
from .config import Settings
from .evidence import build_brief
from .parseable import ParseableClient


def create_server(settings: Settings, client: ParseableClient | None = None) -> ThreadingHTTPServer:
    query_client = client or ParseableClient(
        settings.parseable_url,
        settings.parseable_stream,
        max_rows=settings.max_rows,
        timeout=settings.request_timeout_seconds,
        auth_header=settings.parseable_auth_header,
    )

    class Handler(BaseHTTPRequestHandler):
        def _json(self, status: int, payload: dict[str, object]) -> None:
            encoded = json.dumps(payload, sort_keys=True).encode("utf-8")
            self.send_response(status)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(encoded)))
            self.end_headers()
            self.wfile.write(encoded)

        def do_GET(self) -> None:  # noqa: N802 - stdlib handler API
            parsed = urlparse(self.path)
            if parsed.path in ("/healthz", "/api/health"):
                self._json(200, {"status": "ok", "service": "obsai", "version": __version__})
                return
            if parsed.path == "/api/brief":
                raw_window = parse_qs(parsed.query).get("window", [str(settings.default_window_hours)])[0]
                try:
                    hours = int(raw_window.removesuffix("h"))
                except ValueError:
                    self._json(400, {"error": "window must be an integer number of hours"})
                    return
                self._json(200, build_brief(query_client, settings, window_hours=hours))
                return
            self._json(404, {"error": "not found"})

        def log_message(self, _format: str, *_args: object) -> None:
            return

    return ThreadingHTTPServer((settings.bind_host, settings.bind_port), Handler)
