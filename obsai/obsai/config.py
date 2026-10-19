from __future__ import annotations

import os
from dataclasses import dataclass


def _positive_int(name: str, default: int, value: str | None = None) -> int:
    raw = value if value is not None else os.getenv(name)
    if raw is None or not raw.strip():
        return default
    try:
        parsed = int(raw)
    except ValueError as exc:
        raise ValueError(f"{name} must be an integer") from exc
    if parsed < 1:
        raise ValueError(f"{name} must be positive")
    return parsed


@dataclass(frozen=True)
class Settings:
    """Runtime settings with safe, bounded defaults.

    Parseable authentication is optional for local development. Production
    deployments should provide it through the service environment, never a
    committed config file.
    """

    bind_host: str = "127.0.0.1"
    bind_port: int = 8080
    parseable_url: str = "http://observability_parseable:8000"
    parseable_stream: str = "pmcluster-logs"
    parseable_auth_header: str | None = None
    max_rows: int = 50_000
    max_templates: int = 150
    max_exemplar_chars: int = 500
    default_window_hours: int = 2
    max_window_hours: int = 24
    request_timeout_seconds: float = 10.0

    @classmethod
    def from_env(cls) -> "Settings":
        return cls(
            bind_host=os.getenv("OBSAI_BIND_HOST", cls.bind_host),
            bind_port=_positive_int("OBSAI_BIND_PORT", cls.bind_port),
            parseable_url=os.getenv("OBSAI_PARSEABLE_URL", cls.parseable_url),
            parseable_stream=os.getenv("OBSAI_PARSEABLE_STREAM", cls.parseable_stream),
            parseable_auth_header=os.getenv("OBSAI_PARSEABLE_AUTH_HEADER") or None,
            max_rows=_positive_int("OBSAI_MAX_ROWS", cls.max_rows),
            max_templates=_positive_int("OBSAI_MAX_TEMPLATES", cls.max_templates),
            max_exemplar_chars=_positive_int("OBSAI_MAX_EXEMPLAR_CHARS", cls.max_exemplar_chars),
            default_window_hours=_positive_int("OBSAI_DEFAULT_WINDOW_HOURS", cls.default_window_hours),
            max_window_hours=_positive_int("OBSAI_MAX_WINDOW_HOURS", cls.max_window_hours),
            request_timeout_seconds=float(os.getenv("OBSAI_REQUEST_TIMEOUT_SECONDS", cls.request_timeout_seconds)),
        )
