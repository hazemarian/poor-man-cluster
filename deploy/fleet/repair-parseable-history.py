#!/usr/bin/env python3
"""Repair duplicate Parseable day manifests after a backend recreate.

Parseable 2.8.0 chooses the first manifest for a day, then appends a new
manifest when the process hostname changes.  Subsequent flushes can therefore
rewrite one hostname's manifest while the snapshot accumulates references to
it.  This tool is deliberately offline and conservative:

* it expects the Parseable container and its collector to be stopped by the
  caller;
* it makes complete tar backups of both mounted volumes before writing;
* it rebuilds every file in each affected day from Parquet metadata, including
  column statistics and sorting information;
* it replaces duplicate snapshot entries with one canonical entry and leaves
  old manifest files and parquet data in place for rollback.

The script runs in a temporary Python container with pyarrow installed.  It
never reads or prints telemetry rows, request bodies, or credentials.
"""

from __future__ import annotations

import argparse
import datetime as dt
import hashlib
import json
import math
import os
from pathlib import Path, PurePosixPath
import re
import shutil
import stat
import sys
import tarfile
import tempfile
from typing import Any


HOST_RE = re.compile(r"^[A-Za-z0-9][A-Za-z0-9_-]{0,62}$")


class RepairError(RuntimeError):
    """A safe, user-facing failure without data or secret output."""


def fail(message: str) -> "NoReturn":
    raise RepairError(message)


def load_json(path: Path, label: str) -> dict[str, Any]:
    try:
        with path.open("r", encoding="utf-8") as handle:
            value = json.load(handle)
    except (OSError, UnicodeError, json.JSONDecodeError):
        fail(label + " is unreadable")
    if not isinstance(value, dict):
        fail(label + " is not a JSON object")
    return value


def write_json_atomic(path: Path, value: dict[str, Any]) -> None:
    try:
        mode = stat.S_IMODE(path.stat().st_mode) if path.exists() else 0o644
        path.parent.mkdir(parents=True, exist_ok=True)
        with tempfile.NamedTemporaryFile(
            "w", encoding="utf-8", dir=path.parent, prefix=".repair-", delete=False
        ) as handle:
            temporary = Path(handle.name)
            json.dump(value, handle, ensure_ascii=False, separators=(",", ":"))
            handle.write("\n")
            handle.flush()
            os.fsync(handle.fileno())
        os.chmod(temporary, mode)
        os.replace(temporary, path)
        directory_fd = os.open(path.parent, os.O_RDONLY)
        try:
            os.fsync(directory_fd)
        finally:
            os.close(directory_fd)
    except OSError:
        try:
            temporary.unlink(missing_ok=True)
        except (NameError, OSError):
            pass
        fail("could not atomically write repaired metadata")


def object_path_to_fs(data_root: Path, object_path: str) -> Path:
    """Map Parseable's local-store `data/...` path back into the mounted root."""
    pure = PurePosixPath(object_path)
    if pure.is_absolute() or ".." in pure.parts:
        fail("manifest contains an unsafe object path")
    parts = pure.parts
    if not parts or parts[0] != "data":
        fail("manifest contains an unexpected object path")
    # The mounted root is `/data`; Parseable's object path starts with the
    # literal `data/` followed by the stream directory.
    relative = Path(*parts[1:]) if len(parts) > 1 else Path()
    return data_root / relative if len(parts) > 1 else data_root


def canonical_object_path(stream: str, day: str, hostname: str) -> str:
    return f"data/{stream}/date={day}/{hostname}.manifest.json"


def file_object_path(stream: str, data_root: Path, path: Path) -> str:
    try:
        relative = path.relative_to(data_root / stream)
    except ValueError:
        fail("parquet file is outside its stream volume")
    return PurePosixPath("data", stream, *relative.parts).as_posix()


def timestamp_number(value: Any) -> int | float | str | bool:
    """Convert pyarrow's timestamp presentation to Parseable's raw integer."""
    if isinstance(value, dt.datetime):
        if value.tzinfo is None:
            value = value.replace(tzinfo=dt.timezone.utc)
        else:
            value = value.astimezone(dt.timezone.utc)
        epoch = dt.datetime(1970, 1, 1, tzinfo=dt.timezone.utc)
        delta = value - epoch
        return delta.days * 86_400_000_000_000 + delta.seconds * 1_000_000_000 + delta.microseconds * 1_000
    if isinstance(value, dt.date):
        return value.toordinal()
    return value


