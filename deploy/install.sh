#!/usr/bin/env sh
# AetherixCloud node installer.
#
# Installs Docker (if missing), aetherixd + ax, Corrosion, Caddy (Aetherix
# build when available), BuildKit + Railpack (Dockerfile-less GitHub builds),
# the systemd units, then optionally bootstraps or joins a cluster.
#
# Usage (matches CreateJoinToken's InstallCommand):
#   curl -fsSL https://raw.githubusercontent.com/Aetherix-code/AetherixCloud/main/deploy/install.sh | sudo sh -s -- --init
#   curl -fsSL https://raw.githubusercontent.com/Aetherix-code/AetherixCloud/main/deploy/install.sh | sudo sh -s -- --join <token>
#
# Flags:
#   --init                 create a new cluster on this machine
#   --join TOKEN           join an existing cluster (token from ax machine add)
#   --from-backup FILE     with --init: restore from a .snapshot on first start (DR)
#   --name NAME            machine name (default: hostname / node-1)
#   --cluster-name NAME    cluster name for --init (default: aetherix)
#   --advertise HOST       public IP/hostname (repeatable; default: auto-detect)
#   --domain DOMAIN        default cluster domain for --init
#   --version VER          release tag or "latest" (default: latest)
#   --channel CHANNEL      release channel alias (stable|latest); default latest
#   --no-start             install only; do not enable/start systemd
#   --skip-docker          do not install or reconfigure Docker
#   --skip-corrosion       do not install Corrosion (daemon falls back to sqlite)
#   --skip-caddy           do not install Caddy (ingress disabled on this node)
#   --skip-builder         do not install BuildKit / Railpack (no auto-builds)
#   --bin-dir DIR          install binaries here (default: /usr/local/bin)
#   --data-dir DIR         aetherix data dir (default: /var/lib/aetherix)
#   --help
#
# Environment overrides:
#   AETHERIX_REPO            GitHub owner/repo (default: Aetherix-code/AetherixCloud)
#   AETHERIX_VERSION         same as --version
#   AETHERIX_DOWNLOAD_BASE   base URL for release assets (overrides GitHub)
#   CORROSION_VERSION        Corrosion tag (default: v1.0.0)
#   BUILDKIT_VERSION         BuildKit tag without v (default: 0.35.1)
#   RAILPACK_VERSION         Railpack tag without v (default: 0.43.1)
#   AETHERIX_CADDY_URL       direct URL to a caddy binary or .tar.gz
#
# Must be runnable under plain `sh` (the curl | sh path ignores the shebang).
# shellcheck shell=sh

set -eu

REPO="${AETHERIX_REPO:-Aetherix-code/AetherixCloud}"
VERSION="${AETHERIX_VERSION:-latest}"
CHANNEL="latest"
CORROSION_VERSION="${CORROSION_VERSION:-v1.0.0}"
BUILDKIT_VERSION="${BUILDKIT_VERSION:-0.35.1}"
RAILPACK_VERSION="${RAILPACK_VERSION:-0.43.1}"
BIN_DIR="/usr/local/bin"
DATA_DIR="/var/lib/aetherix"
UNIT_DIR="/etc/systemd/system"
DOCKER_DAEMON_JSON="/etc/docker/daemon.json"
BUILDKIT_CONFIG_DIR="/etc/buildkit"

DO_INIT=0
DO_JOIN=0
JOIN_TOKEN=""
FROM_BACKUP=""
MACHINE_NAME=""
CLUSTER_NAME="aetherix"
DOMAIN=""
NO_START=0
SKIP_DOCKER=0
SKIP_CORROSION=0
SKIP_CADDY=0
SKIP_BUILDER=0
ADVERTISE=""

usage() {
  sed -n '2,40p' "$0" | sed 's/^# \{0,1\}//'
}

