#!/usr/bin/env bash
#
# harden-host.sh — one-shot host hardening for an Ubuntu server running
# pmcluster (+ its Docker Swarm stack).
#
# Applies, in order:
#   1. ufw host firewall (default-deny incoming; open 22/80/443 only)
#   2. iptables DOCKER-USER rules — close the "bypasses Traefik" published
#      ports (OTLP 4318, Parseable 8000) and the swarm control plane
#      (2377/7946) to the WAN while letting docker/overlay subnets and
#      loopback keep talking
#   3. fail2ban — SSH brute-force defense
#   4. pending security updates
#
# SAFE SUBSET: it does NOT disable SSH password authentication. Do that
# separately, only after a public key is installed, or you will lock yourself
# out. See the comment in the "SSH hardening (manual)" section.
#
# Idempotent — safe to re-run (firewall chains/DOCKER-USER are flushed first).
# Tested on Ubuntu 24.04/26.04 ("resolute").
#
# Usage:  sudo ./harden-host.sh
#
set -euo pipefail

# ──────────────────────────── Tunables ────────────────────────────
#
# Comma-separated docker/overlay subnets that MUST keep reaching the cluster's
# internal services. Traefik reaches the pmcluster API via the docker gateway
# host (host.docker.internal = the bridge GW IP), so this subnet class must be
# allowed through to 9090/4318 while the WAN is not.
DOCKER_SUBNETS="${DOCKER_SUBNETS:-172.16.0.0/12}"

# Everything PRIVATE is trusted, everything else is the WAN. This is the whole
# rule, and assuming 172.16.0.0/12 alone is a production outage: on a node whose
# overlay pool is 10.0.0.0/8 and whose peers live on the provider's private
# network (10.7.224.0/24 here), an ingress-published port IS reached from another
# node over VXLAN carrying that peer's private address. Swarm keeps ESTABLISHED
# tunnels alive, so the cluster keeps reporting Ready while every NEW cross-node
# connection is dropped — the failure looks like nothing at all.
PRIVATE_SUBNETS="${PRIVATE_SUBNETS:-127.0.0.0/8 10.0.0.0/8 172.16.0.0/12 192.168.0.0/16}"
# Swarm control + overlay ports must be open to that same private space, or the
# control plane is blocked by the rule written to protect it.
SWARM_PEER_SOURCES="${SWARM_PEER_SOURCES:-$PRIVATE_SUBNETS}"

# WAN-facing ports to keep publicly open.
WAN_PORTS="${WAN_PORTS:-22 80 443}"

# Docker-PUBLISHED ports (reachable via the routing mesh / docker-proxy) that
# must NOT be open to the internet.
#   4318 = OTel collector OTLP (the obvious Traefik bypass)
#   8000 = Parseable HTTP API, published on the routing mesh so the HOST daemon
#          can read telemetry at 127.0.0.1:8000 on every node. It carries the
#          admin credential, so a WAN-reachable 8000 is a cluster-wide telemetry
#          read/write exposure.
# Add any other published-but-private ports here as you expose them.
RESTRICTED_DOCKER_PORTS="${RESTRICTED_DOCKER_PORTS:-4318 8000}"

# Host-bound control-plane ports that must not be WAN-reachable:
#   9090 = pmcluster management API (bound on all interfaces, Traefik reaches
#          it over the docker gateway so we only block the public side)
#   2377 = Docker Swarm manager/join  7946 = Swarm gossip
RESTRICTED_HOST_PORTS="${RESTRICTED_HOST_PORTS:-9090 2377 7946}"

# Comma-separated peer IPs allowed to reach the swarm control plane
# (add manager/worker nodes in a multi-node swarm; single-node → leave empty).
# Export this when hardening each node: SWARM_PEERS=<ip-of-the-other-node>
# Set via env so the committed file stays generic.
SWARM_PEERS="${SWARM_PEERS:-}"

# ───────────────────────────── Helpers ────────────────────────────
info() { echo "==> $*"; }
die()  { echo "ERROR: $*" >&2; exit 1; }

require_root() { [ "$(id -u)" -eq 0 ] || die "run with sudo / as root"; }

ip_to_list() { # "a,b,c" -> "a b c"
  IFS=',' read -r -a _args <<< "$1"; echo "${_args[@]}"
}