def raw_stat(statistics: Any, bound: str) -> Any:
    raw = getattr(statistics, bound + "_raw", None)
    if raw is None:
        raw = getattr(statistics, bound, None)
    return timestamp_number(raw)


def typed_statistics(column: Any, statistics: Any) -> dict[str, dict[str, Any]] | None:
    if statistics is None or not getattr(statistics, "has_min_max", False):
        return None
    minimum = raw_stat(statistics, "min")
    maximum = raw_stat(statistics, "max")
    physical = str(getattr(column, "physical_type", ""))
    try:
        if physical == "BOOLEAN":
            return {"Bool": {"min": bool(minimum), "max": bool(maximum)}}
        if physical in {"INT32", "INT64", "INT96"}:
            if isinstance(minimum, (bytes, bytearray)):
                if physical == "INT96" and len(minimum) == 12:
                    nanos_of_day = int.from_bytes(minimum[:8], "little", signed=False)
                    julian_day = int.from_bytes(minimum[8:], "little", signed=False)
                    minimum = (julian_day - 2_440_588) * 86_400_000_000_000 + nanos_of_day
                else:
                    minimum = int.from_bytes(minimum, "little", signed=True)
            if isinstance(maximum, (bytes, bytearray)):
                if physical == "INT96" and len(maximum) == 12:
                    nanos_of_day = int.from_bytes(maximum[:8], "little", signed=False)
                    julian_day = int.from_bytes(maximum[8:], "little", signed=False)
                    maximum = (julian_day - 2_440_588) * 86_400_000_000_000 + nanos_of_day
                else:
                    maximum = int.from_bytes(maximum, "little", signed=True)
            return {"Int": {"min": int(minimum), "max": int(maximum)}}
        if physical in {"FLOAT", "DOUBLE"}:
            minimum = float(minimum)
            maximum = float(maximum)
            if not math.isfinite(minimum) or not math.isfinite(maximum):
                return None
            if minimum > maximum:
                return None
            return {"Float": {"min": minimum, "max": maximum}}
        if physical in {"BYTE_ARRAY", "FIXED_LEN_BYTE_ARRAY"}:
            if isinstance(minimum, (bytes, bytearray)):
                minimum = bytes(minimum).decode("utf-8")
            if isinstance(maximum, (bytes, bytearray)):
                maximum = bytes(maximum).decode("utf-8")
            if not isinstance(minimum, str) or not isinstance(maximum, str):
                return None
            return {"String": {"min": minimum, "max": maximum}}
    except (OverflowError, TypeError, ValueError, UnicodeError):
        return None
    return None


def statistic_status(column: Any, chunk: Any, row_count: int) -> tuple[str, dict[str, dict[str, Any]] | None]:
    """Classify one row-group column range without guessing missing bounds.

    A row group with no min/max is safe to ignore only when Parquet explicitly
    records every value as null.  This lets a valid range from a different row
    group remain useful while rejecting incomplete non-null bounds.
    """
    statistics = getattr(chunk, "statistics", None)
    typed = typed_statistics(column, statistics)
    if typed is not None:
        return "usable", typed
    if statistics is not None and getattr(statistics, "has_null_count", False):
        null_count = getattr(statistics, "null_count", None)
        try:
            if null_count is not None and int(null_count) == int(row_count):
                return "allnull", None
        except (TypeError, ValueError, OverflowError):
            pass
    return "unknown", None


def merge_typed_statistics(
    current: dict[str, dict[str, Any]] | None,
    new: dict[str, dict[str, Any]] | None,
) -> dict[str, dict[str, Any]] | None:
    # Parseable keeps a column's stats only while every usable row-group stat
    # agrees on the typed variant.  Explicitly all-null row groups are skipped
    # by the caller; unknown bounds poison the column before this is called.
    if current is None or new is None:
        return current
    current_kind, current_value = next(iter(current.items()))
    new_kind, new_value = next(iter(new.items()))
    if current_kind != new_kind:
        return None
    if current_kind == "Float":
        minimum = min(float(current_value["min"]), float(new_value["min"]))
        maximum = max(float(current_value["max"]), float(new_value["max"]))
        if not math.isfinite(minimum) or not math.isfinite(maximum) or minimum > maximum:
            return None
    else:
        minimum = min(current_value["min"], new_value["min"])
        maximum = max(current_value["max"], new_value["max"])
    return {current_kind: {"min": minimum, "max": maximum}}


