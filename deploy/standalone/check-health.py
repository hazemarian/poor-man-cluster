#!/usr/bin/env python3
"""Read-only health check for the standalone monitoring deployment.

Run this from the deployment directory (normally ``~/nextrum-monitoring``).
The private Compose deployment is checked through container IPs.  Deshli's
public variant keeps Parseable in Compose but runs the daemon and authenticated
edge as user systemd services, so their live process environment is used for
the local endpoint and credentials.

The checker deliberately reads response bodies only in memory.  It prints
statuses and bounded counts, never credentials, response bodies, SQL rows, or
application log text.
"""

import argparse
import base64
import datetime as dt
import json
import math
import os
from pathlib import Path
import subprocess
import sys
import time
import urllib.error
import urllib.parse
import urllib.request


ROOT = Path(__file__).resolve().parent
STREAMS = ("pmcluster-logs", "pmcluster-metrics", "pmcluster-traces")
METRIC_NAMES = (
    "system.cpu.utilization",
    "system.memory.utilization",
    "container.cpu.utilization",
    "container.memory.percent",
)
MAX_RESPONSE_BYTES = 2 * 1024 * 1024
REQUEST_TIMEOUT = 20


class CheckFailure(RuntimeError):
    """A safe, user-facing check failure without secret or body details."""


def fail(message):
    raise CheckFailure(message)


def run_command(args, label, timeout=20, allow_empty=False, env=None):
    try:
        result = subprocess.run(
            args,
            cwd=str(ROOT),
            env=env,
            capture_output=True,
            text=True,
            timeout=timeout,
            check=False,
        )
    except FileNotFoundError:
        fail(label + ": required command is unavailable")
    except subprocess.TimeoutExpired:
        fail(label + ": command timed out")
    except OSError:
        fail(label + ": command could not be started")
    if result.returncode != 0:
        fail(label + ": command failed")
    output = result.stdout.strip()
    if not output and not allow_empty:
        fail(label + ": command returned no result")
    return output


def parse_env_file(path):
    values = {}
    if not path.is_file():
        return values
    try:
        lines = path.read_text(encoding="utf-8").splitlines()
    except (OSError, UnicodeError):
        fail("deployment credentials file could not be read")
    for raw in lines:
        line = raw.strip()
        if not line or line.startswith("#"):
            continue
        if line.startswith("export "):
            line = line[7:].lstrip()
        if "=" not in line:
            continue
        key, value = line.split("=", 1)
        key = key.strip()
        if not key:
            continue
        value = value.strip()
        if len(value) >= 2 and value[0] == value[-1] and value[0] in "'\"":
            value = value[1:-1]
        values[key] = value
    return values


def compose_container_ip(service, required=True):
    """Return the first running container IP for a Compose service."""
    output = run_command(
        ["docker", "compose", "ps", "-q", service],
        "docker compose service discovery",
        allow_empty=True,
    )
    container_id = next((line.strip() for line in output.splitlines() if line.strip()), "")
    if not container_id:
        if required:
            fail("Compose service " + service + " is not running")
        return None
    inspect = run_command(
        ["docker", "inspect", container_id],
        "Docker container inspection",
    )
    try:
        records = json.loads(inspect)
        record = records[0]
        if not record.get("State", {}).get("Running", False):
            fail("Compose service " + service + " is not running")
        networks = record.get("NetworkSettings", {}).get("Networks", {})
        addresses = [
            details.get("IPAddress", "").strip()
            for details in networks.values()
            if isinstance(details, dict)
        ]
        address = next((item for item in addresses if item), "")
    except (ValueError, TypeError, IndexError, AttributeError):
        fail("Docker container inspection returned no usable address")
    if not address:
        fail("Compose service " + service + " has no container address")
    return address