# ────────────────────────── 1) Firewall ────────────────────────────
apply_firewall() {
  info "Installing ufw"
  apt-get install -y ufw >/dev/null

  # Docker programs its own FORWARD/iptables chains. ufw's default FORWARD
  # policy of DROP breaks overlay networking + swarm ingress, so we must set
  # ACCEPT before enabling ufw, then rely on the specific rules below.
  info "Setting ufw FORWARD policy to ACCEPT (required for Docker/Swarm)"
  sed -i 's/^DEFAULT_FORWARD_POLICY=.*/DEFAULT_FORWARD_POLICY="ACCEPT"/' /etc/default/ufw

  info "Configuring ufw rules"
  ufw --force reset >/dev/null
  ufw default deny incoming >/dev/null
  ufw default allow outgoing >/dev/null

  for p in $WAN_PORTS; do
    ufw allow "$p/tcp" >/dev/null
  done

  # pmcluster API + OTLP: keep reachable only from docker/overlay subnets
  # (the process/listener hears on all interfaces; this blocks the public side
  # while letting Traefik's host.docker.internal path through).
  for sn in $(ip_to_list "$DOCKER_SUBNETS"); do
    for p in $RESTRICTED_HOST_PORTS; do
      ufw allow from "$sn" to any port "$p" proto tcp >/dev/null
    done
  done
  # Swarm control plane: allow configured peers FIRST (ufw matches top-down, so
  # peer-specific allows must precede the catch-all denies or the deny wins and
  # node-to-node swarm traffic is blocked).
  if [ -n "$SWARM_PEERS" ]; then
    for peer in $(ip_to_list "$SWARM_PEERS"); do
      ufw allow from "$peer" to any port 2377 proto tcp >/dev/null # control plane
      ufw allow from "$peer" to any port 7946 proto tcp >/dev/null # gossip
      ufw allow from "$peer" to any port 7946 proto udp >/dev/null # gossip (udp)
      ufw allow from "$peer" to any port 4789 proto udp >/dev/null # overlay VXLAN
    done
  fi
  # Then deny everything else for these ports (still reachable from the docker
  # subnets + peers above, blocked from the WAN). Numbers 9090/4318/2377/7946.
  # Swarm control plane + VXLAN. MUST be added BEFORE the deny block below:
  # ufw is first-match-wins, so a blanket deny appended first shadows every
  # later allow — the rules look right in `ufw status` and block anyway.
  for pn in $(ip_to_list "$SWARM_PEER_SOURCES"); do
    ufw allow from "$pn" to any port 2377 proto tcp >/dev/null
    ufw allow from "$pn" to any port 7946 proto tcp >/dev/null
    ufw allow from "$pn" to any port 7946 proto udp >/dev/null
    ufw allow from "$pn" to any port 4789 proto udp >/dev/null
  done

  ufw deny 9090/tcp  >/dev/null   # pmcluster API       — no WAN
  ufw deny 4318/tcp  >/dev/null   # OTLP (host path)    — no WAN
  ufw deny 8000/tcp  >/dev/null   # Parseable API       — no WAN (belt; the DOCKER-USER rule is the real gate)
  ufw deny 2377/tcp  >/dev/null   # Swarm manager       — no WAN
  ufw deny 7946/tcp  >/dev/null   # Swarm gossip        — no WAN
  ufw deny 7946/udp  >/dev/null   # Swarm gossip (udp)  — no WAN
  ufw deny 4789/udp  >/dev/null   # Overlay VXLAN       — no WAN

  info "Enabling ufw"
  echo y | ufw --force enable >/dev/null
  ufw status verbose | head -30
}

# ─────────────── 2) DOCKER-USER rules (published ports) ───────────
#
# The rules are written to a small script and a unit that re-runs it after
# docker starts, because the chain does not exist before Docker and iptables
# rules do not survive a reboot. Without that, every restart silently reopens
# these ports — a firewall you think you have is worse than none.
DOCKER_USER_SCRIPT=/usr/local/sbin/pmcluster-docker-user.sh
DOCKER_USER_UNIT=/etc/systemd/system/pmcluster-docker-user.service

write_docker_user_script() {
  {
    echo '#!/bin/sh'
    echo '# Generated by harden-host.sh — do not edit; re-run the script instead.'
    echo 'set -eu'
    echo '# Docker removed or not up yet: nothing to protect, and no reason to fail.'
    echo 'iptables -n -L DOCKER-USER >/dev/null 2>&1 || exit 0'
    echo 'iptables -F DOCKER-USER'
    for sn in $(ip_to_list "$DOCKER_SUBNETS"); do
      echo "iptables -A DOCKER-USER -s '$sn' -j RETURN"
    done
    # Every PRIVATE source returns before the drops: loopback (the daemon reads
    # its own published port), the overlay/bridge pools, and the cluster's private
    # peer network (an ingress-published port is served by whichever node hosts
    # the task, so a peer's request arrives over VXLAN carrying the peer address).
    for pn in $(ip_to_list "$PRIVATE_SUBNETS"); do
      echo "iptables -A DOCKER-USER -s '$pn' -j RETURN"
    done
    for p in $RESTRICTED_DOCKER_PORTS; do
      # NOTE: --dport matches the CONTAINER port here, not the published one.
      # That is fine while a port is published to the same number (4318, 8000);
      # a remapped publish (host 9000 -> container 8000) would need
      # `-m conntrack --ctstate DNAT --ctorigdstport 9000` instead.
      echo "iptables -A DOCKER-USER -p tcp --dport '$p' -j DROP"
    done
    echo 'iptables -A DOCKER-USER -j RETURN'
  } > "$DOCKER_USER_SCRIPT"
  chmod 0755 "$DOCKER_USER_SCRIPT"
}