def parquet_file_entry(data_root: Path, stream: str, path: Path) -> dict[str, Any]:
    try:
        import pyarrow.parquet as parquet
    except ImportError:
        fail("pyarrow is unavailable; run this tool in its temporary Python container")

    try:
        metadata = parquet.ParquetFile(path).metadata
        size = path.stat().st_size
    except (OSError, RuntimeError, ValueError):
        fail("a parquet file could not be read")

    columns: dict[str, dict[str, Any]] = {}
    column_stats_unknown: dict[str, bool] = {}
    row_group_orders: list[list[list[Any]]] = []
    for row_group_index in range(metadata.num_row_groups):
        row_group = metadata.row_group(row_group_index)
        for column_index in range(metadata.num_columns):
            column = metadata.schema.column(column_index)
            chunk = row_group.column(column_index)
            name = column.name
            status, stats = statistic_status(column, chunk, row_group.num_rows)
            if name not in columns:
                columns[name] = {
                    "name": name,
                    "stats": stats,
                    "uncompressed_size": int(chunk.total_uncompressed_size),
                    "compressed_size": int(chunk.total_compressed_size),
                }
                column_stats_unknown[name] = status == "unknown"
            else:
                entry = columns[name]
                entry["uncompressed_size"] += int(chunk.total_uncompressed_size)
                entry["compressed_size"] += int(chunk.total_compressed_size)
                if status == "unknown":
                    # A min/max range is useful only when every non-null row
                    # group has a usable range.  Do not retain partial bounds.
                    entry["stats"] = None
                    column_stats_unknown[name] = True
                elif status == "usable" and not column_stats_unknown[name]:
                    if entry["stats"] is None:
                        entry["stats"] = stats
                    else:
                        entry["stats"] = merge_typed_statistics(entry["stats"], stats)
                        if entry["stats"] is None:
                            column_stats_unknown[name] = True
                elif status == "allnull":
                    # Explicitly all-null row groups contribute no bounds and
                    # must not erase a valid range from another row group.
                    continue

        order: list[list[Any]] = []
        try:
            sorting_columns = row_group.sorting_columns
        except (AttributeError, RuntimeError, ValueError):
            sorting_columns = ()
        for sorting in sorting_columns:
            column_name = metadata.schema.column(sorting.column_index).name
            if sorting.descending and sorting.nulls_first:
                order_value = 3
            elif sorting.descending:
                order_value = 2
            elif sorting.nulls_first:
                order_value = 0
            else:
                order_value = 1
            order.append([column_name, order_value])
        row_group_orders.append(order)

    sort_order_id: list[list[Any]] = []
    if row_group_orders:
        candidate = row_group_orders[-1]
        if all(order == candidate for order in row_group_orders[:-1]):
            sort_order_id = candidate

    return {
        "file_path": file_object_path(stream, data_root, path),
        "num_rows": int(metadata.num_rows),
        "file_size": int(size),
        "ingestion_size": sum(
            int(metadata.row_group(index).total_byte_size)
            for index in range(metadata.num_row_groups)
        ),
        "columns": list(columns.values()),
        "sort_order_id": sort_order_id,
        # Private planning metadata.  It is removed before the manifest is
        # written, but lets the dry-run distinguish unknown bounds from an
        # explicitly all-null column.
        "_stats_unknown": sorted(name for name, unknown in column_stats_unknown.items() if unknown),
    }


