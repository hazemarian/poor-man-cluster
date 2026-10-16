#!/bin/sh
# Applied by a one-shot service that runs INSIDE the swarm network, so
# Parseable needs no published port at all.
#
# Why not the daemon: the daemon is a systemd HOST process, not a member of a
# swarm network, so it cannot resolve `observability_parseable`. Publishing 8000
# on the routing mesh was a workaround for calling from the wrong place — this
# puts the call where the name resolves.
#
# Parseable takes NO retention from env, from a flag or from a config file; it
# is API-only, so this is the mechanism. The body must be a JSON SEQUENCE and
# `action` is mandatory — a bare object is rejected with
# "invalid type: map, expected a sequence".
set -u

BASE="${P_BASE:-http://observability_parseable:8000}"
: "${P_USER:?P_USER is required}"
: "${P_PASS:?P_PASS is required}"

# Basic auth assembled from the environment, never from argv: a credential in a
# process list is readable by anything on the node.
AUTH="$(printf '%s:%s' "$P_USER" "$P_PASS" | base64 | tr -d '\n')"

# apply STREAM DAYS — PUT one stream's retention, retrying until the server
# answers. The stack gives no ordering guarantees (swarm ignores depends_on on
# this path), so the wait lives here.
apply() {
    stream="$1"
    days="${2:-}"

    # An unset or non-positive setting means "not configured": leave whatever
    # retention is already on the stream alone rather than clearing it.
    case "$days" in
        ''|*[!0-9]*) return 0 ;;
    esac
    [ "$days" -gt 0 ] || return 0

    payload="[{\"duration\":\"${days}d\",\"action\":\"delete\",\"description\":\"pmcluster ${stream}\"}]"

    attempt=0
    while [ "$attempt" -lt 60 ]; do
        code="$(curl -sS -o /dev/null -w '%{http_code}' -m 10 -X PUT \
            -H "Authorization: Basic ${AUTH}" \
            -H 'Content-Type: application/json' \
            --data "$payload" \
            "${BASE}/api/v1/logstream/${stream}/retention" 2>/dev/null || echo 000)"
        case "$code" in
            2*)
                echo "  ${stream}: retention ${days}d applied"
                return 0
                ;;
        esac
        attempt=$((attempt + 1))
        sleep 5
    done

    # Best-effort by design: an unreachable backend must not fail the deploy.
    echo "  ${stream}: retention NOT applied (last HTTP ${code})" >&2
    return 0
}

apply pmcluster-logs "${LOGS_RETENTION_DAYS:-}"
apply pmcluster-metrics "${METRICS_RETENTION_DAYS:-}"
apply pmcluster-traces "${TRACES_RETENTION_DAYS:-}"

echo "observability retention converged"
