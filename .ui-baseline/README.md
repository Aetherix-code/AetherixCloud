# UI baseline (pre-modernization)

Snapshot of the dashboard UI **before** the Nov 24, 2026 modernization pass.

## Restore

```sh
cd /Users/jerry/Developer/AetherixCloud
tar -xzf .ui-baseline/pre-modernization-20261124.tar.gz
# then rebuild / hot-reload:
#   make dev
# or: make web && make build && ./bin/ax dev down && make dev-cluster
```

Ask the agent: “revert the UI modernization” and point at this archive.
