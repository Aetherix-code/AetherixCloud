# Testing

## Fast default suite

```sh
CGO_ENABLED=0 go test ./...
cd web && npm test          # vitest unit tests
```

These stay Docker-free. Integration and Playwright packages are separate.

## Integration harness (`internal/devcluster`)

The same package behind `ax dev` drives multi-node scenarios. Tests are gated:

```go
//go:build integration
```

so `go test ./...` never starts DinD. Run them explicitly:

```sh
# 1. Build the node image once (several minutes the first time)
make dev-image

# 2. Run the suite (skips cleanly if Docker/image is missing)
go test -tags=integration -count=1 -timeout 20m ./internal/devcluster/

# Force an image rebuild as part of the suite:
AX_INTEGRATION_BUILD=1 go test -tags=integration -count=1 -timeout 30m ./internal/devcluster/

# Disable even with the tag:
AX_INTEGRATION=0 go test -tags=integration ./internal/devcluster/
```

Scenarios covered:

| Test | What it asserts |
| --- | --- |
| `TestMultiNodeJoin` | 3 nodes initialise / join and report UP |
| `TestDeployRollingUpdateZeroDowntime` | Deploy + start-first rollout under a load generator |
| `TestKillNodeReschedule` | Killing a worker moves reschedulable replicas |
| `TestPartitionAndHeal` | `Partition` / `Heal` and API stays reachable |

Unit tests for helpers (`LoadGen`, naming, scripts) run in the default suite without the tag.

## Sample apps / auto-build

Fixtures live in [`testdata/apps/`](../testdata/apps/) (Node, Python, Go, static). Detection and pipeline tests use BuildKit/Railpack **fakes**:

```sh
go test ./internal/build/ -run SampleApps
```

## MCP contract tests

`internal/mcp` already exercises the Go SDK client: tool catalog, scopes/RBAC, confirmation gating, audit, rate limits, OAuth, resources/prompts, plus deploy lifecycle tools (`deploy_service`, `redeploy`, `rollback`, `add_domain`).

```sh
go test ./internal/mcp/ ./cmd/ax/ -run MCP
```

## Playwright (dashboard e2e)

Mock-mode flows (no backend):

```sh
cd web
npm ci
npx playwright install chromium   # first time
npm run test:e2e
```

`VITE_MOCK=1` is set by `playwright.config.ts` via the Vite webServer. Real-backend e2e is optional and only runs when `AX_E2E_URL` points at a live cluster (`ax dev up`).

## Makefile shortcuts

```sh
make test              # CGO_ENABLED=0 go test ./...
make test-integration  # -tags=integration ./internal/devcluster/
make test-e2e          # web Playwright mock suite
```
