# AetherixCloud

Self-hosted, peer-to-peer PaaS in Go. Every machine runs the same daemon (`aetherixd`): WireGuard mesh, CRDT-replicated state (Corrosion), Docker runtime, Caddy ingress, embedded dashboard, deploy-on-push via a GitHub App, and an MCP server so agents (ChatGPT, Claude, Cursor) can operate the cluster.

Modeled on Uncloud-style architecture — **no separate control plane**.

## Quick links

| | |
| --- | --- |
| Local playground | [docs/quickstart.md](docs/quickstart.md) — `ax dev up` |
| VPS install | [docs/bootstrap.md](docs/bootstrap.md) — `deploy/install.sh` |
| GitHub App | [docs/github-app.md](docs/github-app.md) |
| MCP / ChatGPT | [docs/mcp.md](docs/mcp.md) |
| Architecture | [docs/architecture.md](docs/architecture.md) |
| Docs index | [docs/README.md](docs/README.md) |

## Try it locally

```sh
make build
./bin/ax dev up --nodes 3
# open the printed https://127.0.0.1:8443 URL; context "dev" is saved for the CLI
```

## Install on a VPS

```sh
curl -fsSL https://raw.githubusercontent.com/Aetherix-code/AetherixCloud/main/deploy/install.sh | sudo sh -s -- --init --advertise YOUR_PUBLIC_IP
```

Adds Docker, `aetherixd`/`ax`, Corrosion, Caddy, and a systemd unit. Join more machines with the one-line command from `ax machine add --print` or the dashboard.

## Repository layout

```
cmd/aetherixd/     node daemon
cmd/ax/            CLI
proto/aetherix/v1/ Connect-RPC services
internal/          mesh, store, runtime, deploy, ingress, build, github, mcp, …
web/               React dashboard (embedded via make web → internal/web/dist)
deploy/            install.sh, systemd unit, Caddy module, DinD playground
docs/              operator guides
```

## Build

```sh
make build          # bin/ax, bin/aetherixd (CGO_ENABLED=0)
make web            # build SPA into internal/web/dist for go:embed
make test           # fast unit/integration (no Docker)
make test-integration  # DinD multi-node harness (-tags=integration)
make test-e2e       # Playwright dashboard flows (VITE_MOCK=1)
make proto          # regenerate Go Connect stubs (needs .bin/buf)
```

See [docs/testing.md](docs/testing.md) for the harness, sample apps, MCP contracts and Playwright.

Multi-arch release archives: [`.goreleaser.yaml`](.goreleaser.yaml) (`goreleaser release --clean`).

## Status

Core daemon, mesh, runtime, rolling deploys, dashboard shell, GitHub/auto-build, MCP/OAuth, local `ax dev` playground, release install path, and the testing harness (unit + tagged DinD + Playwright) are in tree.