log() { printf '%s\n' "$*"; }
err() { printf 'error: %s\n' "$*" >&2; exit 1; }
need_cmd() { command -v "$1" >/dev/null 2>&1 || err "required command not found: $1"; }

# --- args --------------------------------------------------------------------

while [ "$#" -gt 0 ]; do
  case "$1" in
    --init) DO_INIT=1 ;;
    --join)
      DO_JOIN=1
      [ "$#" -ge 2 ] || err "--join requires a token"
      JOIN_TOKEN="$2"
      shift
      ;;
    --join=*)
      DO_JOIN=1
      JOIN_TOKEN="${1#--join=}"
      ;;
    --from-backup)
      [ "$#" -ge 2 ] || err "--from-backup requires a file path"
      FROM_BACKUP="$2"
      shift
      ;;
    --from-backup=*) FROM_BACKUP="${1#--from-backup=}" ;;
    --name)
      [ "$#" -ge 2 ] || err "--name requires a value"
      MACHINE_NAME="$2"
      shift
      ;;
    --name=*) MACHINE_NAME="${1#--name=}" ;;
    --cluster-name)
      [ "$#" -ge 2 ] || err "--cluster-name requires a value"
      CLUSTER_NAME="$2"
      shift
      ;;
    --cluster-name=*) CLUSTER_NAME="${1#--cluster-name=}" ;;
    --advertise)
      [ "$#" -ge 2 ] || err "--advertise requires a value"
      if [ -n "$ADVERTISE" ]; then ADVERTISE="$ADVERTISE $2"; else ADVERTISE="$2"; fi
      shift
      ;;
    --advertise=*)
      a="${1#--advertise=}"
      if [ -n "$ADVERTISE" ]; then ADVERTISE="$ADVERTISE $a"; else ADVERTISE="$a"; fi
      ;;
    --domain)
      [ "$#" -ge 2 ] || err "--domain requires a value"
      DOMAIN="$2"
      shift
      ;;
    --domain=*) DOMAIN="${1#--domain=}" ;;
    --version)
      [ "$#" -ge 2 ] || err "--version requires a value"
      VERSION="$2"
      shift
      ;;
    --version=*) VERSION="${1#--version=}" ;;
    --channel)
      [ "$#" -ge 2 ] || err "--channel requires a value"
      CHANNEL="$2"
      shift
      ;;
    --channel=*) CHANNEL="${1#--channel=}" ;;
    --no-start) NO_START=1 ;;
    --skip-docker) SKIP_DOCKER=1 ;;
    --skip-corrosion) SKIP_CORROSION=1 ;;
    --skip-caddy) SKIP_CADDY=1 ;;
    --skip-builder) SKIP_BUILDER=1 ;;
    --bin-dir)
      [ "$#" -ge 2 ] || err "--bin-dir requires a value"
      BIN_DIR="$2"
      shift
      ;;
    --bin-dir=*) BIN_DIR="${1#--bin-dir=}" ;;
    --data-dir)
      [ "$#" -ge 2 ] || err "--data-dir requires a value"
      DATA_DIR="$2"
      shift
      ;;
    --data-dir=*) DATA_DIR="${1#--data-dir=}" ;;
    -h|--help) usage; exit 0 ;;
    *) err "unknown argument: $1 (try --help)" ;;
  esac
  shift
done

if [ "$DO_INIT" -eq 1 ] && [ "$DO_JOIN" -eq 1 ]; then
  err "use either --init or --join, not both"
fi
if [ -n "$FROM_BACKUP" ] && [ "$DO_INIT" -eq 0 ]; then
  err "--from-backup requires --init"
fi

# --- platform ----------------------------------------------------------------

[ "$(id -u)" -eq 0 ] || err "run as root (sudo)"

