#!/usr/bin/env bash
#
# harden-host.sh — one-shot host hardening for an Ubuntu server running
# pmcluster (+ its Docker Swarm stack).
#
# Applies, in order:
#   1. ufw host firewall (default-deny incoming; open 22/80/443 only)
#   2. iptables DOCKER-USER rules — close the "bypasses Traefik" ports
#      (OTLP 4318) and the swarm control plane (2377/7946) to the WAN while
#      letting docker/overlay subnets keep talking
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
DOCKER_SUBNETS="172.16.0.0/12"

# WAN-facing ports to keep publicly open.
WAN_PORTS="22 80 443"

# Docker-PUBLISHED ports (reachable via docker-proxy) that must NOT be open to
# the internet. 4318 = OTel collector OTLP (the obvious Traefik bypass).
# Add any other published-but-private ports here as you expose them.
RESTRICTED_DOCKER_PORTS="4318"

# Host-bound control-plane ports that must not be WAN-reachable:
#   9090 = pmcluster management API (bound on all interfaces, Traefik reaches
#          it over the docker gateway so we only block the public side)
#   2377 = Docker Swarm manager/join  7946 = Swarm gossip
RESTRICTED_HOST_PORTS="9090 2377 7946"

# Comma-separated peer IPs allowed to reach the swarm control plane
# (add manager/worker nodes in a multi-node swarm; single-node → leave empty).
SWARM_PEERS=

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
  # Swarm control plane: allow configured peers, deny everything else.
  ufw deny 9090/tcp  >/dev/null   # pmcluster API       — no WAN
  ufw deny 4318/tcp  >/dev/null   # OTLP (host path)    — no WAN
  ufw deny 2377/tcp  >/dev/null   # Swarm manager       — no WAN
  ufw deny 7946/tcp  >/dev/null   # Swarm gossip        — no WAN
  if [ -n "$SWARM_PEERS" ]; then
    for peer in $(ip_to_list "$SWARM_PEERS"); do
      ufw allow from "$peer" to any port 2377,7946 proto tcp >/dev/null
    done
  fi

  info "Enabling ufw"
  echo y | ufw --force enable >/dev/null
  ufw status verbose | head -30
}

# ─────────────── 2) DOCKER-USER rules (published ports) ───────────
apply_docker_user_rules() {
  # DOCKER-USER is the sanctioned iptables chain for filtering packets destined
  # to docker-published ports. It does NOT get in the way of swarm ingress
  # (80/443 go via DOCKER-INGRESS) as long as we RETURN the rest.
  iptables -n -L DOCKER-USER >/dev/null 2>&1 \
    || die "DOCKER-USER chain not found — is Docker running?"

  info "Programming DOCKER-USER rules (close OTLP 4318 to the WAN)"
  iptables -F DOCKER-USER
  for sn in $(ip_to_list "$DOCKER_SUBNETS"); do
    iptables -A DOCKER-USER -s "$sn" -j RETURN
  done
  for p in $RESTRICTED_DOCKER_PORTS; do
    iptables -A DOCKER-USER -p tcp --dport "$p" -j DROP
  done
  iptables -A DOCKER-USER -j RETURN
  echo "--- DOCKER-USER chain ---"
  iptables -n -L DOCKER-USER
}

# ────────────────────────── 3) fail2ban ────────────────────────────
apply_fail2ban() {
  info "Installing + starting fail2ban"
  apt-get install -y fail2ban >/dev/null

  cat > /etc/fail2ban/jail.local <<'EOF'
[DEFAULT]
bantime  = 3600          # ban for 1h
findtime = 600           # over the last 10 min
maxretry = 5             # after 5 failures

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
  require_root
  apply_firewall
  apply_docker_user_rules
  apply_fail2ban
  apply_updates
  cat <<'SUMMARY'

============================== SUMMARY =============================
✔ ufw firewall enabled (default-deny incoming; open: 22 80 443)
✔ DOCKER-USER rules close OTLP 4318 (and any published port in
  $RESTRICTED_DOCKER_PORTS) to the WAN; docker subnets still allowed
✔ pmcluster API 9090 + swarm control plane 2377/7946 blocked from WAN
✔ fail2ban active (sshd jail: 5 fails / 10 min → 1h ban)
✔ security updates applied
✔ SSH password auth LEFT AS-IS (no keys installed) — add a key first,
  then follow the manual step-1 instructions above to close it.
============================== END =================================
SUMMARY
}

main "$@"
