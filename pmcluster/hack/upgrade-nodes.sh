#!/usr/bin/env bash
# Upgrade the pmcluster binary on every node of a swarm.
#
# WHY THIS EXISTS: `pmcluster cluster update` reconciles platform STACKS and
# configs. It does not replace the pmcluster binary on the nodes, and there is no
# `upgrade` command — so updating a server was an undocumented manual dance:
# download the release, checksum it, scp it to each manager, install it, restart
# the unit, and hope the leader went last. Getting that wrong is how a cluster
# ends up with mixed versions and a leader running old code.
#
# Usage:
#   hack/upgrade-nodes.sh <version> [--hosts ip,ip,ip] [--key ~/.ssh/key]
#
#   PMCLUSTER_NODES=10.0.0.1,10.0.0.2 hack/upgrade-nodes.sh v0.2.203
#
# Hosts are ordered workers -> follower managers -> leader LAST. With an
# explicit --hosts/PMCLUSTER_NODES list the *membership* comes from that list, but the
# ORDER is still derived from the swarm's roles (falling back to the stated order if
# the swarm cannot be asked), so you cannot accidentally restart the leader first.
#
# Original note:
# the leader is
# restarted last so the swarm never loses its only manager mid-upgrade. Override
# with --hosts to state the order explicitly (it is taken as given).
#
# Every step that can fail is fatal. A half-upgraded cluster is worse than an
# un-upgraded one, and the failure mode to avoid is "it printed something and
# exited 0".
set -euo pipefail

REPO="hazemarian/poor-man-cluster"
VERSION="${1:-}"
[ -n "$VERSION" ] || { echo "usage: $0 <version> [--hosts ip,ip,ip] [--key path]" >&2; exit 2; }
shift || true

HOSTS="${PMCLUSTER_NODES:-}"     # optional; discovered from the swarm when empty
KEY="${PMCLUSTER_SSH_KEY:-$HOME/.ssh/pmcluster_vps4_ed25519}"
SSHUSER="${PMCLUSTER_SSH_USER:-root}"
while [ $# -gt 0 ]; do
  case "$1" in
    --hosts) HOSTS="$2"; shift 2 ;;
    --key)   KEY="$2";   shift 2 ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
done
# NODES ARE DISCOVERED, NOT ASSUMED.
#
# The first version took an explicit IP list, which only works on the cluster it
# was written for: a second deployment means editing the script, and an edited
# script silently upgrades the wrong machines. So when no list is given, ask the
# swarm itself — one seed host is enough — and derive the IPs from the nodes'
# Status.Addr rather than from DNS or a hand-kept list.
SSHOPT=(-i "$KEY" -o BatchMode=yes -o ConnectTimeout=15 -o ServerAliveInterval=8)

# Ask ONE reachable node what the swarm looks like, printing "ip|role|leader".
# A swarm answers the same set of nodes from any manager, so a single seed is
# enough. Used twice below: to enumerate nodes when none are named, and to learn
# each named node's role so the restart order can be derived even from an
# explicit list.
swarm_roles() {
  ssh "${SSHOPT[@]}" "$SSHUSER@$1" '
    docker node ls --format "{{.ID}}" | while read -r id; do
      ip=$(docker node inspect "$id" --format "{{.Status.Addr}}")
      role=$(docker node inspect "$id" --format "{{.Spec.Role}}")
      lead=$(docker node inspect "$id" --format "{{.ManagerStatus.Leader}}")
      [ -n "$ip" ] && printf "%s|%s|%s\n" "$ip" "$role" "$lead"
    done
  ' 2>/dev/null
}

# Restart order: workers, then follower managers, leader LAST — so the swarm
# keeps a manager throughout and never loses its leader mid-upgrade. Given an
# explicit host list, sort the IDS IT NAMED by the roles discovered above,
# keeping each node's stated position as the tie-break; a name with no matching
# role (unreachable, or the swarm answered for a different set) is treated as a
# plain worker and kept, never dropped.
order_explicit_hosts() {
  # $1: comma-separated IPs ; stdin: "ip|role|leader" lines
  awk -v list="$1" -F'|' '
    { prio[$1] = ($3=="true" ? 3 : ($2=="manager" ? 2 : 1)) }
    END {
      n = split(list, a, ",")
      for (i = 1; i <= n; i++) {
        p = (a[i] in prio) ? prio[a[i]] : 1
        printf "%d\t%d\t%s\n", p, i, a[i]
      }
    }' | sort -k1,1n -k2,2n | cut -f3 | paste -sd, -
}