OS="$(uname -s | tr '[:upper:]' '[:lower:]')"
ARCH_RAW="$(uname -m)"
case "$ARCH_RAW" in
  x86_64|amd64) ARCH="amd64"; CORRO_ARCH="x86_64"; CADDY_ARCH="amd64"; RP_ARCH="x86_64" ;;
  aarch64|arm64) ARCH="arm64"; CORRO_ARCH="aarch64"; CADDY_ARCH="arm64"; RP_ARCH="arm64" ;;
  *) err "unsupported architecture: $ARCH_RAW (need amd64 or arm64)" ;;
esac
[ "$OS" = "linux" ] || err "install.sh targets Linux VPS hosts (got $OS); use 'ax dev' on a laptop"

need_cmd curl
need_cmd tar
need_cmd uname

TMPDIR="${TMPDIR:-/tmp}"
WORK="$(mktemp -d "$TMPDIR/aetherix-install.XXXXXX")"
trap 'rm -rf "$WORK"' EXIT INT TERM

# --- helpers -----------------------------------------------------------------

download() {
  # download URL DEST
  curl -fsSL --retry 3 --retry-delay 2 -o "$2" "$1"
}

github_latest_tag() {
  # Prints the latest release tag for $REPO (falls back to empty).
  curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest" 2>/dev/null \
    | sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' \
    | head -n 1
}

resolve_version() {
  if [ "$VERSION" = "latest" ] || [ "$CHANNEL" = "stable" ]; then
    v="$(github_latest_tag || true)"
    if [ -z "$v" ]; then
      err "could not resolve latest release for ${REPO}; pass --version vX.Y.Z or set AETHERIX_DOWNLOAD_BASE"
    fi
    VERSION="$v"
  fi
  log "using AetherixCloud ${VERSION}"
}

asset_url() {
  # asset_url NAME -> prints download URL
  name="$1"
  if [ -n "${AETHERIX_DOWNLOAD_BASE:-}" ]; then
    printf '%s/%s\n' "${AETHERIX_DOWNLOAD_BASE%/}" "$name"
  else
    printf 'https://github.com/%s/releases/download/%s/%s\n' "$REPO" "$VERSION" "$name"
  fi
}

detect_advertise() {
  if [ -n "$ADVERTISE" ]; then
    return 0
  fi
  # Best-effort public IPv4: AWS IMDSv2, then outbound discovery, then primary iface.
  ip=""
  tok="$(curl -fsS --connect-timeout 1 -H 'X-aws-ec2-metadata-token-ttl-seconds: 60' \
    -X PUT 'http://169.254.169.254/latest/api/token' 2>/dev/null || true)"
  if [ -n "$tok" ]; then
    ip="$(curl -fsS --connect-timeout 1 -H "X-aws-ec2-metadata-token: $tok" \
      http://169.254.169.254/latest/meta-data/public-ipv4 2>/dev/null || true)"
  fi
  if [ -z "$ip" ]; then
    ip="$(curl -fsS --connect-timeout 2 https://api.ipify.org 2>/dev/null || true)"
  fi
  if [ -z "$ip" ]; then
    ip="$(hostname -I 2>/dev/null | awk '{print $1}')"
  fi
  [ -n "$ip" ] || err "could not detect a public address; pass --advertise <ip-or-hostname>"
  ADVERTISE="$ip"
  log "advertise: $ADVERTISE"
}

