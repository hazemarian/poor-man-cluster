#!/bin/sh
# Provisioning for the observability backend, run INSIDE the swarm network by a
# one-shot service so the backend needs no published port.
#
# Why not the daemon: it is a systemd HOST process, not a member of a swarm
# network, so it cannot resolve `observability_parseable`. Publishing a port was
# a workaround for calling from the wrong place — this calls from the right one.
#
# Parseable takes NO retention from env, from a flag or from a config file, and
# dashboards only exist through its API, so the API is the only mechanism. Three
# things converge here, all idempotent, all best-effort: per-stream retention,
# the platform dashboard, and its tiles.
set -u

BASE="${P_BASE:-http://observability_parseable:8000}"
: "${P_USER:?P_USER is required}"
: "${P_PASS:?P_PASS is required}"

# Basic auth from the environment, never argv: a credential in a process list is
# readable by anything on the node.
AUTH="$(printf '%s:%s' "$P_USER" "$P_PASS" | base64 | tr -d '\n')"

api() { # api METHOD PATH [BODY]
    _m="$1"; _p="$2"; _b="${3:-}"
    if [ -n "$_b" ]; then
        curl -sS -m 20 -X "$_m" \
            -H "Authorization: Basic ${AUTH}" -H 'Content-Type: application/json' \
            --data "$_b" "${BASE}${_p}" 2>/dev/null
    else
        curl -sS -m 20 -X "$_m" -H "Authorization: Basic ${AUTH}" "${BASE}${_p}" 2>/dev/null
    fi
}

# wait_ready blocks until the API answers, because the stack gives no ordering
# guarantees (swarm ignores depends_on on this path).
wait_ready() {
    _i=0
    while [ "$_i" -lt 60 ]; do
        if curl -sS -m 5 -o /dev/null -H "Authorization: Basic ${AUTH}" "${BASE}/api/v1/about" 2>/dev/null; then
            return 0
        fi
        _i=$((_i + 1))
        sleep 5
    done
    echo "observability backend unreachable after 300s — skipping provisioning" >&2
    return 1
}

# ── retention ───────────────────────────────────────────────────────────────
# The body must be a JSON SEQUENCE and `action` is mandatory; a bare object is
# rejected with "invalid type: map, expected a sequence".
apply_retention() {
    _stream="$1"; _days="${2:-}"
    case "$_days" in
        ''|*[!0-9]*) return 0 ;;   # unset/non-numeric: leave what is there
    esac
    [ "$_days" -gt 0 ] || return 0
    _payload="[{\"duration\":\"${_days}d\",\"action\":\"delete\",\"description\":\"pmcluster ${_stream}\"}]"
    _code=$(curl -sS -o /dev/null -w '%{http_code}' -m 10 -X PUT \
        -H "Authorization: Basic ${AUTH}" -H 'Content-Type: application/json' \
        --data "$_payload" "${BASE}/api/v1/logstream/${_stream}/retention" 2>/dev/null || echo 000)
    case "$_code" in
        2*) echo "  ${_stream}: retention ${_days}d applied" ;;
        *)  echo "  ${_stream}: retention NOT applied (HTTP ${_code})" >&2 ;;
    esac
}

# ── dashboard ───────────────────────────────────────────────────────────────
# A tile_id is any id string; it only has to be unique within the dashboard, so
# it is derived from the tile title to stay stable across runs.
tile_id() {
    printf '%s' "$1" | md5sum | cut -c1-26 | tr 'a-f' 'A-F'
}

# json_escape escapes the two characters JSON string interpolation needs.
#
# Done in awk, not sed: sed's backslash handling differs between this BusyBox and
# the host shell, and getting it wrong does not fail loudly — the API answers 200
# and stores SQL with stray backslashes (`metric_name = \'x\'`), which then
# matches nothing and renders an empty tile.
json_escape() {
    printf '%s' "$1" | awk '{ gsub(/\\/, "\\\\"); gsub(/"/, "\\\""); printf "%s", $0 }'
}

