#!/usr/bin/env bash
# install.sh — fetch the latest pmcluster release binary and drop it in
# /usr/local/bin (override with PREFIX=/path/to/dir).
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/hazemarian/poor-man-cluster/main/install.sh | bash
#   curl -fsSL https://raw.githubusercontent.com/hazemarian/poor-man-cluster/main/install.sh | VERSION=v0.2.0 bash
#
# Optional env vars:
#   VERSION=v0.2.0          pin a specific release (default: latest)
#   PREFIX=/opt/bin         install location (default: /usr/local/bin)
#   PMCLUSTER_USER=deployer systemd service user (default: auto-detected)
#   PMCLUSTER_REGISTRY=host=user=token,...  auto-configure registries
#     (GHCR: PMCLUSTER_REGISTRY=ghcr.io=USERNAME=ghp_...)
#
# Cluster apply (auto-cluster up/update):
#   CLUSTER_APPLY=auto|none   auto=up fresh / update existing (default auto)
#   PMCLUSTER_DOMAIN=<domain>            required to auto 'cluster up' on a fresh box
#   PMCLUSTER_CERT / PMCLUSTER_KEY       PEM file paths (BYO cert mode)
#   PMCLUSTER_ACME_EMAIL=<you@host>      alt: Let's Encrypt mode
#   PMCLUSTER_OPENOBSERVE_EMAIL=<you@host>
#
# On a fresh box with no cluster, install.sh runs 'pmcluster cluster up' using
# the PMCLUSTER_* inputs; on a box where the 'infra' stack is already deployed
# it runs 'pmcluster cluster update' to apply config changes. Set
# CLUSTER_APPLY=none to skip both.

set -euo pipefail

REPO="hazemarian/poor-man-cluster"
PREFIX="${PREFIX:-/usr/local/bin}"
VERSION="${VERSION:-latest}"

# Detect OS and arch in the same shape the release workflow uses.
case "$(uname -s)" in
  Darwin) OS=darwin ;;
  Linux)  OS=linux ;;
  *) echo "unsupported OS: $(uname -s)" >&2; exit 1 ;;
esac

case "$(uname -m)" in
  arm64|aarch64) ARCH=arm64 ;;
  x86_64|amd64)  ARCH=amd64 ;;
  *) echo "unsupported arch: $(uname -m)" >&2; exit 1 ;;
esac