def process_environment(unit):
    """Read a user systemd unit's live environment without exposing values."""
    uid = os.getuid()
    bus = dict(os.environ)
    bus["XDG_RUNTIME_DIR"] = "/run/user/" + str(uid)
    bus["DBUS_SESSION_BUS_ADDRESS"] = "unix:path=/run/user/" + str(uid) + "/bus"
    try:
        pid = run_command(
            ["systemctl", "--user", "show", unit, "--property=MainPID", "--value"],
            "public edge service discovery",
            env=bus,
        )
        if not pid.isdigit() or int(pid) <= 1:
            return {}
        raw = Path("/proc/" + pid + "/environ").read_bytes()
    except (CheckFailure, OSError):
        return {}
    values = {}
    for item in raw.decode("utf-8", errors="ignore").split("\0"):
        if "=" in item:
            key, value = item.split("=", 1)
            values[key] = value
    return values


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, *args, **kwargs):
        return None


NO_REDIRECT_OPENER = urllib.request.build_opener(NoRedirect())


def request(base, path, method="GET", auth=None, payload=None, opener=None):
    headers = {"Accept": "application/json, text/html", "User-Agent": "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 Chrome/124 Safari/537.36"}
    if auth:
        headers["Authorization"] = auth
    data = None
    if payload is not None:
        data = json.dumps(payload, separators=(",", ":")).encode("utf-8")
        headers["Content-Type"] = "application/json"
    url = base.rstrip("/") + path
    req = urllib.request.Request(url, data=data, headers=headers, method=method)
    client = opener or urllib.request.build_opener()
    try:
        with client.open(req, timeout=REQUEST_TIMEOUT) as response:
            return response.status, dict(response.headers), response.read(MAX_RESPONSE_BYTES)
    except urllib.error.HTTPError as error:
        # Do not retain or surface error bodies. The status and selected headers
        # are sufficient for every negative assertion in this checker.
        try:
            error.read(MAX_RESPONSE_BYTES)
        except OSError:
            pass
        return error.code, dict(error.headers), b""
    except (urllib.error.URLError, TimeoutError, OSError):
        fail("HTTP request failed for " + path)


def json_response(base, path, label, auth=None, method="GET", payload=None):
    status, _, body = request(base, path, method=method, auth=auth, payload=payload)
    if status < 200 or status >= 300:
        fail(label + ": HTTP " + str(status))
    try:
        return json.loads(body.decode("utf-8"))
    except (UnicodeError, ValueError, TypeError):
        fail(label + ": invalid JSON response")


def basic_auth(values, process_values):
    auth = process_values.get("OBSERVABILITY_PARSEABLE_AUTH") or values.get("PARSEABLE_AUTH")
    if auth:
        return auth
    password = values.get("PARSEABLE_PASSWORD")
    if not password:
        fail("Parseable credentials are unavailable locally")
    encoded = base64.b64encode(("monitoring:" + password).encode("utf-8")).decode("ascii")
    return "Basic " + encoded


def normalize_duration(value):
    if isinstance(value, str):
        return value.strip().lower()
    return ""


def retention_durations(value):
    found = []
    if isinstance(value, dict):
        for key, item in value.items():
            if key == "duration":
                duration = normalize_duration(item)
                if duration:
                    found.append(duration)
            else:
                found.extend(retention_durations(item))
    elif isinstance(value, list):
        for item in value:
            found.extend(retention_durations(item))
    return found


def rows_from_query(value):
    if isinstance(value, list):
        return value
    if isinstance(value, dict):
        for key in ("data", "rows", "result"):
            rows = value.get(key)
            if isinstance(rows, list):
                return rows
    return []


def timestamp(now):
    return now.astimezone(dt.timezone.utc).strftime("%Y-%m-%dT%H:%M:%S.%fZ")


