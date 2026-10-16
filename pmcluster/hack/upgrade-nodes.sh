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
# Hosts are ordered followers-first, leader-last by default: the leader is
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

HOSTS="${PMCLUSTER_NODES:-}"
KEY="${PMCLUSTER_SSH_KEY:-$HOME/.ssh/pmcluster_vps4_ed25519}"
while [ $# -gt 0 ]; do
  case "$1" in
    --hosts) HOSTS="$2"; shift 2 ;;
    --key)   KEY="$2";   shift 2 ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
done
[ -n "$HOSTS" ] || { echo "no hosts: set PMCLUSTER_NODES or pass --hosts" >&2; exit 2; }

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
SSHOPT=(-i "$KEY" -o BatchMode=yes -o ConnectTimeout=15 -o ServerAliveInterval=8)

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
  if ! scp -i "$KEY" -o BatchMode=yes -q "$BIN" "root@$ip:/root/pmcluster.new"; then
    echo "  scp FAILED — is SSH blocked (fail2ban)? nothing changed on this node" >&2
    FAILED=1; continue
  fi
  ssh "${SSHOPT[@]}" "root@$ip" "
    set -e
    echo \"  before: \$(pmcluster version 2>/dev/null | head -1)\"
    cp -a /usr/local/bin/pmcluster /usr/local/bin/pmcluster.\$VERSION.prev 2>/dev/null || true
    install -m 0755 /root/pmcluster.new /usr/local/bin/pmcluster
    rm -f /root/pmcluster.new
    systemctl restart pmcluster
    sleep 6
    echo \"  after:  \$(pmcluster version 2>/dev/null | head -1)\"
    echo \"  unit:   \$(systemctl is-active pmcluster)\"
  " || { echo "  install/restart FAILED on $ip" >&2; FAILED=1; }
done

if [ "$FAILED" -ne 0 ]; then
  echo "✖ upgrade incomplete — some nodes are still on the old binary" >&2
  exit 1
fi

# Reconcile stacks and configs AFTER the binaries agree, so a re-render is done by
# the new version everywhere.
echo "▶ reconciling platform stacks + configs"
ssh "${SSHOPT[@]}" "root@${IPS[${#IPS[@]}-1]}" 'pmcluster cluster update' 2>&1 | grep -viE 'telemetry|4318|export:' | tail -12
echo "✔ upgraded to $VERSION"
