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
# tile_id derives a stable id per tile (from its title), in the shape of a real
# ULID.
#
# The shape matters, and not subtly: tile_id is the one field Parseable types
# (`tile_id: Ulid`), so the server parses and re-serialises it. A hash-derived id
# looks like a ULID but is not one, and the server rewrites its FIRST character
# (2 -> A, 3 -> B) while `layout.i` — inside the untyped passthrough — keeps the
# original. The two then disagree, and react-grid-layout keys on `i`, so the
# dashboard throws "Something went wrong".
#
# The 10-char timestamp prefix is taken verbatim from a tile the UI created, so it
# is a value the server demonstrably accepts; the remaining 16 chars come from the
# title hash, keeping ids stable across runs. Hex is a subset of Crockford base32
# (no I/L/O/U), so the result is valid.
tile_id() {
    printf '01M52G7RCJ%s' "$(printf '%s' "$1" | md5sum | cut -c1-16 | tr 'a-f' 'A-F')"
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

# add_tile DASHBOARD_ID TITLE STREAM SHAPE QUERY X_FIELD Y_FIELD X_TITLE Y_TITLE X Y W H
#
# Emits the tile shape the UI itself produces. Established by creating one tile in
# the UI and reading it back, because the server stores tiles unvalidated:
#
#   - `tileType` is `builder` for these charts (an earlier attempt sent `code`).
#   - `layout` MUST carry `i`, and it equals tile_id. It is the grid item id
#     react-grid-layout keys on: absent, the dashboard throws
#     "Something went wrong" before drawing anything.
#   - `config.axes` is what the renderer reads for the x/y fields.
#
# Queries use BACKTICK identifiers exactly as the UI writes them — no JSON
# escaping of double quotes, so none of the mangling that silently stored
# `metric_name = \'x\'` while still answering 200.
add_tile() {
    _d="$1"; _title="$2"; _stream="$3"; _shape="$4"; _query="$5"
    _xf="$6"; _yf="$7"; _xt="$8"; _yt="$9"; _x="${10}"; _y="${11}"; _w="${12}"; _h="${13}"
    _id=$(tile_id "$_title")
    _q=$(json_escape "$_query")
    _body=$(cat <<JSON
{"tile_id":"${_id}","title":"${_title}","tileType":"builder","dbName":["${_stream}"],
 "chartType":"${_shape}","chartQuery":"${_q}",
 "config":{"type":"${_shape}","colourScheme":"forest",
   "layout":{"legendPosition":"bottom"},
   "axes":{"x":{"field":"${_xf}","title":"${_xt}"},"y":{"field":"${_yf}","title":"${_yt}"},"groupBy":{"field":[]}},
   "advanced":{"dataLabels":{"enabled":false},"tooltip":{"enabled":true,"mode":"index","intersect":false}}},
 "layout":{"i":"${_id}","x":${_x},"y":${_y},"w":${_w},"h":${_h},"moved":false,"static":false}}
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

    # Tiles, in the shape the UI itself produces.
    #
    # Established by having the UI create one tile and reading it back, because
    # Parseable's model is a passthrough:
    #
    #     pub struct Tile { pub tile_id: Ulid,
    #                       #[serde(flatten)] pub other_fields: Option<Map<..>> }
    #
    # Only tile_id is typed; everything else is stored unvalidated, so a
    # malformed tile is accepted with HTTP 200 and fails in the browser instead.
    # Two details were wrong in an earlier attempt and each is fatal:
    #
    #  - layout MUST carry `i`, and it equals tile_id. It is the grid item id
    #    react-grid-layout keys on; absent, the dashboard throws
    #    "Something went wrong" before drawing anything.
    #  - tileType is `builder` for these charts (not `code`), and config carries
    #    the axes the renderer reads.
    #
    # Queries use BACKTICK identifiers, as the UI writes them: no JSON escaping,
    # and no chance of the double-quote mangling that silently produced
    # `metric_name = \'x\'` earlier.
    # Shapes vary deliberately — a dashboard of eight identical bar charts hides
    # the differences that matter. Shares of a whole (which node, which service
    # dominates) read better as pie/donut; magnitudes compare better as bars.
    #
    # Metric names come from SELECT DISTINCT metric_name, not from assumption:
    # docker_stats gives container.*, hostmetrics gives system.* per node.
    # PLATFORM vs DEPLOYED, on every widget.
    #
    # The split is computed in SQL rather than assumed, from the one dimension
    # both signals share. Field names came from the streams themselves
    # (`SELECT nope FROM ...` returns the schema), not from expectation — that is
    # how `stack.name` was found to be missing from logs entirely, and how
    # `severity_text` was confirmed present with 3 ERROR rows.
    #
    # Platform prefixes are the stacks pmcluster itself runs; everything else is
    # a tenant. `pmcluster` is the operator console, which is platform by
    # definition even though it is the control plane rather than a workload.
    _KIND="CASE WHEN \`service.name\` LIKE 'infra%' OR \`service.name\` LIKE 'observability%' OR \`service.name\` LIKE 'sso%' OR \`service.name\` LIKE 'backup%' OR \`service.name\` LIKE 'edge%' OR \`service.name\` LIKE 'pmcluster%' THEN 'platform' ELSE 'deployed' END"

    # chartType FOLLOWS THE SHAPE, and that is the whole trick.
    #
    # chartType names the renderer family, config.type names the shape inside it.
    # Pinning chartType to one value while varying config.type produced tiles that
    # drew axes and legends with nothing plotted — and when the pinned value was
    # "timeseries", pie/donut/table came out as "No data available" outright,
    # because that family has no such shapes.
    #
    # The evidence: the first revision used chartType "bar" + config.type "bar" and
    # rendered; a later revision pinned chartType "timeseries" and everything went
    # frame-only. So chartType = the shape name.
    #
    # Shapes are picked per resource rather than applied uniformly:
    #   bar   — comparisons and magnitudes (per-service volume, split by tier)
    #   table — per-node scalars, where four exact numbers beat four bars
    #
    # pie/donut are NOT used, and that is a finding rather than a preference. With
    # chartType "pie"/"donut" the tiles render their legends with the correct values
    # (0.302/0.195 for the CPU split; 63.8/18.2/3.8/3.5 per node) but draw no marks
    # at all — the shape needs an axes configuration this provisioner does not have.
    # A share-of-whole chart that draws nothing is worse than a bar that draws.
    # To finish it: create a pie tile in the UI, read it back from
    # GET /api/v1/dashboards/{id}, and copy its config verbatim — the same route
    # that produced layout.i and tileType.
    #
    # An earlier revision set chartType "timeseries" (the family the UI's own
    # time-bucketed tile carries) while varying config.type for shape. Every tile
    # then drew axes, legends and correct axis SCALES but no plotted values at all,
    # which reads as "no numbers" and is easy to misdiagnose as missing data: the
    # queries returned rows the whole time (platform=0.786/deployed=0.183; all four
    # node loads).
    #
    # The evidence that settles it is the first revision, which used chartType
    # "bar": its capture showed Node CPU % plotting real per-node values 0.5-67.9.
    # So chartType "bar" + config.type "bar" renders, and the "timeseries" family
    # only renders a time-bucketed x-axis. Categorical x axes (kind, host.name)
    # need the bar family.
    #
    # Not a style choice — it is the only combination verified to render. Tiles
    # declared as pie/donut/table showed "No data available" while their queries
    # returned rows (checked: the CPU split returned platform=0.786, deployed=0.183;
    # node load returned all four nodes), because config.type selects a shape from a
    # family the "timeseries" chartType does not provide, and the renderer then has
    # no axes to draw. bar is the proven member of that family, so it is what ships.
    #
    # Variety is worth having, but not at the cost of a dashboard that silently
    # charts nothing: the correct family strings for pie/donut/table have to be read
    # off a tile the UI itself creates, the same way layout.i and tileType were.
    # LAYOUT: a 12-column grid, so x + w must not exceed 12.
    #
    # The first pass used w=5 at x=0/5/10 — 10+5=15, past the edge — so the third
    # tile in each row overflowed its row and left a gap. w=4 at x=0/4/8 is exactly
    # 12 and packs cleanly as three columns.
    # ── the split, one widget per signal ──
    add_tile "$_d" "Log lines: platform vs deployed" pmcluster-logs bar \
        "SELECT ${_KIND} AS \`kind\`, COUNT(*) AS \`COUNT_STAR\` FROM \`pmcluster-logs\` GROUP BY \`kind\` ORDER BY \`COUNT_STAR\` DESC LIMIT 5" \
        'kind' 'COUNT_STAR' 'Tier' 'Log lines' 0 0 4 6
    add_tile "$_d" "CPU: platform vs deployed" pmcluster-metrics bar \
        "SELECT ${_KIND} AS \`kind\`, MAX(data_point_value) AS \`value\` FROM \`pmcluster-metrics\` WHERE metric_name = 'container.cpu.utilization' GROUP BY \`kind\` ORDER BY \`value\` DESC LIMIT 5" \
        'kind' 'value' 'Tier' 'CPU' 4 0 4 6
    add_tile "$_d" "Memory: platform vs deployed" pmcluster-metrics bar \
        "SELECT ${_KIND} AS \`kind\`, MAX(data_point_value) AS \`value\` FROM \`pmcluster-metrics\` WHERE metric_name = 'container.memory.percent' GROUP BY \`kind\` ORDER BY \`value\` DESC LIMIT 5" \
        'kind' 'value' 'Tier' 'Memory %' 8 0 4 6
    # NO NETWORK TILE, and not because it is unimportant. Every container network
    # metric (container.network.io.usage.rx_bytes and friends) is an int64, which
    # Parseable does not map — the row arrives with an empty value, so a network
    # tile is blank by construction and no query fixes it. Same type split that
    # makes system.memory.usage unchartable.
    #
    # When a double-valued network or throughput series exists (a rate rather than
    # a cumulative byte count, or a synthetic one from the collector), this is the
    # first tile to add back.

    # ── per node ──
    #
    # METRIC CHOICE IS NOT COSMETIC HERE. Verified by counting populated values
    # per metric on the live cluster:
    #
    #   system.cpu.time                  16 rows, 16 with a value
    #   system.cpu.load_average.1m        2 rows,  2 with a value
    #   system.memory.usage              12 rows,  0 with a value
    #   system.cpu.logical.count          2 rows,  0 with a value
    #
    # The split is exact and it is a TYPE split: every metric whose datapoints are
    # doubles lands in data_point_value, and every INTEGER-valued one arrives with
    # an empty value. Parseable does not map integer datapoints, so a node memory
    # tile built on system.memory.usage charts nothing no matter how the query is
    # written — the value was never stored. `data_point_value_double` /
    # `_int` do not exist; there is no column to reach for.
    #
    # So node CPU comes from the double-valued system.cpu.time (as a ratio of
    # non-idle to total, both of which are doubles), and node memory comes from
    # container.memory.percent, which is a double and is attributed to the node
    # because the docker detector runs first in resourcedetection.
    #
    # The `host.name LIKE 'nxt-%'` filter is not decoration: rows written before
    # that detector fix carry the CONTAINER id in host.name (1af781ba5184), and
    # grouping without it silently mixes nodes and containers in one chart.
    add_tile "$_d" "Node CPU %" pmcluster-metrics bar \
        "SELECT \`host.name\`, SUM(CASE WHEN state <> 'idle' THEN data_point_value ELSE 0 END) / SUM(data_point_value) * 100 AS \`cpu_pct\` FROM \`pmcluster-metrics\` WHERE metric_name = 'system.cpu.time' GROUP BY \`host.name\` ORDER BY \`cpu_pct\` DESC LIMIT 10" \
        'host.name' 'cpu_pct' 'Node' 'CPU %' 0 6 4 6
    add_tile "$_d" "Node memory %" pmcluster-metrics bar \
        "SELECT \`host.name\`, MAX(data_point_value) AS \`mem_pct\` FROM \`pmcluster-metrics\` WHERE metric_name = 'container.memory.percent' AND \`host.name\` LIKE 'nxt-%' GROUP BY \`host.name\` ORDER BY \`mem_pct\` DESC LIMIT 10" \
        'host.name' 'mem_pct' 'Node' 'Memory %' 4 6 4 6
    add_tile "$_d" "Node load (1m)" pmcluster-metrics table \
        "SELECT \`host.name\`, MAX(data_point_value) AS \`load_1m\` FROM \`pmcluster-metrics\` WHERE metric_name = 'system.cpu.load_average.1m' GROUP BY \`host.name\` ORDER BY \`load_1m\` DESC LIMIT 10" \
        'host.name' 'load_1m' 'Node' 'Load 1m' 8 6 4 6

    # ── per service, for the detail the split necessarily hides ──
    add_tile "$_d" "Log volume by service" pmcluster-logs bar \
        'SELECT `service.name`, COUNT(*) AS `COUNT_STAR` FROM `pmcluster-logs` GROUP BY `service.name` ORDER BY `COUNT_STAR` DESC LIMIT 12' \
        'service.name' 'COUNT_STAR' 'Service' 'Log lines' 0 12 4 6
    add_tile "$_d" "Memory % by service" pmcluster-metrics bar \
        "SELECT \`service.name\`, MAX(data_point_value) AS \`value\` FROM \`pmcluster-metrics\` WHERE metric_name = 'container.memory.percent' GROUP BY \`service.name\` ORDER BY \`value\` DESC LIMIT 12" \
        'service.name' 'value' 'Service' 'Memory %' 4 12 4 6

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