def query_metrics(backend, auth, now):
    start = now - dt.timedelta(minutes=10)
    sql = (
        "SELECT metric_name, data_point_value FROM `pmcluster-metrics` WHERE "
        "p_timestamp >= TIMESTAMP '" + timestamp(start) + "' AND "
        "p_timestamp <= TIMESTAMP '" + timestamp(now) + "' AND metric_name IN ("
        + ",".join("'" + name + "'" for name in METRIC_NAMES)
        + ") LIMIT 2000"
    )
    payload = {
        "query": sql,
        "startTime": timestamp(start),
        "endTime": timestamp(now),
    }
    value = json_response(
        backend,
        "/api/v1/query",
        "fresh metric query",
        auth=auth,
        method="POST",
        payload=payload,
    )
    counts = {name: 0 for name in METRIC_NAMES}
    for row in rows_from_query(value):
        if not isinstance(row, dict):
            continue
        name = row.get("metric_name")
        number = row.get("data_point_value")
        if name not in counts or isinstance(number, bool) or not isinstance(number, (int, float)):
            continue
        try:
            if math.isfinite(float(number)):
                counts[name] += 1
        except (TypeError, ValueError, OverflowError):
            continue
    return counts


def check_metrics(backend, auth):
    deadline = time.monotonic() + 45
    last = {name: 0 for name in METRIC_NAMES}
    while True:
        last = query_metrics(backend, auth, dt.datetime.now(dt.timezone.utc))
        if all(last[name] > 0 for name in METRIC_NAMES):
            print("fresh_metrics_pass", " ".join(name + "=" + str(last[name]) for name in METRIC_NAMES))
            return
        if time.monotonic() >= deadline:
            fail("fresh metric query returned no numeric host/container values")
        time.sleep(5)


def query_historical_metrics(backend, auth, start, end):
    sql = (
        "SELECT metric_name, data_point_value FROM `pmcluster-metrics` WHERE "
        "p_timestamp >= TIMESTAMP '" + timestamp(start) + "' AND "
        "p_timestamp <= TIMESTAMP '" + timestamp(end) + "' LIMIT 5000"
    )
    value = json_response(
        backend,
        "/api/v1/query",
        "historical metric query",
        auth=auth,
        method="POST",
        payload={"query": sql, "startTime": timestamp(start), "endTime": timestamp(end)},
    )
    count = 0
    for row in rows_from_query(value):
        if not isinstance(row, dict):
            continue
        number = row.get("data_point_value")
        if isinstance(number, bool) or not isinstance(number, (int, float)):
            continue
        try:
            if math.isfinite(float(number)):
                count += 1
        except (TypeError, ValueError, OverflowError):
            continue
    return count


def query_historical_logs(backend, auth, start, end):
    sql = (
        "SELECT p_timestamp FROM `pmcluster-logs` WHERE "
        "p_timestamp >= TIMESTAMP '" + timestamp(start) + "' AND "
        "p_timestamp <= TIMESTAMP '" + timestamp(end) + "' LIMIT 2000"
    )
    value = json_response(
        backend,
        "/api/v1/query",
        "historical log query",
        auth=auth,
        method="POST",
        payload={"query": sql, "startTime": timestamp(start), "endTime": timestamp(end)},
    )
    return len([row for row in rows_from_query(value) if isinstance(row, dict)])


def check_history(backend, auth, minutes, streams):
    if minutes <= 10:
        fail("history check requires a window longer than ten minutes")
    now = dt.datetime.now(dt.timezone.utc)
    start = now - dt.timedelta(minutes=minutes)
    end = now - dt.timedelta(minutes=10)
    for stream in streams:
        if stream == "metrics":
            count = query_historical_metrics(backend, auth, start, end)
        elif stream == "logs":
            count = query_historical_logs(backend, auth, start, end)
        else:
            fail("unsupported historical stream")
        if count <= 0:
            fail("historical " + stream + " query returned no stored rows")
        print("history_pass", "stream=" + stream, "rows=" + str(count), "age_minutes>=10")