if [ "$VERSION" = "latest" ]; then
  echo "→ Resolving latest release from github.com/${REPO}"
  API_RESPONSE=$(curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest")
  if [ -z "$API_RESPONSE" ]; then
    echo "could not reach GitHub API (rate-limited or network error). Try:" >&2
    echo "  curl -fsSL .../install.sh | VERSION=v0.0.1 bash" >&2
    exit 1
  fi
  VERSION=$(echo "$API_RESPONSE" \
    | grep -m1 '"tag_name"' \
    | sed -E 's/.*"tag_name"[[:space:]]*:[[:space:]]*"([^"]+)".*/\1/')
  if [ -z "$VERSION" ]; then
    echo "could not resolve latest release tag" >&2
    exit 1
  fi
fi

ARCHIVE="pmcluster-${VERSION}-${OS}-${ARCH}.tar.gz"
URL="https://github.com/${REPO}/releases/download/${VERSION}/${ARCHIVE}"

echo "→ Downloading ${URL}"
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT
curl -fsSL "$URL" -o "$TMP/$ARCHIVE"

# Verify checksum. The release workflow ships a SHA256SUMS.txt covering
# every archive; we grab the line for our archive and feed it to shasum.
echo "→ Verifying checksum"
curl -fsSL "https://github.com/${REPO}/releases/download/${VERSION}/SHA256SUMS.txt" \
  -o "$TMP/SHA256SUMS.txt"
(cd "$TMP" && grep " ${ARCHIVE}\$" SHA256SUMS.txt | shasum -a 256 -c -) \
  || { echo "checksum verification failed" >&2; exit 1; }

tar -xzf "$TMP/$ARCHIVE" -C "$TMP"
BIN="$TMP/pmcluster-${VERSION}-${OS}-${ARCH}"

mkdir -p "$PREFIX"
if [ -w "$PREFIX" ]; then
  install -m 0755 "$BIN" "$PREFIX/pmcluster"
else
  echo "→ ${PREFIX} requires sudo"
  sudo install -m 0755 "$BIN" "$PREFIX/pmcluster"
fi

echo
echo "✅ pmcluster ${VERSION} installed at ${PREFIX}/pmcluster"
# Ensure backup destination exists (bind mount in backup stack).
if [ ! -d /var/backups/docker-volumes ]; then
  mkdir -p /var/backups/docker-volumes 2>/dev/null || true
fi

echo
"$PREFIX/pmcluster" version || true
echo

# --- Auto-configure registries (if PMCLUSTER_REGISTRY is set) ---
configure_registries() {
  local spec="${1:-}"
  [ -z "$spec" ] && return

  # Split on comma: host=user=token,host2=user2=token2,...
  IFS=',' read -ra ENTRIES <<<"$spec"
  for entry in "${ENTRIES[@]}"; do
    # Trim whitespace.
    entry="$(echo "$entry" | xargs)"
    [ -z "$entry" ] && continue

    # Parse host, user, token (token is everything after the second =).
    host="${entry%%=*}"
    rest="${entry#*=}"
    user="${rest%%=*}"
    token="${rest#*=}"

    if [ -z "$host" ] || [ -z "$user" ] || [ -z "$token" ]; then
      echo "⚠  Skipping malformed registry entry: $entry (expect host=user=token)" >&2
      continue
    fi

    # docker login: bad credentials fail loud.
    echo "→ Logging in to $host as $user"
    if ! echo "$token" | docker login "$host" -u "$user" --password-stdin >/dev/null 2>&1; then
      echo "⚠  docker login $host failed — credentials may be wrong or expired" >&2
      continue
    fi

    # Persist encrypted in pmcluster so daemon replays on restart.
    if "$PREFIX/pmcluster" registry add "$host" --username "$user" --password-stdin <<<"$token" >/dev/null 2>&1; then
      echo "✅ Registry $host added (user: $user)"
    else
      echo "⚠  pmcluster registry add $host failed (docker login succeeded but encryption/persist failed)" >&2
    fi
  done
}

if [ -n "${PMCLUSTER_REGISTRY:-}" ]; then
  echo "→ Configuring registries from PMCLUSTER_REGISTRY"
  configure_registries "$PMCLUSTER_REGISTRY"
  echo
fi

# --- Daemon management (moved to the CLI) ---
# The pmcluster daemon is now managed by the CLI itself — install.sh only
# installs the binary. The systemd unit is created + started by:
#   pmcluster cluster up / cluster update   (on a manager)
#   pmcluster join --token <t> --manager <m>  (joining a swarm node)
# and can always be run in the foreground with `pmcluster serve`.
echo "→ pmcluster binary installed. The daemon is started by the CLI itself:"
echo "  pmcluster cluster up/update   (manager), pmcluster join (node), or pmcluster serve (foreground)"
echo
echo "Next (if not already done):"
echo "  pmcluster init                # create ~/.pmcluster + bootstrap user"
echo "  pmcluster cluster up --domain=<your-domain> --cert=<cert> --key=<key> --openobserve-email=<you@host>"
echo "  pmcluster join --token=<token> --manager=<manager>:2377   # join a swarm node"
echo
echo "Manage the daemon:"
echo "  systemctl status pmcluster    # check status"
echo "  systemctl restart pmcluster   # restart after cluster changes"
echo "  journalctl -u pmcluster -f    # follow logs"
echo "  pmcluster serve               # foreground (non-Linux or no systemd)"

if [ -n "${PMCLUSTER_REGISTRY:-}" ]; then
  echo
  echo "Registries were auto-configured. Verify:"
  echo "  pmcluster registry list"
fi

# --- Cluster apply (auto cluster up / update) ---
# Decides to bring the cluster up on a fresh box, or update it when infra is
# already deployed. Skipped entirely when CLUSTER_APPLY=none.
#
#   CLUSTER_APPLY=auto|none   (default auto)
#   PMCLUSTER_DOMAIN=<domain>            required to auto 'cluster up' on a fresh box
#   PMCLUSTER_CERT / PMCLUSTER_KEY       PEM file paths (BYO cert mode)
#   PMCLUSTER_ACME_EMAIL=<you@host>      alt: Let's Encrypt mode
#   PMCLUSTER_OPENOBSERVE_EMAIL=<you@host>
CLUSTER_APPLY="${CLUSTER_APPLY:-auto}"
PMCLUSTER_USER="${PMCLUSTER_USER:-${SUDO_USER:-$(id -un)}}"
PMCLUSTER_HOME="$(eval echo ~${PMCLUSTER_USER})"

run_cluster_apply() {
  [ "$CLUSTER_APPLY" = "none" ] && { echo "→ CLUSTER_APPLY=none — skipping cluster up/update"; return 0; }

  local CONFIG_FILE="${PMCLUSTER_HOME}/.pmcluster/config.yaml"

  if [ -f "$CONFIG_FILE" ]; then
    echo
    echo "→ Cluster already exists at $CONFIG_FILE — running 'cluster update'"
    if ! "$PREFIX/pmcluster" cluster update; then
      echo "⚠  pmcluster cluster update failed (see output above)." >&2
      return 1
    fi
    echo "→ cluster update complete."
    return 0
  fi

  # Fresh box: need a domain to bring the cluster up.
  if [ -z "${PMCLUSTER_DOMAIN:-}" ]; then
    echo "→ No existing cluster and PMCLUSTER_DOMAIN is empty — skipping auto 'cluster up'."
    echo "  Set PMCLUSTER_DOMAIN=<domain> to auto-provision, or run 'pmcluster init' + 'pmcluster cluster up' yourself."
    return 0
  fi

  echo
  echo "→ No existing cluster — bootstrapping with 'pmcluster init' + 'cluster up'"
  if ! "$PREFIX/pmcluster" init; then
    echo "⚠  pmcluster init failed (see output above)." >&2
    return 1
  fi

  local UP_ARGS=(--domain="$PMCLUSTER_DOMAIN")
  if [ -n "${PMCLUSTER_CERT:-}" ] && [ -n "${PMCLUSTER_KEY:-}" ]; then
    UP_ARGS+=(--cert="$PMCLUSTER_CERT" --key="$PMCLUSTER_KEY")
  elif [ -n "${PMCLUSTER_ACME_EMAIL:-}" ]; then
    UP_ARGS+=(--acme-email="$PMCLUSTER_ACME_EMAIL")
  fi
  if [ -n "${PMCLUSTER_OPENOBSERVE_EMAIL:-}" ]; then
    UP_ARGS+=(--openobserve-email="$PMCLUSTER_OPENOBSERVE_EMAIL")
  fi

  if ! "$PREFIX/pmcluster" cluster up "${UP_ARGS[@]}"; then
    echo "⚠  pmcluster cluster up failed (see output above)." >&2
    return 1
  fi
  echo "→ cluster up complete."
  return 0
}

run_cluster_apply || true
