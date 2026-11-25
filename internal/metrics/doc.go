// Package metrics samples per-replica Docker stats into a node-local ring
// buffer and serves them at GET /api/metrics?service_id=… for the dashboard
// Metrics tab (and MCP get_metrics when wired to the same path).
package metrics