def parquet_stats_profile(path: Path) -> dict[str, str]:
    """Return usable/all-null/unknown bounds coverage per column for one file."""
    try:
        import pyarrow.parquet as parquet
        metadata = parquet.ParquetFile(path).metadata
    except (ImportError, OSError, RuntimeError, ValueError):
        fail("a parquet file could not be read for stats audit")
    profile: dict[str, str] = {}
    for row_group_index in range(metadata.num_row_groups):
        row_group = metadata.row_group(row_group_index)
        for column_index in range(metadata.num_columns):
            column = metadata.schema.column(column_index)
            status, _ = statistic_status(column, row_group.column(column_index), row_group.num_rows)
            previous = profile.get(column.name)
            if previous == "unknown" or status == "unknown":
                profile[column.name] = "unknown"
            elif previous == "usable" or status == "usable":
                profile[column.name] = "usable"
            else:
                profile[column.name] = "allnull"
    return profile


def stream_paths(data_root: Path, selected: list[str] | None) -> list[str]:
    candidates = []
    for child in sorted(data_root.iterdir()):
        if not child.is_dir() or child.name.startswith("."):
            continue
        if (child / ".stream" / ".stream.json").is_file():
            candidates.append(child.name)
    if selected:
        unknown = sorted(set(selected) - set(candidates))
        if unknown:
            fail("requested stream is unavailable")
        return list(dict.fromkeys(selected))
    if not candidates:
        fail("no Parseable streams were found")
    return candidates


def day_entries(snapshot: dict[str, Any]) -> dict[str, list[dict[str, Any]]]:
    items = snapshot.get("snapshot", {}).get("manifest_list")
    if not isinstance(items, list):
        fail("stream snapshot has no manifest list")
    result: dict[str, list[dict[str, Any]]] = {}
    for item in items:
        if not isinstance(item, dict):
            fail("stream snapshot contains an invalid manifest entry")
        lower = item.get("time_lower_bound")
        if not isinstance(lower, str) or len(lower) < 10:
            fail("stream snapshot contains an invalid partition bound")
        result.setdefault(lower[:10], []).append(item)
    return result


def repair_days(snapshot: dict[str, Any], hostname: str) -> list[str]:
    return sorted(
        day
        for day, items in day_entries(snapshot).items()
        if len(items) > 1
        or len({item.get("manifest_path") for item in items}) > 1
        or any(
            not str(item.get("manifest_path", "")).endswith(
                f"/{hostname}.manifest.json"
            )
            for item in items
        )
    )


def parquet_paths(data_root: Path, stream: str, day: str) -> list[Path]:
    root = data_root / stream
    if day:
        root /= f"date={day}"
    if not root.is_dir():
        return []
    return sorted(path for path in root.rglob("*.parquet") if path.is_file())


def summarize(repairs: list[dict[str, Any]]) -> None:
    if not repairs:
        print("No duplicate Parseable day partitions require repair.")
        return
    print(f"Repair plan: {len(repairs)} stream/day partition(s)")
    for repair in repairs:
        print(
            "  stream={stream} day={day} old_entries={entries} parquet_files={files} "
            "rows={rows} bytes={bytes} stats_missing={stats_missing} "
            "stats_audit={stats_audit}".format(**repair)
        )