install_docker() {
  if [ "$SKIP_DOCKER" -eq 1 ]; then
    log "skipping Docker install (--skip-docker)"
    return 0
  fi
  if command -v docker >/dev/null 2>&1 && docker info >/dev/null 2>&1; then
    log "Docker already running"
  else
    log "installing Docker..."
    need_cmd curl
    curl -fsSL https://get.docker.com | sh
    systemctl enable --now docker >/dev/null 2>&1 || service docker start || true
    docker info >/dev/null 2>&1 || err "Docker installed but not usable; check the docker service"
  fi

  # Merge insecure-registries required by the embedded peer registry
  # (10.210.0.0/16 mesh + 127.0.0.1:5000). Preserve any existing keys.
  mkdir -p /etc/docker
  if [ -f "$DOCKER_DAEMON_JSON" ]; then
    if grep -q '10.210.0.0/16' "$DOCKER_DAEMON_JSON" 2>/dev/null; then
      log "Docker daemon.json already has mesh insecure-registries"
      return 0
    fi
    log "warning: $DOCKER_DAEMON_JSON exists without 10.210.0.0/16; adding aetherix registries via a drop-in is recommended"
  fi
  if [ ! -f "$DOCKER_DAEMON_JSON" ]; then
    cat >"$DOCKER_DAEMON_JSON" <<'EOF'
{
  "insecure-registries": ["10.210.0.0/16", "127.0.0.0/8"],
  "log-driver": "json-file",
  "log-opts": { "max-size": "10m", "max-file": "3" }
}
EOF
    systemctl restart docker >/dev/null 2>&1 || true
  fi
}

install_pkgs() {
  # WireGuard userspace tools (kernel module expected on modern VPS kernels).
  if command -v apt-get >/dev/null 2>&1; then
    export DEBIAN_FRONTEND=noninteractive
    apt-get update -qq
    apt-get install -y -qq ca-certificates curl git iptables wireguard-tools >/dev/null
  elif command -v dnf >/dev/null 2>&1; then
    dnf install -y ca-certificates curl git iptables wireguard-tools >/dev/null
  elif command -v yum >/dev/null 2>&1; then
    yum install -y ca-certificates curl git iptables wireguard-tools >/dev/null
  elif command -v apk >/dev/null 2>&1; then
    apk add --no-cache ca-certificates curl git iptables wireguard-tools >/dev/null
  else
    log "warning: unknown package manager; ensure curl, git, iptables and wireguard-tools are installed"
  fi
}

install_aetherix_bins() {
  resolve_version
  mkdir -p "$BIN_DIR" "$DATA_DIR"

  # Preferred archive names from .goreleaser.yaml (linux_${ARCH}).
  archive="aetherixcloud_${VERSION#v}_linux_${ARCH}.tar.gz"
  url="$(asset_url "$archive")"
  log "downloading $url"
  if ! download "$url" "$WORK/ax.tgz" 2>/dev/null; then
    # Fallback: goreleaser default without stripping the leading v.
    archive="aetherixcloud_${VERSION}_linux_${ARCH}.tar.gz"
    url="$(asset_url "$archive")"
    log "retrying $url"
    download "$url" "$WORK/ax.tgz" || err "failed to download release archive; build locally with 'make build' or set AETHERIX_DOWNLOAD_BASE"
  fi
  mkdir -p "$WORK/extract"
  tar -xzf "$WORK/ax.tgz" -C "$WORK/extract"
  # Archives may nest binaries at top level or under a directory.
  aetherixd_bin="$(find "$WORK/extract" -type f -name aetherixd | head -n 1)"
  ax_bin="$(find "$WORK/extract" -type f -name ax | head -n 1)"
  [ -n "$aetherixd_bin" ] && [ -n "$ax_bin" ] || err "archive missing aetherixd/ax binaries"
  install -m 0755 "$aetherixd_bin" "$BIN_DIR/aetherixd"
  install -m 0755 "$ax_bin" "$BIN_DIR/ax"
  log "installed $BIN_DIR/aetherixd and $BIN_DIR/ax"
}

