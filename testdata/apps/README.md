# Sample apps for auto-build detection

Minimal apps used by `internal/build` unit tests to exercise Railpack/auto-build
detection without BuildKit:

| Directory | Stack markers | Expected provider |
| --- | --- | --- |
| `node/` | `package.json` | railpack (node) |
| `python/` | `requirements.txt`, `main.py` | railpack (python) |
| `go/` | `go.mod`, `main.go` | railpack (go) |
| `static/` | `index.html` only | railpack (static) |

Run:

```sh
go test ./internal/build/ -run SampleApps
```