# add_tile DASHBOARD_ID TITLE TILE_TYPE STREAM CHART QUERY X Y W H
add_tile() {
    _d="$1"; _title="$2"; _type="$3"; _stream="$4"; _chart="$5"; _query="$6"
    _x="$7"; _y="$8"; _w="$9"; _h="${10}"
    _id=$(tile_id "$_title")
    # Two things the API accepts but the UI cannot render:
    #  - chartQuery is what the tile actually charts. Its renderer does
    #    `typeof chartQuery === "string" ? chartQuery : convertObject(chartQuery)`,
    #    so omitting it passes validation and then throws in the browser
    #    ("Something went wrong"). A string is accepted verbatim, so the SQL goes
    #    in directly.
    #  - the query carries double-quoted identifiers ("service.name"), which must
    #    be escaped inside a JSON string or the body is invalid (API answers 400).
    _q=$(json_escape "$_query")
    _body=$(cat <<JSON
{"tile_id":"${_id}","title":"${_title}","tileType":"${_type}","dbName":["${_stream}"],
 "chartType":"${_chart}","chartQuery":"${_q}","config":{"type":"${_chart}"},
 "layout":{"x":${_x},"y":${_y},"w":${_w},"h":${_h}}}
JSON
)
    _code=$(curl -sS -o /dev/null -w '%{http_code}' -m 20 -X PUT \
        -H "Authorization: Basic ${AUTH}" -H 'Content-Type: application/json' \
        --data "$_body" "${BASE}/api/v1/dashboards/${_d}/add_tile" 2>/dev/null || echo 000)
    case "$_code" in
        2*) echo "  tile: ${_title}" ;;
        *)  echo "  tile ${_title}: NOT added (HTTP ${_code})" >&2 ;;
    esac
}

provision_dashboard() {
    _title="pmcluster"

    # Idempotent by title: the list is a JSON array and this image has no jq, so
    # match the literal title in the response body.
    _list=$(api GET /api/v1/dashboards)
    case "$_list" in
        *"\"title\":\"${_title}\""*)
            echo "  dashboard '${_title}' already exists — leaving it alone"
            return 0
            ;;
    esac

    _created=$(api POST /api/v1/dashboards \
        "{\"title\":\"${_title}\",\"description\":\"Platform overview: ingestion, errors and per-service resource use.\",\"tags\":[\"pmcluster\"]}")
    # dashboardId is the only ULID-shaped value in the response.
    _d=$(printf '%s' "$_created" | sed -n 's/.*"dashboardId":"\([^"]*\)".*/\1/p')
    if [ -z "$_d" ]; then
        echo "  dashboard could not be created: $(printf '%s' "$_created" | head -c 160)" >&2
        return 0
    fi
    echo "  dashboard '${_title}' created: ${_d}"

    # Field names come from the live schema, not from assumptions:
    #   logs    -> service.name, severity_text, time
    #   metrics -> metric_name, data_point_value, service.name
    add_tile "$_d" "Log volume by service" code pmcluster-logs timeseries \
        'SELECT "service.name", count(*) AS lines FROM "pmcluster-logs" GROUP BY "service.name" ORDER BY lines DESC LIMIT 10' 0 0 6 4
    add_tile "$_d" "Errors by service" code pmcluster-logs bar \
        'SELECT "service.name", count(*) AS errors FROM "pmcluster-logs" WHERE severity_text = '"'"'ERROR'"'"' GROUP BY "service.name" ORDER BY errors DESC LIMIT 10' 6 0 6 4
    # NOTE: no "memory bytes" tile. `container.memory.usage.total` has rows but its
    # data_point_value is null on this build, so the tile renders empty; the
    # percent metric is the one that actually charts memory. Verified by comparing
    # count(*) (1881 rows, both) against the selected value column (null vs real).
    add_tile "$_d" "Container memory %" code pmcluster-metrics bar \
        'SELECT "service.name", max(data_point_value) AS percent FROM "pmcluster-metrics" WHERE metric_name = '"'"'container.memory.percent'"'"' GROUP BY "service.name" ORDER BY percent DESC LIMIT 10' 6 4 6 4
    add_tile "$_d" "Network received by service" code pmcluster-metrics timeseries \
        'SELECT "service.name", sum(data_point_value) AS rx_bytes FROM "pmcluster-metrics" WHERE metric_name = '"'"'container.network.io.usage.rx_bytes'"'"' GROUP BY "service.name" ORDER BY rx_bytes DESC LIMIT 10' 0 8 6 4
    add_tile "$_d" "CPU by service" code pmcluster-metrics timeseries \
        'SELECT "service.name", max(data_point_value) AS cpu FROM "pmcluster-metrics" WHERE metric_name = '"'"'container.cpu.utilization'"'"' GROUP BY "service.name" ORDER BY cpu DESC LIMIT 10' 6 8 6 4
}

if wait_ready; then
    echo "observability retention:"
    apply_retention pmcluster-logs "${LOGS_RETENTION_DAYS:-}"
    apply_retention pmcluster-metrics "${METRICS_RETENTION_DAYS:-}"
    apply_retention pmcluster-traces "${TRACES_RETENTION_DAYS:-}"
    echo "observability dashboard:"
    provision_dashboard
    echo "observability provisioning converged"
fi