install_corrosion() {
  if [ "$SKIP_CORROSION" -eq 1 ]; then
    log "skipping Corrosion (--skip-corrosion); aetherixd will use the sqlite store"
    return 0
  fi
  if command -v corrosion >/dev/null 2>&1; then
    log "corrosion already on PATH"
    return 0
  fi
  tarname="corrosion-${CORRO_ARCH}-unknown-linux-gnu.tar.gz"
  url="https://github.com/superfly/corrosion/releases/download/${CORROSION_VERSION}/${tarname}"
  log "downloading Corrosion ${CORROSION_VERSION} ($url)"
  if ! download "$url" "$WORK/corrosion.tgz" 2>/dev/null; then
    log "warning: could not download Corrosion; aetherixd will fall back to sqlite"
    return 0
  fi
  mkdir -p "$WORK/corrosion"
  tar -xzf "$WORK/corrosion.tgz" -C "$WORK/corrosion"
  cbin="$(find "$WORK/corrosion" -type f -name corrosion | head -n 1)"
  [ -n "$cbin" ] || { log "warning: Corrosion archive had no binary"; return 0; }
  install -m 0755 "$cbin" "$BIN_DIR/corrosion"
  log "installed $BIN_DIR/corrosion"
}

install_caddy() {
  if [ "$SKIP_CADDY" -eq 1 ]; then
    log "skipping Caddy (--skip-caddy); ingress disabled unless AX_CADDY_BIN is set"
    return 0
  fi
  if [ -n "${AETHERIX_CADDY_URL:-}" ]; then
    log "downloading Caddy from AETHERIX_CADDY_URL"
    download "$AETHERIX_CADDY_URL" "$WORK/caddy.blob"
    case "$AETHERIX_CADDY_URL" in
      *.tar.gz|*.tgz)
        mkdir -p "$WORK/caddy"
        tar -xzf "$WORK/caddy.blob" -C "$WORK/caddy"
        cbin="$(find "$WORK/caddy" -type f -name caddy | head -n 1)"
        ;;
      *) cbin="$WORK/caddy.blob" ;;
    esac
    [ -n "$cbin" ] || err "AETHERIX_CADDY_URL did not yield a caddy binary"
    install -m 0755 "$cbin" "$BIN_DIR/caddy"
    log "installed $BIN_DIR/caddy"
    return 0
  fi

  # Prefer the Aetherix-built Caddy (replicated-store module + Cloudflare DNS-01)
  # attached to the same GitHub release as aetherixd.
  for name in \
    "caddy_linux_${CADDY_ARCH}.tar.gz" \
    "aetherix-caddy_linux_${CADDY_ARCH}.tar.gz" \
    "caddy_${VERSION#v}_linux_${CADDY_ARCH}.tar.gz"
  do
    url="$(asset_url "$name")"
    if download "$url" "$WORK/caddy.tgz" 2>/dev/null; then
      mkdir -p "$WORK/caddy"
      tar -xzf "$WORK/caddy.tgz" -C "$WORK/caddy"
      cbin="$(find "$WORK/caddy" -type f -name caddy | head -n 1)"
      if [ -n "$cbin" ]; then
        install -m 0755 "$cbin" "$BIN_DIR/caddy"
        log "installed $BIN_DIR/caddy (from $name)"
        return 0
      fi
    fi
  done

  # Fallback: official Caddy (no aetherix storage module — certs stay node-local).
  log "warning: no Aetherix Caddy asset on the release; installing stock Caddy (shared ACME storage unavailable)"
  stock="https://github.com/caddyserver/caddy/releases/download/v2.8.4/caddy_2.8.4_linux_${CADDY_ARCH}.tar.gz"
  if download "$stock" "$WORK/caddy-stock.tgz" 2>/dev/null; then
    mkdir -p "$WORK/caddy-stock"
    tar -xzf "$WORK/caddy-stock.tgz" -C "$WORK/caddy-stock"
    cbin="$(find "$WORK/caddy-stock" -type f -name caddy | head -n 1)"
    if [ -n "$cbin" ]; then
      install -m 0755 "$cbin" "$BIN_DIR/caddy"
      log "installed stock $BIN_DIR/caddy"
      return 0
    fi
  fi
  log "warning: Caddy not installed; set AX_CADDY_BIN or rebuild with deploy/caddy/build.sh"
}