def build_repairs(
    data_root: Path, streams: list[str], hostname: str
) -> tuple[list[dict[str, Any]], dict[str, dict[str, Any]]]:
    repairs: list[dict[str, Any]] = []
    snapshots: dict[str, dict[str, Any]] = {}
    for stream in streams:
        snapshot_path = data_root / stream / ".stream" / ".stream.json"
        snapshot = load_json(snapshot_path, f"{stream} stream metadata")
        snapshots[stream] = snapshot
        # Per-column file counts: usable bounds, explicitly all-null row
        # groups, and unknown coverage.  Unknown includes non-null row groups
        # without min/max and is never silently treated as all-null.
        audit: dict[str, list[int]] = {}
        stream_files = parquet_paths(data_root, stream, "")
        for path in stream_files:
            for name, status in parquet_stats_profile(path).items():
                counts = audit.setdefault(name, [0, 0])
                if len(counts) < 3:
                    counts.append(0)
                counts[{"usable": 0, "allnull": 1, "unknown": 2}[status]] += 1
        unsafe = [
            f"{name}:usable={counts[0]},allnull={counts[1]},unknown={counts[2]}"
            for name, counts in sorted(audit.items())
            if counts[2]
        ]
        all_null_only = [
            f"{name}:usable={counts[0]},allnull={counts[1]}"
            for name, counts in sorted(audit.items())
            if counts[1] and not counts[2]
        ]
        if unsafe:
            stats_audit = "mixed(" + ";".join(unsafe) + ")"
        elif all_null_only:
            stats_audit = "complete(allnull(" + ";".join(all_null_only) + "))"
        else:
            stats_audit = "complete"
        for day in repair_days(snapshot, hostname):
            entries = day_entries(snapshot)[day]
            files = parquet_paths(data_root, stream, day)
            if not files:
                fail("a duplicate partition has no parquet files")
            rebuilt = [parquet_file_entry(data_root, stream, path) for path in files]
            for entry, source in zip(rebuilt, files):
                mapped = object_path_to_fs(data_root, entry["file_path"])
                if mapped != source or not mapped.is_file():
                    fail("rebuilt parquet path did not round-trip to its source")
            missing_stats: dict[str, int] = {}
            for entry in rebuilt:
                for name in entry.get("_stats_unknown", []):
                    missing_stats[name] = missing_stats.get(name, 0) + 1
            manifest_files = [
                {key: value for key, value in entry.items() if not key.startswith("_")}
                for entry in rebuilt
            ]
            repairs.append(
                {
                    "stream": stream,
                    "day": day,
                    "entries": len(entries),
                    "files": len(rebuilt),
                    "rows": sum(entry["num_rows"] for entry in rebuilt),
                    "bytes": sum(entry["file_size"] for entry in rebuilt),
                    "stats_missing": (
                        "none"
                        if not missing_stats
                        else ",".join(
                            f"{name}:{count}" for name, count in sorted(missing_stats.items())
                        )
                    ),
                    "missing_stats": missing_stats,
                    "stats_audit": stats_audit,
                    "stats_safe": not unsafe,
                    "manifest": {"version": "v1", "files": manifest_files},
                    "snapshot_path": snapshot_path,
                    "canonical_path": canonical_object_path(stream, day, hostname),
                }
            )
    return repairs, snapshots


def apply_snapshots(
    data_root: Path,
    hostname: str,
    repairs: list[dict[str, Any]],
    snapshots: dict[str, dict[str, Any]],
) -> None:
    by_stream: dict[str, list[dict[str, Any]]] = {}
    for repair in repairs:
        by_stream.setdefault(repair["stream"], []).append(repair)

    for repair in repairs:
        manifest_path = object_path_to_fs(data_root, repair["canonical_path"])
        write_json_atomic(manifest_path, repair["manifest"])

    for stream, stream_repairs in by_stream.items():
        snapshot = snapshots[stream]
        items = snapshot["snapshot"]["manifest_list"]
        for repair in stream_repairs:
            day = repair["day"]
            matching = [item for item in items if item["time_lower_bound"][:10] == day]
            if not matching:
                fail("snapshot changed while repairing metadata")
            first = matching[0]
            canonical = {
                **first,
                "manifest_path": repair["canonical_path"],
                "events_ingested": repair["rows"],
                "ingestion_size": sum(
                    entry["ingestion_size"] for entry in repair["manifest"]["files"]
                ),
                "storage_size": repair["bytes"],
            }
            first_index = items.index(first)
            items[:] = [
                item
                for item in items
                if item["time_lower_bound"][:10] != day
            ]
            items.insert(first_index, canonical)
        write_json_atomic(stream_repairs[0]["snapshot_path"], snapshot)


def validate_repaired_metadata(
    data_root: Path,
    repairs: list[dict[str, Any]],
) -> None:
    """Verify each repaired day is queryable as one complete manifest."""
    for repair in repairs:
        manifest_path = object_path_to_fs(data_root, repair["canonical_path"])
        manifest = load_json(manifest_path, "repaired manifest")
        files = manifest.get("files")
        if not isinstance(files, list) or len(files) != repair["files"]:
            fail("repaired manifest file count did not validate")
        paths = [entry.get("file_path") for entry in files if isinstance(entry, dict)]
        if len(paths) != len(set(paths)):
            fail("repaired manifest contains duplicate parquet paths")
        for object_path in paths:
            if not isinstance(object_path, str) or not object_path_to_fs(data_root, object_path).is_file():
                fail("repaired manifest references a missing parquet file")

        snapshot = load_json(repair["snapshot_path"], "repaired stream metadata")
        items = snapshot.get("snapshot", {}).get("manifest_list", [])
        matches = [item for item in items if item.get("time_lower_bound", "")[:10] == repair["day"]]
        if len(matches) != 1 or matches[0].get("manifest_path") != repair["canonical_path"]:
            fail("repaired snapshot does not contain one canonical day entry")