def check_daemon(daemon, token, expected_host, check_mutation):
    health = json_response(daemon, "/health", "daemon health")
    if health.get("status") != "ok":
        fail("daemon health did not report ok")
    print("daemon_health_pass")

    nodes = json_response(daemon, "/api/nodes", "authenticated node inventory", auth=token)
    rows = nodes.get("nodes") if isinstance(nodes, dict) else None
    if not isinstance(rows, list) or len(rows) != 1 or not isinstance(rows[0], dict):
        fail("standalone node inventory is not exactly one host")
    node = rows[0]
    if node.get("role") != "standalone" or node.get("status") != "ready" or node.get("availability") != "unknown":
        fail("node inventory is not a ready standalone host")
    hostname = str(node.get("hostname", ""))
    if expected_host and hostname.lower() != expected_host.lower():
        fail("node inventory hostname does not match this host")

    anonymous, _, _ = request(daemon, "/api/nodes")
    if anonymous != 401:
        fail("daemon API accepted an anonymous node read")
    print("daemon_auth_inventory_pass", "hostname=" + hostname)

    services = json_response(daemon, "/api/services", "standalone service inventory", auth=token)
    service_rows = services.get("services") if isinstance(services, dict) else None
    if not isinstance(service_rows, list) or any(
        not isinstance(item, dict) or item.get("standalone") is not True or item.get("desired_known") is not False
        for item in service_rows
    ):
        fail("service inventory is not marked read-only standalone")
    if check_mutation:
        status, _, _ = request(
            daemon,
            "/api/services/__nextrum_healthcheck__/__nextrum_healthcheck__/restart",
            method="POST",
            auth=token,
            payload={},
        )
        if status != 405:
            fail("standalone workload mutation was not rejected with HTTP 405")
        print("readonly_mutation_guard_pass")


def check_retention(backend, auth):
    for stream in STREAMS:
        path = "/api/v1/logstream/" + urllib.parse.quote(stream, safe="") + "/retention"
        value = json_response(backend, path, stream + " retention", auth=auth)
        durations = retention_durations(value)
        if not durations or any(item != "1d" for item in durations):
            fail(stream + " retention is not exactly 1d")
        print("retention_pass", stream + "=1d")


def check_private_ui(edge, tabs):
    for tab, marker in tabs.items():
        status, _, body = request(edge, "/web/observability?tab=" + tab)
        if status != 200:
            fail("observability " + tab + " tab returned HTTP " + str(status))
        if marker and marker not in body.decode("utf-8", errors="ignore"):
            fail("observability " + tab + " tab did not render")
        print("ui_tab_pass", tab + "=200")


def check_public_ui(origin, values, process_values, tabs):
    status, headers, _ = request(origin, "/web/observability", opener=NO_REDIRECT_OPENER)
    location = next((value for key, value in headers.items() if key.lower() == "location"), "")
    if status not in (301, 302, 303, 307, 308) or "/web/login" not in location:
        fail("public console anonymous access was not redirected to login")

    username = process_values.get("PMCLUSTER_UI_USER") or values.get("PMCLUSTER_UI_USER")
    password = process_values.get("PMCLUSTER_UI_PASS") or values.get("PMCLUSTER_UI_PASS")
    if not username or not password:
        fail("public console credentials are unavailable in the live process")

    import http.cookiejar

    cookies = http.cookiejar.CookieJar()
    client = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(cookies))
    status, _, _ = request(origin, "/web/login", opener=client)
    if status != 200:
        fail("public console login page returned HTTP " + str(status))
    form = urllib.parse.urlencode({"username": username, "password": password}).encode("utf-8")
    req = urllib.request.Request(
        origin.rstrip("/") + "/web/login",
        data=form,
        headers={
            "Accept": "text/html",
            "Content-Type": "application/x-www-form-urlencoded",
            "Origin": origin.rstrip("/"),
            "User-Agent": "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 Chrome/124 Safari/537.36",
        },
        method="POST",
    )
    try:
        with client.open(req, timeout=REQUEST_TIMEOUT) as response:
            body = response.read(MAX_RESPONSE_BYTES)
            status = response.status
    except urllib.error.HTTPError as error:
        try:
            error.read(MAX_RESPONSE_BYTES)
        except OSError:
            pass
        fail("public console login returned HTTP " + str(error.code))
    except (urllib.error.URLError, TimeoutError, OSError):
        fail("public console login request failed")
    if status != 200 or b'data-nav="observability"' not in body:
        fail("public console login did not establish an operator session")
    print("public_login_pass")

    for tab, marker in tabs.items():
        status, _, body = request(origin, "/web/observability?tab=" + tab, opener=client)
        if status != 200:
            fail("public observability " + tab + " tab returned HTTP " + str(status))
        if marker and marker not in body.decode("utf-8", errors="ignore"):
            fail("public observability " + tab + " tab did not render")
        print("ui_tab_pass", tab + "=200")