install_builder() {
  # BuildKit + Railpack power Dockerfile-less GitHub builds (auto-detect when
  # no Dockerfile / compose build: is present). See docs/github-app.md.
  if [ "$SKIP_BUILDER" -eq 1 ]; then
    log "skipping BuildKit/Railpack (--skip-builder); Dockerfile-less builds will fail"
    return 0
  fi
  need_cmd curl
  need_cmd sha256sum

  need_bk=1
  if command -v buildkitd >/dev/null 2>&1 && command -v buildctl >/dev/null 2>&1; then
    if buildkitd --version 2>/dev/null | grep -q "${BUILDKIT_VERSION}" \
      && buildctl --version 2>/dev/null | grep -q "${BUILDKIT_VERSION}"; then
      need_bk=0
      log "BuildKit v${BUILDKIT_VERSION} already installed"
    fi
  fi
  if [ "$need_bk" -eq 1 ]; then
    bk_url="https://github.com/moby/buildkit/releases/download/v${BUILDKIT_VERSION}/buildkit-v${BUILDKIT_VERSION}.linux-${ARCH}.tar.gz"
    log "downloading BuildKit v${BUILDKIT_VERSION} ($bk_url)"
    download "$bk_url" "$WORK/buildkit.tgz"
    mkdir -p "$WORK/buildkit"
    tar -xzf "$WORK/buildkit.tgz" -C "$WORK/buildkit"
    # Archive layout: bin/buildkitd bin/buildctl …
    find "$WORK/buildkit" -type f \( -name buildkitd -o -name buildctl \) -exec install -m 0755 {} "$BIN_DIR/" \;
    command -v buildkitd >/dev/null 2>&1 || err "buildkitd missing after install"
    command -v buildctl >/dev/null 2>&1 || err "buildctl missing after install"
    log "installed buildkitd and buildctl"
  fi

  need_rp=1
  if command -v railpack >/dev/null 2>&1; then
    if railpack --version 2>/dev/null | grep -q "${RAILPACK_VERSION}"; then
      need_rp=0
      log "Railpack v${RAILPACK_VERSION} already installed"
    fi
  fi
  if [ "$need_rp" -eq 1 ]; then
    rp="railpack-v${RAILPACK_VERSION}-${RP_ARCH}-unknown-linux-musl.tar.gz"
    rp_base="https://github.com/railwayapp/railpack/releases/download/v${RAILPACK_VERSION}"
    log "downloading Railpack v${RAILPACK_VERSION}"
    download "${rp_base}/${rp}" "$WORK/${rp}"
    download "${rp_base}/checksums.txt" "$WORK/railpack-checksums.txt"
    (
      cd "$WORK"
      grep " ${rp}\$" railpack-checksums.txt | sha256sum -c -
    ) || err "Railpack checksum mismatch"
    tar -xzf "$WORK/${rp}" -C "$WORK" railpack
    install -m 0755 "$WORK/railpack" "$BIN_DIR/railpack"
    railpack --version >/dev/null 2>&1 || err "railpack not runnable after install"
    log "installed railpack"
  fi

  mkdir -p "$BUILDKIT_CONFIG_DIR" "$UNIT_DIR"
  script_dir="$(CDPATH= cd -- "$(dirname "$0")" 2>/dev/null && pwd)" || script_dir=""
  if [ -n "$script_dir" ] && [ -f "$script_dir/buildkitd.toml" ]; then
    install -m 0644 "$script_dir/buildkitd.toml" "$BUILDKIT_CONFIG_DIR/buildkitd.toml"
  else
    cat >"$BUILDKIT_CONFIG_DIR/buildkitd.toml" <<'EOF'
[registry."127.0.0.1:5000"]
  http = true
  insecure = true

[worker.oci]
  gc = true
  [[worker.oci.gcpolicy]]
    keepBytes = 10737418240
    all = true
EOF
  fi
  if [ -n "$script_dir" ] && [ -f "$script_dir/buildkitd.service" ]; then
    install -m 0644 "$script_dir/buildkitd.service" "$UNIT_DIR/buildkitd.service"
  else
    cat >"$UNIT_DIR/buildkitd.service" <<EOF
[Unit]
Description=AetherixCloud BuildKit daemon (builder nodes)
Documentation=https://github.com/${REPO}
After=network-online.target docker.service
Wants=network-online.target docker.service

[Service]
Type=simple
ExecStart=${BIN_DIR}/buildkitd --config ${BUILDKIT_CONFIG_DIR}/buildkitd.toml --addr unix:///run/buildkit/buildkitd.sock --root /var/lib/buildkit
Restart=always
RestartSec=5
TimeoutStopSec=30
LimitNOFILE=1048576
RuntimeDirectory=buildkit
StateDirectory=buildkit

[Install]
WantedBy=multi-user.target
EOF
  fi
  # When BIN_DIR is non-default, point the unit at the installed binary.
  if [ "$BIN_DIR" != "/usr/local/bin" ]; then
    mkdir -p "$UNIT_DIR/buildkitd.service.d"
    cat >"$UNIT_DIR/buildkitd.service.d/override.conf" <<EOF
[Service]
ExecStart=
ExecStart=${BIN_DIR}/buildkitd --config ${BUILDKIT_CONFIG_DIR}/buildkitd.toml --addr unix:///run/buildkit/buildkitd.sock --root /var/lib/buildkit
EOF
  fi
  if command -v systemctl >/dev/null 2>&1; then
    systemctl daemon-reload
    if [ "$NO_START" -eq 0 ]; then
      systemctl enable --now buildkitd >/dev/null 2>&1 || systemctl restart buildkitd || true
      log "buildkitd enabled and started"
    else
      systemctl enable buildkitd >/dev/null 2>&1 || true
      log "buildkitd unit installed (not started; --no-start)"
    fi
  fi
}

