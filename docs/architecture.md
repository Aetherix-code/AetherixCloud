# Architecture overview

AetherixCloud is a peer-to-peer, self-hosted PaaS. Every machine runs one identical daemon.

```
Browser / CLI / MCP ──HTTPS──► Caddy (ingress nodes)
                              │
                              ▼
                         aetherixd (:8443 TLS public, :8080 on mesh/loopback)
                              │
          ┌───────────────────┼───────────────────┐
          ▼                   ▼                   ▼
     WireGuard mesh     Corrosion CRDT        Docker runtime
     (10.210.0.0/16)    (or SQLite)           + embedded registry
          │                   │                   │
          └────── gossip / Connect-RPC over mesh ─┘
```

## Per-node components (`aetherixd`)

| Piece | Package | Role |
| --- | --- | --- |
| Mesh | `internal/mesh` | WireGuard via `wgctrl`; `/24` per machine from `10.210.0.0/16` |
| Store | `internal/store` | Corrosion sidecar when installed; else SQLite |
| Membership | `internal/membership` | SWIM (`memberlist`); machine states up/suspect/down |
| Runtime / reconciler | `internal/runtime`, `internal/reconciler` | Docker containers labeled by service/deployment/replica |
| Scheduler | `internal/scheduler` | Replicated/global modes, placement |
| Deploy | `internal/deploy` | Health-gated rolling updates (start-first / stop-first), history, rollback |
| Reschedule | `internal/reschedule` | After grace, lease-holder moves reschedulable replicas |
| Ingress | `internal/ingress` | Caddy config + shared cert storage module (`deploy/caddy`) |
| DNS | `internal/dns` | Embedded resolver on the Docker/mesh network (`*.internal`) |
| Registry | `internal/registry` | Embedded distribution registry; peer pull over the mesh |
| Build / GitHub | `internal/build`, `internal/github` | BuildKit + Railpack; App webhooks and statuses |
| Auth / OAuth / MCP | `internal/auth`, `internal/oauth`, `internal/mcp` | Users, JWT, API tokens, MCP OAuth, `/mcp` |
| API | `internal/api` | Connect-RPC (protobuf in `proto/aetherix/v1`) |
| Web | `internal/web` | React SPA embedded with `go:embed` |

## Consistency

Most writes are idempotent CRDT/SQLite rows. Exactly-once actors (builds, reschedule, ACME) take a **lease** with a fencing token (`internal/lease`): claim, wait for gossip convergence, read back; ties break on lowest machine ID.

## CLI (`ax`)

Talks HTTPS + API token to any node (Connect-RPC). Also bootstraps machines over SSH (`ax machine init|add`) and drives the DinD playground (`ax dev`).

## Public surface

Only Caddy (80/443) and WireGuard (UDP 51820) need to be reachable between sites. The dashboard API is on 8443 (TLS) and on the mesh; MCP and GitHub webhooks share the same HTTP stack as the API/SPA.