def sha256(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as handle:
        for chunk in iter(lambda: handle.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def create_backup(data_root: Path, staging_root: Path, backup_root: Path) -> Path:
    stamp = dt.datetime.now(dt.timezone.utc).strftime("%Y%m%dT%H%M%SZ")
    backup = backup_root / f"parseable-history-{stamp}"
    try:
        backup_root.mkdir(parents=True, exist_ok=True, mode=0o700)
        os.chmod(backup_root, 0o700)
        required = volume_bytes(data_root) + volume_bytes(staging_root)
        if shutil.disk_usage(backup_root).free < required + 64 * 1024 * 1024:
            fail("backup filesystem does not have enough free space")
        backup.mkdir(parents=True, exist_ok=False, mode=0o700)
        os.chmod(backup, 0o700)
    except OSError:
        fail("backup directory could not be created")
    archive_info = []
    try:
        for label, source in (("data", data_root), ("staging", staging_root)):
            archive = backup / f"{label}.tar.gz"
            with tarfile.open(archive, "w:gz") as output:
                output.add(source, arcname=label, recursive=True)
            os.chmod(archive, 0o600)
            archive_info.append(
                {"name": archive.name, "bytes": archive.stat().st_size, "sha256": sha256(archive)}
            )
        backup_json = backup / "backup.json"
        with backup_json.open("w", encoding="utf-8") as handle:
            json.dump(
                {
                    "created_at": dt.datetime.now(dt.timezone.utc).isoformat(),
                    "archives": archive_info,
                },
                handle,
                indent=2,
            )
            handle.write("\n")
        os.chmod(backup_json, 0o600)
    except (OSError, tarfile.TarError):
        fail("complete Parseable data/staging backup failed")
    print(f"Backup complete: {backup.name} ({len(archive_info)} volume archives)")
    return backup


def volume_bytes(root: Path) -> int:
    total = 0
    try:
        for path in root.rglob("*"):
            if path.is_file():
                total += path.stat().st_size
    except OSError:
        fail("Parseable volume size could not be measured")
    return total


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--data", type=Path, default=Path("/data"))
    parser.add_argument("--staging", type=Path, default=Path("/staging"))
    parser.add_argument("--backup", type=Path, default=Path("/backup"))
    parser.add_argument("--hostname", required=True, help="stable Parseable container hostname")
    parser.add_argument("--stream", action="append", dest="streams")
    parser.add_argument("--apply", action="store_true", help="write after making a full backup")
    parser.add_argument(
        "--offline",
        action="store_true",
        help="confirm that Parseable and its collector are stopped",
    )
    args = parser.parse_args()
    if not HOST_RE.fullmatch(args.hostname):
        fail("stable hostname contains unsupported characters")
    for root, label in ((args.data, "data"), (args.staging, "staging")):
        if not root.is_dir():
            fail(label + " volume mount is unavailable")
    streams = stream_paths(args.data, args.streams)
    repairs, snapshots = build_repairs(args.data, streams, args.hostname)
    summarize(repairs)
    if not args.apply:
        print("Dry run only; no metadata or volume files were changed.")
        return 0
    if not args.offline:
        fail("--apply requires --offline after stopping Parseable and its collector")
    if not repairs:
        print("Nothing to apply.")
        return 0
    if any(not repair["stats_safe"] or repair["missing_stats"] for repair in repairs):
        fail("parquet metadata has unknown column-statistics coverage; no files were changed")
    backup = create_backup(args.data, args.staging, args.backup)
    apply_snapshots(args.data, args.hostname, repairs, snapshots)
    validate_repaired_metadata(args.data, repairs)
    print(f"Applied {len(repairs)} repaired partition(s); rollback backup is {backup.name}")
    return 0


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except RepairError as error:
        print("repair-parseable-history: " + str(error), file=sys.stderr)
        raise SystemExit(2)