install_unit() {
  mkdir -p "$UNIT_DIR"
  # Prefer the unit shipped next to this script when installed from a checkout;
  # otherwise write the canonical unit inline (curl | sh has no sibling files).
  script_dir="$(CDPATH= cd -- "$(dirname "$0")" 2>/dev/null && pwd)" || script_dir=""
  if [ -n "$script_dir" ] && [ -f "$script_dir/aetherixd.service" ]; then
    install -m 0644 "$script_dir/aetherixd.service" "$UNIT_DIR/aetherixd.service"
  else
    cat >"$UNIT_DIR/aetherixd.service" <<EOF
[Unit]
Description=AetherixCloud node daemon (aetherixd)
Documentation=https://github.com/${REPO}
After=network-online.target docker.service buildkitd.service
Wants=network-online.target buildkitd.service
Requires=docker.service

[Service]
Type=simple
ExecStart=${BIN_DIR}/aetherixd run --data-dir ${DATA_DIR}
Restart=on-failure
RestartSec=5
TimeoutStopSec=60
LimitNOFILE=1048576
Environment=AX_CADDY_BIN=${BIN_DIR}/caddy
Environment=AX_BUILDKIT_ADDR=unix:///run/buildkit/buildkitd.sock
Environment=AX_RAILPACK_FRONTEND=ghcr.io/railwayapp/railpack-frontend:v${RAILPACK_VERSION}

[Install]
WantedBy=multi-user.target
EOF
  fi
  # Always drop builder env so upgrades pick up the pinned frontend tag even
  # when the unit file came from an older checkout.
  mkdir -p "$UNIT_DIR/aetherixd.service.d"
  cat >"$UNIT_DIR/aetherixd.service.d/builder.conf" <<EOF
[Service]
Environment=AX_CADDY_BIN=${BIN_DIR}/caddy
Environment=AX_BUILDKIT_ADDR=unix:///run/buildkit/buildkitd.sock
Environment=AX_RAILPACK_FRONTEND=ghcr.io/railwayapp/railpack-frontend:v${RAILPACK_VERSION}
EOF
  # Override data-dir / binary path when non-default.
  if [ "$DATA_DIR" != "/var/lib/aetherix" ] || [ "$BIN_DIR" != "/usr/local/bin" ]; then
    cat >"$UNIT_DIR/aetherixd.service.d/override.conf" <<EOF
[Service]
ExecStart=
ExecStart=${BIN_DIR}/aetherixd run --data-dir ${DATA_DIR}
EOF
  fi
  if command -v systemctl >/dev/null 2>&1; then
    systemctl daemon-reload
  else
    err "systemd (systemctl) is required"
  fi
  log "installed systemd unit aetherixd.service"
}