apply_docker_user_rules() {
  # DOCKER-USER is the sanctioned iptables chain for filtering packets destined
  # to docker-published ports. It does NOT get in the way of swarm ingress
  # (80/443 go via DOCKER-INGRESS) as long as we RETURN the rest.
  iptables -n -L DOCKER-USER >/dev/null 2>&1 \
    || die "DOCKER-USER chain not found — is Docker running?"

  info "Programming DOCKER-USER rules (close ${RESTRICTED_DOCKER_PORTS// /, } to the WAN)"
  write_docker_user_script
  "$DOCKER_USER_SCRIPT"
  echo "--- DOCKER-USER chain ---"
  iptables -n -L DOCKER-USER
}

install_docker_user_unit() {
  info "Installing $DOCKER_USER_UNIT (re-applies the rules after every boot)"
  cat > "$DOCKER_USER_UNIT" <<UNIT
[Unit]
Description=pmcluster — keep published-but-private ports off the WAN
Documentation=file:///etc/pmcluster/README.hardening
After=docker.service
Wants=docker.service

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=$DOCKER_USER_SCRIPT
ExecReload=$DOCKER_USER_SCRIPT

[Install]
WantedBy=multi-user.target
UNIT
  systemctl daemon-reload
  systemctl enable --now pmcluster-docker-user.service >/dev/null
  systemctl is-enabled pmcluster-docker-user.service
}

# ────────────────────────── 3) fail2ban ────────────────────────────
apply_fail2ban() {
  info "Installing + starting fail2ban"
  apt-get install -y fail2ban >/dev/null

  # NOTE: no inline comments in these values — fail2ban 1.1.0 mishandles them
  # (findtime ends up "600  # over the last 10 min" → jail fails to start).
  cat > /etc/fail2ban/jail.local <<'EOF'
[DEFAULT]
bantime  = 3600
findtime = 600
maxretry = 5

[sshd]
enabled = true
port    = ssh
logpath = %(sshd_log)s
EOF

  systemctl enable --now fail2ban
  systemctl restart fail2ban
  systemctl is-active fail2ban
  echo "--- sshd jail status ---"
  fail2ban-client status sshd 2>/dev/null || true
}

# ─────────────────────── 4) Security updates ───────────────────────
apply_updates() {
  info "Applying pending updates (this upgrades Docker/Swarm runtime too)"
  export DEBIAN_FRONTEND=noninteractive
  apt-get update >/dev/null
  apt-get upgrade -y
}

# ───────────── SSH hardening (manual — step 1, SKIPPED) ────────────
# The box has no SSH keys at all, so disabling password auth now would lock
# you out. Do this LATER, once a key is in /root/.ssh/authorized_keys:
#
#   install -d -m700 /root/.ssh
#   echo "<YOUR_PUBLIC_KEY>" > /root/.ssh/authorized_keys  && chmod 600 /root/.ssh/authorized_keys
#   rm -f /etc/ssh/sshd_config.d/50-cloud-init.conf          # removes 'PasswordAuthentication yes'
#   sed -i 's/^PermitRootLogin .*/PermitRootLogin prohibit-password/' /etc/ssh/sshd_config
#   systemctl reload ssh
#

# ───────────────────────────── main ────────────────────────────────
main() {
  # --no-updates skips the apt upgrade. On a live cluster that step can upgrade
  # the Docker engine itself, which restarts the daemon on a node that is
  # currently running workloads — a decision for the operator, not a side effect
  # of closing a port. Everything else is safe to re-run at any time.
  local with_updates=1
  for arg in "$@"; do
    case "$arg" in
      --no-updates) with_updates=0 ;;
      -h|--help) sed -n '2,30p' "$0"; exit 0 ;;
      *) die "unknown argument: $arg (use --no-updates)" ;;
    esac
  done

  require_root
  apply_firewall
  apply_docker_user_rules
  install_docker_user_unit
  apply_fail2ban
  if [ "$with_updates" -eq 1 ]; then
    apply_updates
  else
    info "skipping apt upgrade (--no-updates): Docker/Swarm runtime left as-is"
  fi
  cat <<'SUMMARY'

============================== SUMMARY =============================
✔ ufw firewall enabled (default-deny incoming; open: 22 80 443)
✔ DOCKER-USER rules close OTLP 4318 + Parseable 8000 (and any published port
  in $RESTRICTED_DOCKER_PORTS) to the WAN; docker subnets and loopback still
  allowed, so the daemon keeps reading its own backend
✔ pmcluster-docker-user.service re-applies those rules after every boot
✔ pmcluster API 9090 + swarm control plane 2377/7946 blocked from WAN
✔ fail2ban active (sshd jail: 5 fails / 10 min → 1h ban)
✔ security updates applied (unless --no-updates was given)
✔ SSH password auth LEFT AS-IS (no keys installed) — add a key first,
  then follow the manual step-1 instructions above to close it.
============================== END =================================
SUMMARY
}

main "$@"
