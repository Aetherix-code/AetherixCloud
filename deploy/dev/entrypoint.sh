#!/usr/bin/env bash
# Dev node entrypoint: start dockerd, init or join the cluster once, run aetherixd.
set -euo pipefail

DATA=/var/lib/aetherix
mkdir -p "$DATA"

mkdir -p /dev/net
[ -c /dev/net/tun ] || mknod /dev/net/tun c 10 200 || true

# Docker-in-Docker daemon. After an outer-container SIGKILL (ax dev kill /
# TestKillNodeReschedule), pid files on the persisted /var/lib/docker volume
# still name PIDs that happen to exist again in the new pid namespace — dockerd
# then aborts with "process with PID N is still running". Wipe them first.
rm -f /var/run/docker.pid /run/docker.pid \
      /var/run/docker/containerd/containerd.pid /run/containerd/containerd.pid
find /var/lib/docker -name '*.pid' -type f -delete 2>/dev/null || true
dockerd --host=unix:///var/run/docker.sock >/var/log/dockerd.log 2>&1 &
for i in $(seq 1 60); do docker info >/dev/null 2>&1 && break; sleep 1; done
docker info >/dev/null 2>&1 || { echo "dockerd failed to start"; tail -50 /var/log/dockerd.log; exit 1; }

# BuildKit daemon for the auto-build pipeline (Dockerfile + Railpack). Host
# networking keeps builds simple inside this nested container; the embedded
# registry on 127.0.0.1:5000 is reachable for pushes and the layer cache.
mkdir -p /run/buildkit /var/lib/buildkit
buildkitd --config /etc/buildkit/buildkitd.toml --addr unix:///run/buildkit/buildkitd.sock \
  --root /var/lib/buildkit --oci-worker-net=host >/var/log/buildkitd.log 2>&1 &
for i in $(seq 1 30); do buildctl --addr unix:///run/buildkit/buildkitd.sock debug workers >/dev/null 2>&1 && break; sleep 1; done
buildctl --addr unix:///run/buildkit/buildkitd.sock debug workers >/dev/null 2>&1 \
  || echo "warning: buildkitd is not ready (builds will fail); see /var/log/buildkitd.log" >&2

if [ ! -f "$DATA/node.json" ]; then
  if [ "${AX_INIT:-}" = "1" ]; then
    aetherixd init --json --data-dir "$DATA" --advertise "${AX_ADVERTISE}" --name "${AX_NODE_NAME}" \
      --cluster-name "${AX_CLUSTER_NAME:-dev}" > /tmp/init.json
    mv /tmp/init.json "$DATA/init.json"   # admin_token + ca_pem, read by `ax dev up`
  elif [ -n "${AX_JOIN_TOKEN:-}" ]; then
    aetherixd join --data-dir "$DATA" --token "$AX_JOIN_TOKEN" --advertise "${AX_ADVERTISE}" --name "${AX_NODE_NAME}"
  else
    echo "no cluster state and neither AX_INIT=1 nor AX_JOIN_TOKEN set" >&2; exit 1
  fi
fi

EXTRA=()
[ "${AX_DEV:-}" = "1" ] && EXTRA+=(--fast-membership)
exec aetherixd run --data-dir "$DATA" "${EXTRA[@]}" "$@"
