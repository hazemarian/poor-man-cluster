#!/usr/bin/env bash
# Rotate the registry credential pmcluster uses for private image pulls.
#
# WHY THIS EXISTS: `pmcluster registry add` is the correct primitive, but it
# takes a secret on stdin — easy to fumble when the token ends up in shell
# history, in `ps` output, or in a CI log because someone typed it as an
# argument. This script keeps the token on stdin from end to end (never argv,
# never echoed, never logged), refuses an empty or malformed token, and prints
# the verification commands you must run afterwards — because "rotated" only
# means "works" once a worker has actually pulled with the new credential.
#
# Usage — run on the Swarm LEADER. `pmcluster registry ...` writes the LOCAL
# store and runs `docker login` on the machine you invoke it from, and the
# whole `registry` command group is leader-gated (pmcluster/internal/cli/
# leader_guard.go:74): on a follower it refuses before touching anything.
#
#   # pipe the token from your vault (1Password / gh / a file you cat):
#   op item get ghcr-rotate --fields one-time --reveal | \
#       pmcluster/hack/registry-rotate.sh --username nextrum-sy
#
#   # with the host and a verification image stated explicitly:
#   cat /tmp/token | pmcluster/hack/registry-rotate.sh ghcr.io \
#       --username nextrum-sy --image ghcr.io/nextrum-sy/pmcluster:edge
#
# Notes:
#   - The token must be a single non-whitespace line; a trailing newline is
#     stripped by pmcluster itself (pmcluster/internal/cli/registry.go:181),
#     we strip surrounding whitespace first and reject anything else.
#   - --image is optional; it only makes the printed verification commands
#     concrete. This script never pulls anything itself.
#   - Order matters: register the new token, verify, THEN revoke the old one.
#     Revoking first is how the cluster wakes up in ImagePullBackOff.
#     The full runbook is docs/registry-credentials.md.
set -euo pipefail

HOST="ghcr.io"
USERNAME=""
IMAGE=""
PMCLUSTER_BIN="${PMCLUSTER_BIN:-pmcluster}"

die() { echo "registry-rotate: $*" >&2; exit 1; }

usage() {
  echo "usage: $0 [<host>] --username <registry-user> [--image <full-image-ref>]  (token on stdin)" >&2
  echo "  <host> defaults to ghcr.io; set PMCLUSTER_BIN=/path/to/pmcluster if not on PATH" >&2
}

[ $# -gt 0 ] || { usage; exit 2; }
while [ $# -gt 0 ]; do
  case "$1" in
    --username) [ $# -ge 2 ] || die "--username needs a value"; USERNAME="$2"; shift 2 ;;
    --image)    [ $# -ge 2 ] || die "--image needs a value";    IMAGE="$2";    shift 2 ;;
    -h|--help)  usage; exit 0 ;;
    -*)         die "unknown flag: $1" ;;
    *)          case "$1" in *[![:graph:]]*) die "host must be a single printable token";; esac
                HOST="$1"; shift ;;
  esac
done

[ -n "$USERNAME" ] || die "--username is required (for ghcr.io: the GitHub login that owns the token)"
case "$HOST" in
  ghcr.io) echo "registry-rotate: note — a ghcr.io token needs 'read:packages'; see docs/registry-credentials.md §4 for the fine-grained-PAT caveat." >&2 ;;
esac
if [ "${PMCLUSTER_BIN#/}" != "$PMCLUSTER_BIN" ] || [ "${PMCLUSTER_BIN#.}" != "$PMCLUSTER_BIN" ]; then
  [ -x "$PMCLUSTER_BIN" ] || die "'$PMCLUSTER_BIN' is not an executable file — set PMCLUSTER_BIN to a real path"
else
  command -v "$PMCLUSTER_BIN" >/dev/null 2>&1 || die "'$PMCLUSTER_BIN' not found on PATH — install pmcluster or set PMCLUSTER_BIN=/path/to/pmcluster"
fi

# --- read the token from stdin ONLY ----------------------------------------
if [ -t 0 ]; then
  die "stdin is an interactive terminal. Pipe the token in from your vault (see the usage header) — pasting echoes it to the screen and into terminal scrollback/logs."
fi
token="$(cat)"
# strip surrounding whitespace (one trailing newline from echo/paste is fine)
token="${token#"${token%%[![:space:]]*}"}"
token="${token%"${token##*[![:space:]]}"}"
[ -n "$token" ] || die "empty token on stdin — refusing to run"
[[ ! "$token" =~ [[:space:]] ]] || die "token contains whitespace — stdin looks like pasted prose or a command line, not a raw token"

printf '%s' "$token" | "$PMCLUSTER_BIN" registry add "$HOST" --username "$USERNAME" --password-stdin \
  || die "pmcluster registry add failed — the store was NOT updated: docker login runs before the DB write (pmcluster/internal/cli/registry.go:88-95), so a rejected token leaves the old credential in place. Fix the token/permissions and re-run."
unset token   # gone from the shell's memory as soon as the register step is done

cat <<EOF

✅ Credential for $HOST re-registered: pmcluster ran docker login (new token
   now in the manager's ~/.docker/config.json) and overwrote the encrypted
   row in the control-plane store. The OLD token is still valid until you
   revoke it. Verify before revoking:

  1. Redeploy so services pick up the forwarded auth (deploy always runs
     'docker stack deploy --with-registry-auth', pmcluster/internal/cluster/
     stacks.go:85):
       pmcluster cluster update
  2. Prove a REAL pull on a worker — re-pull so the answer cannot come from
     a cached copy:
       docker rmi ${IMAGE:-<image>} || true
       docker pull ${IMAGE:-<image>}
  3. Prove scheduled services actually pulled, not just your shell:
       docker service ps --no-trunc <stack>_<service>
     (tasks must converge to 'running'; a bad credential shows as
      'ImagePullBackOff' / 'rejected: ... pull access denied')

EOF

cat <<EOF
  4. Revoke the OLD token:  https://github.com/settings/tokens
     (fine-grained: .../settings/personal-access-tokens)  — only after 1–3 pass.
  5. Record the new token's expiry in your rotation calendar. The new
     credential shows in:  pmcluster registry list
EOF
[ "$HOST" = "ghcr.io" ] && echo "     (its timestamp column is when the host row was FIRST added — an overwrite during rotation keeps the original created_at; pmcluster/internal/cli/registry.go:101-105 + internal/store/registries.go:50)."
echo
echo "This script never prints the token and never passes it as an argument."