def run(args):
    os.chdir(ROOT)
    values = parse_env_file(ROOT / ".env")
    process_values = {}

    parseable_ip = compose_container_ip("parseable")
    daemon_ip = compose_container_ip("daemon", required=False)
    edge_ip = compose_container_ip("edge", required=False)
    public = daemon_ip is None and edge_ip is None
    if (daemon_ip is None) != (edge_ip is None) and not public:
        fail("Compose daemon and edge services are not running together")

    if public:
        process_values = process_environment("pmcluster-edge.service")
        if not process_values:
            fail("public edge systemd service environment is unavailable")
        daemon = "http://127.0.0.1:9090"
        origin = (
            os.environ.get("CHECK_PUBLIC_ORIGIN")
            or process_values.get("PMCLUSTER_PUBLIC_ORIGIN")
            or values.get("CHECK_PUBLIC_ORIGIN")
        )
        if not origin:
            fail("public console origin is unavailable")
        ui = origin.rstrip("/")
    else:
        daemon = "http://" + daemon_ip + ":9090"
        ui = "http://" + edge_ip + ":8042"

    token_value = process_values.get("PMCLUSTER_API_TOKEN") if public else values.get("DAEMON_TOKEN")
    token_value = token_value or values.get("DAEMON_TOKEN") or process_values.get("PMCLUSTER_API_TOKEN")
    if not token_value:
        fail("daemon API token is unavailable locally")
    token = "Bearer " + token_value
    auth = basic_auth(values, process_values)

    backend_candidates = ["http://" + parseable_ip + ":8000"]
    if public:
        backend_candidates.append("http://127.0.0.1:18080")
    backend = None
    for candidate in backend_candidates:
        try:
            status, _, _ = request(candidate, "/api/v1/about", auth=auth)
        except CheckFailure:
            continue
        if status == 200:
            backend = candidate
            break
    if backend is None:
        fail("Parseable backend did not answer its authenticated about endpoint")
    print("parseable_health_pass")

    expected_host = values.get("HOST_NAME") or socket_hostname()
    check_daemon(daemon, token, expected_host, not args.skip_mutation_check)
    check_retention(backend, auth)
    check_metrics(backend, auth)
    if args.history_minutes:
        check_history(
            backend,
            auth,
            args.history_minutes,
            args.history_stream or ["metrics"],
        )

    tabs = {
        "overview": 'id="observability-summary"',
        "logs": 'id="obs-logs"',
        # Metrics has no single wrapper when its source is empty; status still
        # proves the route renders, while the backend metric check above proves
        # the data path. Keep the marker empty for localization-safe checking.
        "metrics": "",
        "traces": 'id="obs-traces"',
    }
    if public:
        check_public_ui(ui, values, process_values, tabs)
    else:
        check_private_ui(ui, tabs)
    print("standalone_health_pass", "mode=" + ("public-deshli" if public else "compose"))


def socket_hostname():
    try:
        return os.uname().nodename
    except AttributeError:
        return ""


def main():
    parser = argparse.ArgumentParser(description="Read-only Nextrum standalone monitoring health check")
    parser.add_argument(
        "--skip-mutation-check",
        action="store_true",
        help="skip the safe HTTP 405 assertion after standalone mode is confirmed",
    )
    parser.add_argument(
        "--history-minutes",
        type=int,
        default=0,
        help="optionally require stored rows older than ten minutes in the selected streams",
    )
    parser.add_argument(
        "--history-stream",
        choices=("metrics", "logs"),
        action="append",
        help="stream to include in the optional historical check (defaults to metrics)",
    )
    args = parser.parse_args()
    try:
        run(args)
    except CheckFailure as error:
        print("health_check_failed: " + str(error), file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
