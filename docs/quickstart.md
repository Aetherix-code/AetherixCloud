# Quickstart (local playground)

Run a multi-node AetherixCloud cluster on your laptop with Docker-in-Docker. No VPS required.

## Prerequisites

- Docker Desktop, OrbStack, or another Docker engine
- Go 1.26+ (for building `ax` / `aetherixd`)
- Node.js + npm (only if you rebuild the dashboard with `make web` / `make dev`)

## Bring up a cluster

From the repo root:

```sh
make build
./bin/ax dev up --nodes 3          # or: make dev-cluster
```

`ax dev up` builds/pulls the node image (`deploy/dev/Dockerfile.node`), creates the `ax-dev` network, starts privileged DinD nodes, runs `aetherixd init` on node-1 and joins the rest, then saves a CLI context named `dev`.

It prints:

- **Dashboard/API** URL (default `https://127.0.0.1:8443`)
- **Admin token** (also stored in `~/.config/aetherix/config.json` under context `dev`)

Open the URL in a browser (the cluster CA is self-signed; accept the warning or use `--insecure` with the CLI).

## Useful commands

```sh
ax --context dev machine ls
ax --context dev status
ax --context dev deploy -f compose.yaml    # when you have a compose file
ax dev kill node-2                         # simulate a dead machine
ax dev partition node-1 node-3             # network split
ax dev heal
ax dev down --volumes                      # tear down
```

Contributor loop:

- `make dev` — Vite hot-reload UI; API/MCP proxied to node-1 (`AX_PROXY`, default `http://127.0.0.1:8080`)
- `make dev-daemon` — single-node host daemon without WireGuard/Docker (`aetherixd run --store sqlite --no-mesh --no-docker`)

## MCP against the local cluster

```sh
ax --context dev mcp config cursor   # paste into Cursor mcp.json
ax --context dev mcp                 # stdio proxy to https://…/mcp
```

See [mcp.md](mcp.md).

## Plain compose alternative

If you prefer not to use the CLI orchestrator:

```sh
docker compose -f deploy/dev/compose.yaml up
```

Bootstrap still uses `AX_INIT` / `AX_JOIN_TOKEN` inside the node entrypoint (`deploy/dev/entrypoint.sh`).