if [ -z "$HOSTS" ]; then
  SEED="${PMCLUSTER_SEED:-}"
  if [ -z "$SEED" ]; then
    echo "no hosts and no seed: pass --hosts, or set PMCLUSTER_SEED to any reachable node" >&2
    exit 2
  fi
  echo "▶ discovering nodes via $SEED"
  DISCOVERED=$(swarm_roles "$SEED") || { echo "  discovery failed via $SEED" >&2; exit 1; }
  [ -n "$DISCOVERED" ] || { echo "  discovery returned no nodes" >&2; exit 1; }
  HOSTS=$(printf '%s\n' "$DISCOVERED" | awk -F'|' '{ if ($3=="true") p=3; else if ($2=="manager") p=2; else p=1; print p, $1 }' | sort -k1,1n -k2,2 | awk '{print $2}' | paste -sd, -)
  echo "  discovered: $HOSTS"
  # DISCOVERY IS NOT A REACHABILITY CHECK.
  #
  # `docker node inspect … {{.Status.Addr}}` is the swarm's INTERNAL address.
  # On an Overlay/private-network deployment (wafaa's nodes answer 10.7.224.x)
  # those addresses are routable between the nodes and nowhere else, so ssh to
  # them from wherever this script runs dies with a timeout that looks like the
  # host is down. The addresses are right for ROLE ORDERING and wrong as SSH
  # targets. Say so instead of letting the operator infer it from silent hangs.
  if [ -z "${PMCLUSTER_NODES:-}" ]; then
    echo "  ⚠ those are swarm-INTERNAL addresses; they may not be reachable from here." >&2
    echo "    If the copies below time out, set PMCLUSTER_NODES (or --hosts) to the" >&2
    echo "    externally reachable addresses — this script still derives the role" >&2
    echo "    order from the swarm, so you do not have to sort them yourself." >&2
  fi
else
  # An explicit list wins for *which* nodes to touch. Its ORDER is still derived
  # from the swarm when a seed answers, because "leader last" is a safety
  # property, not a preference — but a failure here is not fatal: the list was
  # given deliberately, so fall back to the stated order rather than aborting.
  SEED="${PMCLUSTER_SEED:-${HOSTS%%,*}}"
  if ROLES=$(swarm_roles "$SEED") && [ -n "$ROLES" ]; then
    ORDERED=$(printf '%s\n' "$ROLES" | order_explicit_hosts "$HOSTS")
    [ "$ORDERED" = "$HOSTS" ] || echo "  reordered by discovered role: $ORDERED"
    HOSTS="$ORDERED"
  else
    echo "  ⚠ could not read roles via $SEED — using the given order as-is" >&2
  fi
fi

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

echo "▶ release $VERSION"
gh release download "$VERSION" -R "$REPO" -p '*linux-amd64.tar.gz' -p 'SHA256SUMS.txt' -D "$WORK" >/dev/null

# Checksums are not optional: this installs a binary that runs as root on every
# node, so an unverified download is the whole risk.
( cd "$WORK" && grep linux-amd64 SHA256SUMS.txt | sed 's/\*//' > want.txt && sha256sum -c want.txt >/dev/null 2>&1 \
  || shasum -a 256 -c want.txt >/dev/null )
echo "  checksum verified"
tar -xzf "$WORK"/pmcluster-"$VERSION"-linux-amd64.tar.gz -C "$WORK"
BIN="$WORK/pmcluster-$VERSION-linux-amd64"
[ -x "$BIN" ] || { echo "  binary missing from the archive" >&2; exit 1; }

IFS=',' read -r -a IPS <<< "$HOSTS"
echo "▶ ${#IPS[@]} node(s): ${IPS[*]}"

