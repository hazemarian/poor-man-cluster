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
# explicit --hosts/PMCLUSTER_NODES list the *membership* comes from that list, but
# the ORDER comes from asking each node what it is — the swarm's own node list
# answers with INTERNAL addresses, which is why matching the list against it
# silently degraded to "whatever order you typed". A run that cannot identify a
# leader refuses rather than guessing; --assume-order proceeds anyway.
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
ASSUME_ORDER=0
VERSION="${1:-}"
[ -n "$VERSION" ] || { echo "usage: $0 <version> [--hosts ip,ip,ip] [--key path] [--assume-order]" >&2; exit 2; }
shift || true

HOSTS="${PMCLUSTER_NODES:-}"     # optional; discovered from the swarm when empty
KEY="${PMCLUSTER_SSH_KEY:-$HOME/.ssh/pmcluster_vps4_ed25519}"
SSHUSER="${PMCLUSTER_SSH_USER:-root}"
while [ $# -gt 0 ]; do
  case "$1" in
    --hosts) HOSTS="$2"; shift 2 ;;
    --key)   KEY="$2";   shift 2 ;;
    --assume-order) ASSUME_ORDER=1; shift ;;
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

# Ask ONE HOST what it is, over the same ssh channel the copy below uses.
# Prints "role|leader": "manager|true", "manager|false", "worker|", or "" when the
# host could not be asked at all.
#
# WHY PER HOST, not "match the list against the swarm":
#   the swarm answers docker node inspect with INTERNAL addresses (10.7.224.x on an
#   overlay deployment), while the list you pass carries the PUBLIC ones you can ssh
#   to. The old code sorted the given names by matching them against those internal
#   addresses, so on that topology NOTHING matched, every host fell through to
#   "plain worker", and the restart order silently became whatever order you typed —
#   the one case where "leader last" was not guaranteed was also the only case that
#   printed nothing. Identity from the node itself needs no address to match.
host_identity() {
  ssh "${SSHOPT[@]}" "$SSHUSER@$1" '
    if ! command -v docker >/dev/null 2>&1; then echo "nodocker|"; exit 0; fi
    # docker node inspect self is manager-only: on a worker it fails, and failing is
    # the answer (a worker has no ManagerStatus to report).
    docker node inspect self --format "{{.Spec.Role}}|{{.ManagerStatus.Leader}}" 2>/dev/null || echo "worker|"
  ' 2>/dev/null
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
  # An explicit list wins for WHICH nodes to touch. Its ORDER comes from each node
  # saying what it is, because "leader last" is a safety property, not a preference:
  # the reconcile at the end runs on the last host, and a leader restarted first
  # leaves every later node unable to reconcile (and the swarm briefly leaderless).
  echo "▶ reading each node's role (workers → follower managers → leader last)"
  PROBED=""
  UNKNOWN=""
  LEADER_LAST=""
  idx=0
  for ip in $(printf '%s' "$HOSTS" | tr ',' ' '); do
    idx=$((idx + 1))
    # Not "ident=$(…)" alone: ssh exits 255 on an unreachable or banned host, and
    # under `set -e` that aborted the run BEFORE a single node was touched, with no
    # message. A host that cannot be asked is exactly what UNKNOWN is for.
    ident=$(host_identity "$ip") || ident=""
    role=${ident%%|*}
    lead=${ident##*|}
    case "$role" in
      manager) if [ "$lead" = "true" ]; then prio=3; LEADER_LAST="$ip"; else prio=2; fi ;;
      worker)  prio=1 ;;
      *)       prio=1; UNKNOWN="$UNKNOWN $ip" ;;
    esac
    PROBED="$PROBED$prio|$idx|$ip
"
  done
  # prio, then the order you typed as the tie-break — so a fleet of workers keeps
  # its stated order and the leader is hoisted to the back.
  HOSTS=$(printf '%s' "$PROBED" | sort -t'|' -k1,1n -k2,2n | cut -d'|' -f3 | paste -sd, -)
  echo "  order:  $HOSTS"
  if [ -n "$UNKNOWN" ]; then
    # A swarm has ONE leader, so if another host claimed it, an unaskable host is
    # not the leader and its position cannot cost the run. Say which case it is
    # instead of warning about a danger that does not exist.
    if [ -n "$LEADER_LAST" ]; then
      echo "  ⚠ could not ask:$UNKNOWN — treated as workers; the leader ($LEADER_LAST) is last regardless." >&2
    else
      echo "  ⚠ could not ask:$UNKNOWN — treated as workers, kept in the order given." >&2
    fi
  fi
  if [ -z "$LEADER_LAST" ] && [ "${ASSUME_ORDER:-0}" != "1" ]; then
    echo "  ✖ no host identified itself as the swarm leader, so \"leader last\" cannot be" >&2
    echo "    guaranteed — that is the one ordering that must not be guessed. Every host" >&2
    echo "    was asked; the ones that did not answer are named above." >&2
    echo "    Re-run with --assume-order to upgrade anyway: the roles that WERE read still" >&2
    echo "    order the run, and the hosts that could not be asked keep the order you gave." >&2
    exit 1
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
      # A REFUSED PORT 22 IS NOT A DEAD SSHD. fail2ban/sshguard REJECT with a TCP
      # reset, so `Connection refused` is exactly what a temporary ban looks like —
      # and it looks identical to a host whose sshd is down. Seen live: a burst of
      # connections during an upgrade got this workstation banned from one node,
      # the node was reported "failed", and it had in fact installed the new binary
      # a minute earlier. Ban windows expire, so the honest advice is "retry later
      # and verify the version", not "this node is broken".
      if ! (exec 3<>"/dev/tcp/$ip/22") 2>/dev/null; then
        echo "  copy FAILED — port 22 is REFUSED (before any change was made to this node)." >&2
        echo "  A refused port is either a dead sshd or a temporary fail2ban/sshguard ban on this" >&2
        echo "  IP — they are indistinguishable from here. Bans lift by themselves: wait a few" >&2
        echo "  minutes, re-run, and check the node's actual version before assuming it was missed:" >&2
        echo "    ssh -i $KEY $SSHUSER@$ip 'pmcluster version'" >&2
      else
        echo "  copy FAILED (ssh and scp -O both failed) — port 22 answers, so this is auth or" >&2
        echo "  remote-binary trouble, not a network ban. Nothing has changed on this node." >&2
      fi
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
