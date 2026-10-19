from __future__ import annotations

import json
import re
from dataclasses import dataclass
from datetime import datetime, timedelta, timezone
from typing import Any, Callable
from urllib.error import HTTPError, URLError
from urllib.request import Request, urlopen


class ParseableError(RuntimeError):
    """A query could not be completed or did not satisfy the read-only contract."""


@dataclass(frozen=True)
class QueryResult:
    rows: list[dict[str, Any]]
    truncated: bool = False
    unknown: bool = False
    error: str | None = None


_LIMIT_RE = re.compile(r"(?is)\bLIMIT\s+(\d+)")
_WRITE_RE = re.compile(r"(?is)\b(insert|update|delete|drop|alter|create|truncate|replace|merge|grant|revoke)\b")


def _bounded_select(query: str, max_rows: int) -> str:
    statement = query.strip()
    if statement.endswith(";"):
        statement = statement[:-1].rstrip()
    if ";" in statement:
        raise ParseableError("only one SQL statement is allowed")
    if not re.match(r"(?is)^select\b", statement):
        raise ParseableError("only SELECT queries are allowed")
    if _WRITE_RE.search(statement):
        raise ParseableError("mutating SQL is not allowed")

    match = _LIMIT_RE.search(statement)
    if match:
        requested = int(match.group(1))
        bounded = min(requested, max_rows)
        return statement[: match.start(1)] + str(bounded) + statement[match.end(1) :]
    return f"{statement} LIMIT {max_rows}"


def _rows_from_payload(payload: Any) -> list[dict[str, Any]]:
    if isinstance(payload, list):
        raw_rows = payload
    elif isinstance(payload, dict):
        keys = ("data", "result", "records", "rows")
        present = next((key for key in keys if key in payload), None)
        if present is None:
            detail = payload.get("error", "missing data/result/records/rows")
            raise ParseableError(f"Parseable returned an unusable response: {detail}")
        raw_rows = payload[present]
        if isinstance(raw_rows, dict):
            nested_key = next((key for key in ("data", "rows") if key in raw_rows), None)
            if nested_key is None:
                detail = raw_rows.get("error", "missing nested data/rows")
                raise ParseableError(f"Parseable returned an unusable response: {detail}")
            raw_rows = raw_rows[nested_key]
    else:
        raise ParseableError("Parseable returned an unusable response shape")
    if not isinstance(raw_rows, list):
        raise ParseableError("Parseable returned a non-list row set")
    if any(not isinstance(row, dict) for row in raw_rows):
        raise ParseableError("Parseable returned a row that is not an object")
    return raw_rows


class ParseableClient:
    """Small read-only Parseable query client with an explicit row ceiling."""

    def __init__(
        self,
        base_url: str,
        stream: str = "pmcluster-logs",
        *,
        max_rows: int = 50_000,
        timeout: float = 10.0,
        auth_header: str | None = None,
        opener: Callable[..., Any] | None = None,
    ) -> None:
        self.base_url = base_url.rstrip("/")
        self.stream = stream
        self.max_rows = max_rows
        self.timeout = timeout
        self.auth_header = auth_header
        self._opener = opener or urlopen

    def logs_query(self, window_hours: int, limit: int | None = None) -> str:
        bounded = min(limit or self.max_rows, self.max_rows)
        if window_hours < 1:
            raise ParseableError("window_hours must be positive")
        return (
            "SELECT p_timestamp, severity_text, body, `service.name` "
            f"FROM `{self.stream}` "
            f"WHERE p_timestamp >= NOW() - INTERVAL '{window_hours} hours' "
            f"ORDER BY p_timestamp ASC LIMIT {bounded}"
        )

    def query_logs(self, window_hours: int) -> QueryResult:
        start_time, end_time = window_bounds(window_hours)
        # Parseable's query handler requires the explicit camelCase bounds even
        # when the SQL also contains a relative interval. Supplying both keeps
        # the request contract valid across Parseable versions and makes the
        # evidence window visible in captured requests.
        return self.query(self.logs_query(window_hours), start_time=start_time, end_time=end_time)

    def query(
        self,
        query: str,
        *,
        start_time: datetime | None = None,
        end_time: datetime | None = None,
    ) -> QueryResult:
        bounded_query = _bounded_select(query, self.max_rows)
        payload: dict[str, Any] = {"query": bounded_query}
        if start_time is not None:
            payload["startTime"] = _iso(start_time)
        if end_time is not None:
            payload["endTime"] = _iso(end_time)
        request = Request(
            f"{self.base_url}/api/v1/query",
            data=json.dumps(payload).encode("utf-8"),
            headers={
                "Content-Type": "application/json",
                **({"Authorization": self.auth_header} if self.auth_header else {}),
            },
            method="POST",
        )
        try:
            with self._opener(request, timeout=self.timeout) as response:
                body = response.read()
                status = getattr(response, "status", 200)
        except (HTTPError, URLError, TimeoutError, OSError) as exc:
            raise ParseableError(f"Parseable query failed: {exc}") from exc
        if status < 200 or status >= 300:
            raise ParseableError(f"Parseable query returned HTTP {status}")
        try:
            decoded = json.loads(body)
        except (TypeError, ValueError) as exc:
            raise ParseableError("Parseable returned invalid JSON") from exc
        rows = _rows_from_payload(decoded)
        has_more = bool(decoded.get("has_more")) if isinstance(decoded, dict) else False
        return QueryResult(rows=rows, truncated=has_more or len(rows) >= self.max_rows)


def _iso(value: datetime) -> str:
    if value.tzinfo is None:
        value = value.replace(tzinfo=timezone.utc)
    return value.astimezone(timezone.utc).isoformat().replace("+00:00", "Z")


def window_bounds(hours: int, now: datetime | None = None) -> tuple[datetime, datetime]:
    end = now or datetime.now(timezone.utc)
    if end.tzinfo is None:
        end = end.replace(tzinfo=timezone.utc)
    return end - timedelta(hours=hours), end
