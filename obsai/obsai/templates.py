from __future__ import annotations

import hashlib
import re
from dataclasses import asdict, dataclass
from typing import Any, Iterable

from .redaction import redact_text


_ISO_TS = re.compile(r"\b\d{4}-\d{2}-\d{2}[T ]\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:?\d{2})?\b")
_UUID = re.compile(r"\b[0-9a-fA-F]{8}-[0-9a-fA-F-]{27,}\b")
_HEX = re.compile(r"\b[0-9a-fA-F]{16,}\b")
_DURATION = re.compile(r"\b\d+(?:\.\d+)?\s*(?:ns|us|µs|ms|s|m|h)\b", re.I)
_SIZE = re.compile(r"\b\d+(?:\.\d+)?\s*(?:b|kb|mb|gb|tb)\b", re.I)
_NUMBER = re.compile(r"(?<![A-Za-z_])[-+]?\d+(?:\.\d+)?(?![A-Za-z_])")
_QUOTED = re.compile(r'("[^"\n]{2,}"|\'[^\'\n]{2,}\')')
_SPACE = re.compile(r"\s+")


def normalize_message(message: str) -> str:
    """Normalize volatile values while preserving error words and identifiers."""

    value = redact_text(message)
    value = _ISO_TS.sub("<TS>", value)
    value = _UUID.sub("<ID>", value)
    value = _HEX.sub("<ID>", value)
    value = _DURATION.sub("<DUR>", value)
    value = _SIZE.sub("<SZ>", value)
    value = _QUOTED.sub("<STR>", value)
    value = _NUMBER.sub("<N>", value)
    return _SPACE.sub(" ", value).strip()


def _first(row: dict[str, Any], *keys: str) -> Any:
    for key in keys:
        if key in row and row[key] not in (None, ""):
            return row[key]
    return None


def _timestamp(value: Any) -> str | None:
    if value is None:
        return None
    return str(value)


@dataclass(frozen=True)
class Template:
    id: str
    service: str
    severity: str
    template: str
    count: int
    first_ts: str | None
    last_ts: str | None
    exemplar: str
    unknown: bool = False


@dataclass(frozen=True)
class AggregationResult:
    templates: list[Template]
    rows_seen: int
    rows_with_unknown_fields: int
    truncated: bool
    truncation_reasons: list[str]
    unknown_fields: list[str]

    @property
    def unknown(self) -> bool:
        return self.rows_seen == 0 or bool(self.unknown_fields)

    def as_dict(self) -> dict[str, Any]:
        return {
            "templates": [asdict(item) for item in self.templates],
            "rows_seen": self.rows_seen,
            "rows_with_unknown_fields": self.rows_with_unknown_fields,
            "truncated": self.truncated,
            "truncation_reasons": self.truncation_reasons,
            "unknown": self.unknown,
            "unknown_fields": self.unknown_fields,
        }

    def tsv(self) -> str:
        lines = ["service\tseverity\ttemplate\tcount\tfirst_ts\tlast_ts\texemplar"]
        for item in self.templates:
            lines.append(
                "\t".join(
                    [
                        item.service,
                        item.severity,
                        item.template,
                        str(item.count),
                        item.first_ts or "<unknown>",
                        item.last_ts or "<unknown>",
                        item.exemplar,
                    ]
                )
            )
        return "\n".join(lines)


class TemplateAggregator:
    def __init__(self, *, max_templates: int = 150, max_exemplar_chars: int = 500) -> None:
        self.max_templates = max_templates
        self.max_exemplar_chars = max_exemplar_chars

    def aggregate(self, rows: Iterable[dict[str, Any]], *, source_truncated: bool = False) -> AggregationResult:
        groups: dict[tuple[str, str, str], dict[str, Any]] = {}
        unknown_fields: set[str] = set()
        rows_seen = 0
        rows_unknown = 0
        for row in rows:
            rows_seen += 1
            service = _first(row, "service.name", "service_name", "service", "resource.service.name")
            severity = _first(row, "severity_text", "severityText", "severity")
            body = _first(row, "body", "message", "log")
            timestamp = _timestamp(_first(row, "p_timestamp", "timestamp", "time"))
            missing = []
            if service is None:
                service = "<unknown>"
                missing.append("service.name")
            if severity is None:
                severity = "UNKNOWN"
                missing.append("severity_text")
            if body is None:
                body = "<unknown>"
                missing.append("body")
            if timestamp is None:
                missing.append("p_timestamp")
            if missing:
                rows_unknown += 1
                unknown_fields.update(missing)
            service = str(service)
            severity = str(severity).upper()
            message = str(body)
            normalized = normalize_message(message)
            key = (service, severity, normalized)
            group = groups.setdefault(
                key,
                {"count": 0, "first_ts": timestamp, "last_ts": timestamp, "exemplar": redact_text(message)},
            )
            group["count"] += 1
            if timestamp is not None:
                group["first_ts"] = min(filter(None, [group["first_ts"], timestamp]))
                group["last_ts"] = max(filter(None, [group["last_ts"], timestamp]))

        def rank(item: tuple[tuple[str, str, str], dict[str, Any]]) -> tuple[int, int, str]:
            severity = item[0][1]
            priority = {"ERROR": 0, "FATAL": 0, "WARN": 1, "WARNING": 1, "UNKNOWN": 2, "INFO": 3, "DEBUG": 4}.get(severity, 2)
            return priority, -int(item[1]["count"]), item[0][0]

        ordered = sorted(groups.items(), key=rank)
        selected = ordered[: self.max_templates]
        truncated = source_truncated or len(ordered) > len(selected)
        reasons: list[str] = []
        if source_truncated:
            reasons.append("source_row_limit")
        if len(ordered) > len(selected):
            reasons.append("template_limit")
        templates = []
        for (service, severity, normalized), group in selected:
            digest = hashlib.sha256(f"{service}\0{severity}\0{normalized}".encode()).hexdigest()[:12]
            templates.append(
                Template(
                    id=f"tmpl:{service}#{digest}",
                    service=service,
                    severity=severity,
                    template=normalized,
                    count=int(group["count"]),
                    first_ts=group["first_ts"],
                    last_ts=group["last_ts"],
                    exemplar=str(group["exemplar"])[: self.max_exemplar_chars],
                    unknown=service == "<unknown>" or severity == "UNKNOWN",
                )
            )
        return AggregationResult(
            templates=templates,
            rows_seen=rows_seen,
            rows_with_unknown_fields=rows_unknown,
            truncated=truncated,
            truncation_reasons=reasons,
            unknown_fields=sorted(unknown_fields),
        )