FAILED=0
for ip in "${IPS[@]}"; do
  echo "══════ $ip ══════"
  # COPY VIA SSH+CAT, NOT SCP. (primary path)
  #
  # WHY: scp normally rides the SFTP subsystem, which the remote sshd is free to
  # leave disabled. On a host without it the failure is `scp: Connection closed`
  # — a message that reads exactly like an auth refusal or a fail2ban ban and is
  # neither, so it sends you chasing sshd_config and firewall rules that were
  # never the problem. Piping the binary into `cat` over a plain ssh session uses
  # the exec channel only, so it works wherever ssh itself works.
  #
  # The copy is deliberately separate from (and strictly before) the install
  # below: staging into /tmp needs no privileges, so a copy failure leaves this
  # node byte-for-byte untouched and we can `continue` instead of half-applying
  # an upgrade. That is the whole point of the check.
  if ! ssh "${SSHOPT[@]}" "$SSHUSER@$ip" "cat > /tmp/pmcluster.new" < "$BIN"; then
    # FALLBACK ONLY: scp -O forces the legacy SCP protocol, which also skips the
    # SFTP subsystem. It still requires the remote `scp` binary and its own
    # quirks, so ssh+cat stays the primary path and this is a second chance, not
    # the plan.
    echo "  ssh copy failed — retrying with scp -O (legacy protocol)" >&2
    if ! scp -O -i "$KEY" -o BatchMode=yes -q "$BIN" "$SSHUSER@$ip:/tmp/pmcluster.new"; then
      echo "  copy FAILED (ssh and scp -O both failed) — nothing changed on this node" >&2
      FAILED=1; continue
    fi
  fi
  ssh "${SSHOPT[@]}" "$SSHUSER@$ip" "
    set -e
    # Not every deployment logs in as root — wafaa runs as 'wafaa' with
    # passwordless sudo. Privileged steps are written out TWICE, once per branch,
    # instead of via a \`SUDO=\"sudo -n\"; \$SUDO install …\` indirection.
    #
    # WHY, precisely: the remote login shell is the user's own — zsh on wafaa —
    # and zsh does NOT word-split an unquoted variable. \`\$SUDO install …\`
    # therefore tries to exec a command literally named \`sudo -n\` and dies with
    # \`zsh: command not found: sudo -n\`. The branch costs two lines and cannot
    # break on the shell it lands in. (\`sh -c \"\$SUDO …\"\` would also work, but
    # only because the inner sh re-splits; spelling it out is honest about what
    # runs and needs no shell-in-a-shell.)
    if [ \"\$(id -u)\" -eq 0 ]; then
      echo \"  user:   \$(whoami) (root)\"
    else
      echo \"  user:   \$(whoami) (sudo -n)\"
    fi
    echo \"  before: \$(pmcluster version 2>/dev/null | head -1)\"
    cp -a /usr/local/bin/pmcluster /usr/local/bin/pmcluster.\$VERSION.prev 2>/dev/null || true
    if [ \"\$(id -u)\" -eq 0 ]; then
      install -m 0755 /tmp/pmcluster.new /usr/local/bin/pmcluster
    else
      sudo -n install -m 0755 /tmp/pmcluster.new /usr/local/bin/pmcluster
    fi
    rm -f /tmp/pmcluster.new
    if [ \"\$(id -u)\" -eq 0 ]; then
      systemctl restart pmcluster
    else
      sudo -n systemctl restart pmcluster
    fi
    sleep 6
    echo \"  after:  \$(pmcluster version 2>/dev/null | head -1)\"
    echo \"  unit:   \$(systemctl is-active pmcluster)\"
  " || { echo \"  install/restart FAILED on $ip\" >&2; FAILED=1; }
done

if [ "$FAILED" -ne 0 ]; then
  echo "✖ upgrade incomplete — some nodes are still on the old binary" >&2
  exit 1
fi

# Reconcile stacks and configs AFTER the binaries agree, so a re-render is done by
# the new version everywhere.
#
# Single-quoted, so this runs on the node with its own login shell — which brings
# back the zsh no-word-split problem the block above has: a bare `$SUDO pmcluster …`
# is `command not found: sudo -n` there too, and because the failure is piped into
# `grep | tail` it exits 0 and the reconcile is silently skipped. Same explicit
# branch, same reason.
echo "▶ reconciling platform stacks + configs"
ssh "${SSHOPT[@]}" "$SSHUSER@${IPS[${#IPS[@]}-1]}" 'if [ "$(id -u)" -eq 0 ]; then pmcluster cluster update; else sudo -n pmcluster cluster update; fi' 2>&1 | grep -viE 'telemetry|4318|export:' | tail -12
echo "✔ upgraded to $VERSION"
