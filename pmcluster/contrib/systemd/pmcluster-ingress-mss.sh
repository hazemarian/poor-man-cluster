#!/bin/sh
# Limit routing-mesh TCP segments when the overlay runs over a smaller VPN MTU.
set -eu
MSS=${PMCLUSTER_INGRESS_MSS:-1200}
NETNS=${PMCLUSTER_INGRESS_NETNS:-/run/docker/netns/ingress_sbox}
case "$MSS" in ''|*[!0-9]*) echo 'Invalid ingress MSS' >&2; exit 1;; esac
[ "$MSS" -ge 536 ] && [ "$MSS" -le 1400 ] || exit 1
# Docker may not have created its sandbox yet; the timer retries later.
[ -e "$NETNS" ] || exit 0
existing=$(nsenter --net="$NETNS" nft list table inet pmcluster_ingress_mss 2>/dev/null || true)
if ! printf '%s' "$existing" | grep -q "maxseg size set $MSS"; then
  {
    [ -z "$existing" ] || printf '%s\n' 'delete table inet pmcluster_ingress_mss'
    cat <<EOF
table inet pmcluster_ingress_mss {
 chain postrouting {
  type filter hook postrouting priority mangle; policy accept;
  tcp flags & (syn | rst) == syn tcp option maxseg size > $MSS tcp option maxseg size set $MSS
 }
}
EOF
  } | nsenter --net="$NETNS" nft -f -
fi
# Remove the diagnostic rule only after the durable replacement exists.
if nsenter --net="$NETNS" nft list table ip pmcluster_mtu_probe >/dev/null 2>&1; then
  nsenter --net="$NETNS" nft delete table ip pmcluster_mtu_probe
fi

# Optional recovery for a verified ingress endpoint missing from NetworkDB.
# Each row: full-network-id overlay-ip mac-address private-vtep-ip.
# Never carry a mapping into a replacement network with a different identity.
PEERS=${PMCLUSTER_INGRESS_PEERS_FILE:-/etc/pmcluster/ingress-recovery-peers}
if [ -r "$PEERS" ]; then
  network=$(docker network inspect ingress --format '{{.Id}}')
  overlay="/run/docker/netns/1-$(printf '%.10s' "$network")"
  [ -e "$overlay" ] || exit 0
  while read -r expected address mac vtep extra; do
    [ -n "$expected" ] || continue
    case "$expected" in \#*) continue;; esac
    [ "$expected" = "$network" ] || continue
    [ -z "$extra" ] || { echo 'Invalid ingress recovery row' >&2; exit 1; }
    nsenter --net="$overlay" ip neigh replace "$address" lladdr "$mac" nud permanent dev vxlan0
    nsenter --net="$overlay" bridge fdb replace "$mac" dev vxlan0 dst "$vtep" self permanent
  done < "$PEERS"
fi
