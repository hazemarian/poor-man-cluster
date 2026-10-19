from __future__ import annotations

from datetime import datetime, timezone
from typing import Any

from .config import Settings
from .parseable import ParseableClient, ParseableError
from .templates import TemplateAggregator


def build_brief(client: ParseableClient, settings: Settings, *, window_hours: int | None = None) -> dict[str, Any]:
    requested_window = window_hours or settings.default_window_hours
    bounded_window = max(1, min(requested_window, settings.max_window_hours))
    try:
        result = client.query_logs(bounded_window)
    except ParseableError as exc:
        return {
            "status": "unknown",
            "generated_at": datetime.now(timezone.utc).isoformat(),
            "window_hours": bounded_window,
            "source": {
                "stream": client.stream,
                "rows_seen": 0,
                "truncated": False,
                "unknown": True,
                "error": str(exc),
            },
            "evidence": {
                "templates": [],
                "tsv": "service\tseverity\ttemplate\tcount\tfirst_ts\tlast_ts\texemplar",
                "unknown": True,
                "unknown_fields": ["source"],
                "truncated": False,
                "truncation_reasons": [],
            },
        }

    aggregation = TemplateAggregator(
        max_templates=settings.max_templates,
        max_exemplar_chars=settings.max_exemplar_chars,
    ).aggregate(result.rows, source_truncated=result.truncated)
    return {
        "status": "ok" if result.rows else "unknown",
        "generated_at": datetime.now(timezone.utc).isoformat(),
        "window_hours": bounded_window,
        "source": {
            "stream": client.stream,
            "rows_seen": len(result.rows),
            "truncated": result.truncated,
            "unknown": not bool(result.rows),
        },
        "evidence": {
            **aggregation.as_dict(),
            "tsv": aggregation.tsv(),
        },
    }
