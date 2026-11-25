# MCP and ChatGPT connector

Every node serves a Model Context Protocol server at **`/mcp`** (Streamable HTTP) behind the same listeners as the dashboard. Tools map onto the Connect-RPC services; there is no separate control path.

## Remote connector (ChatGPT, Claude, Cursor)

1. Use the public dashboard origin + `/mcp`, e.g. `https://dashboard.example.com/mcp` (or `https://127.0.0.1:8443/mcp` in `ax dev`).
2. Add it as a **custom connector**. The daemon is an OAuth 2.1 authorization server (`internal/oauth`): protected-resource metadata, dynamic client registration, PKCE, and a consent screen at `/oauth/consent`.
3. Sign in with your AetherixCloud account. Access tokens are scoped (`read` / `deploy` / `admin`) and still subject to RBAC. Revoke grants under **Settings → AI agents**.

Dashboard copy-paste configs live on that same page (`web/src/routes/settings.tsx`).

## Local agents (`ax mcp`)

Stdio MCP that proxies to the cluster using the current `ax` context (API token). No OAuth setup:

```sh
ax mcp config cursor     # → { "mcpServers": { "aetherix": { "command": "ax", "args": ["mcp"] } } }
ax mcp config claude
ax mcp config vscode
ax mcp                   # run the proxy
```

## Tool surface

| Scope | Examples |
| --- | --- |
| Read | `list_projects`, `list_services`, `get_logs`, `get_build_logs`, `list_machines`, `get_cluster_status`, … |
| Deploy | `create_service`, `deploy_service`, `rollback`, `scale_service`, `set_variables`, `deploy_compose`, … |
| Admin | `create_join_command`, `drain_machine`, `cordon_machine`, `remove_machine`, `delete_service`, … |

Destructive tools require `confirm: true` (and often return a dry-run summary first unless the token has `admin`). Secret variable values are redacted. Calls are written to the audit log.

## Auth notes

- Bearer API tokens work on `/mcp` for scripts and remote Cursor configs.
- OAuth connector tokens are audience-bound to `/mcp`.
- Rate limits and scopes are enforced in `internal/auth` / `internal/mcp`.
