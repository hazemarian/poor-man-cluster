from __future__ import annotations

import re
from collections.abc import Iterable


_JWT = re.compile(r"\beyJ[A-Za-z0-9_-]{12,}\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\b")
_AWS_KEY = re.compile(r"\bAKIA[0-9A-Z]{16}\b")
_BEARER = re.compile(r"(?i)\bBearer\s+[A-Za-z0-9._~+/=-]{8,}")
_BASIC = re.compile(r"(?i)\bBasic\s+[A-Za-z0-9+/=]{8,}")
_AUTH_HEADER = re.compile(r"(?i)(\bAuthorization\s*[:=]\s*)(Bearer|Basic)\s+[^\s,;]+")
_EMAIL = re.compile(r"\b[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}\b")
_PHONE = re.compile(r"(?<!\d)(?:\+?[0-9][0-9 .()_-]{7,}[0-9])(?!\d)")
_IP = re.compile(r"(?<![\w.])(?:\d{1,3}\.){3}\d{1,3}(?::\d{1,5})?(?![\w.])")
_URL_CREDENTIALS = re.compile(r"(?i)(://[^/:\s]+):([^@\s]+)@")
_SECRET_ASSIGNMENT = re.compile(
    r'''(?i)(["']?\b(?:password|passwd|secret|token|api[_-]?key)\b["']?\s*[=:]\s*)("(?:\\.|[^"\\])*"|'(?:\\.|[^'\\])*'|[^\s,;}]+)'''
)


def redact_text(value: str, exact_values: Iterable[str] = ()) -> str:
    """Mask common credentials and direct identifiers in evidence text.

    The function is intentionally deterministic and dependency-free. It keeps
    field names and protocol markers so an operator can still recognize the
    failure class without exposing the value.
    """

    result = value
    for secret in sorted((s for s in exact_values if s), key=len, reverse=True):
        result = result.replace(secret, "<REDACTED>")
    result = _URL_CREDENTIALS.sub(r"\1:<REDACTED>@", result)
    result = _JWT.sub("<JWT>", result)
    result = _AWS_KEY.sub("<AWS_KEY>", result)
    result = _AUTH_HEADER.sub(r"\1\2 <REDACTED>", result)
    result = _BEARER.sub("Bearer <REDACTED>", result)
    result = _BASIC.sub("Basic <REDACTED>", result)
    result = _SECRET_ASSIGNMENT.sub(_mask_assignment, result)
    result = _EMAIL.sub("<EMAIL>", result)
    result = _IP.sub("<ADDR>", result)
    result = _PHONE.sub("<PHONE>", result)
    return result


def _mask_assignment(match: re.Match[str]) -> str:
    value = match.group(2)
    if len(value) >= 2 and value[0] == value[-1] and value[0] in "\"'":
        return f"{match.group(1)}{value[0]}<REDACTED>{value[0]}"
    return f"{match.group(1)}<REDACTED>"


def redact_row(row: dict[str, object], exact_values: Iterable[str] = ()) -> dict[str, object]:
    """Return a shallow row copy with string values redacted."""

    return {
        key: redact_text(value, exact_values) if isinstance(value, str) else value
        for key, value in row.items()
    }