bootstrap_or_join() {
  if [ "$DO_INIT" -eq 0 ] && [ "$DO_JOIN" -eq 0 ]; then
    log "binaries installed. Next:"
    log "  sudo aetherixd init --advertise <public-ip>   # new cluster"
    log "  sudo aetherixd join --token <token> --advertise <public-ip>"
    log "  sudo systemctl enable --now aetherixd"
    return 0
  fi

  detect_advertise
  if [ -z "$MACHINE_NAME" ]; then
    MACHINE_NAME="$(hostname -s 2>/dev/null || hostname || echo node-1)"
  fi

  if [ -f "$DATA_DIR/node.json" ]; then
    err "$DATA_DIR already contains a node; remove it to re-initialise"
  fi

  if [ "$DO_INIT" -eq 1 ]; then
    log "initialising cluster ${CLUSTER_NAME} as ${MACHINE_NAME}..."
    set -- "$BIN_DIR/aetherixd" init --json --data-dir "$DATA_DIR" --name "$MACHINE_NAME" --cluster-name "$CLUSTER_NAME"
    for a in $ADVERTISE; do
      set -- "$@" --advertise "$a"
    done
    if [ -n "$DOMAIN" ]; then
      set -- "$@" --domain "$DOMAIN"
    fi
    if [ -n "$FROM_BACKUP" ]; then
      [ -f "$FROM_BACKUP" ] || err "backup file not found: $FROM_BACKUP"
      set -- "$@" --from-backup "$FROM_BACKUP"
      log "will restore from ${FROM_BACKUP} on first start"
    fi
    init_out="$("$@")"
    printf '%s\n' "$init_out"
    token="$(printf '%s\n' "$init_out" | sed -n 's/.*"admin_token"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -n 1)"
    api_url="$(printf '%s\n' "$init_out" | sed -n 's/.*"api_url"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -n 1)"
    log ""
    log "Cluster initialised."
    log "  API:         ${api_url}"
    log "  Admin token: ${token}"
    log "  Save with:   ax context add default --url ${api_url} --token ${token} --insecure"
    log "  Dashboard:   ${api_url}  (or https://<ip>.sslip.io once ingress is up)"
    if [ -n "$FROM_BACKUP" ]; then
      log "  DR:          snapshot applied on first start; rejoin other machines afterwards"
    fi
  else
    log "joining cluster as ${MACHINE_NAME}..."
    set -- "$BIN_DIR/aetherixd" join --data-dir "$DATA_DIR" --token "$JOIN_TOKEN" --name "$MACHINE_NAME"
    for a in $ADVERTISE; do
      set -- "$@" --advertise "$a"
    done
    "$@"
  fi

  if [ "$NO_START" -eq 1 ]; then
    log "not starting systemd (--no-start); run: systemctl enable --now aetherixd"
    return 0
  fi
  systemctl enable --now aetherixd
  log "aetherixd is enabled and started"
  systemctl --no-pager --full status aetherixd | head -n 20 || true
}

# --- main --------------------------------------------------------------------

log "AetherixCloud installer"
install_pkgs
install_docker
install_aetherix_bins
install_corrosion
install_caddy
install_builder
install_unit
bootstrap_or_join
log "done."
