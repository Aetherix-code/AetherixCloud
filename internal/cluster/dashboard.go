package cluster

import (
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"

	v1 "github.com/aetherix/aetherixcloud/gen/aetherix/v1"
)

// EffectiveDashboardPort returns the public dashboard port (default 443 when a
// host is configured, otherwise 0).
func EffectiveDashboardPort(c *v1.Cluster) int {
	if c == nil || c.DashboardHost == "" {
		return 0
	}
	if c.DashboardPort > 0 {
		return int(c.DashboardPort)
	}
	return 443
}

// PublicDashboardURL is the HTTPS origin for the dashboard/API/MCP when a
// public host is configured ("" otherwise). Port 443 is omitted from the URL.
func PublicDashboardURL(c *v1.Cluster) string {
	if c == nil {
		return ""
	}
	host := strings.ToLower(strings.TrimSpace(c.DashboardHost))
	if host == "" {
		return ""
	}
	port := EffectiveDashboardPort(c)
	if port == 443 {
		return "https://" + host
	}
	return "https://" + net.JoinHostPort(host, strconv.Itoa(port))
}

// DashboardHostFromURL extracts host (and optional port) from a dashboard URL
// such as https://cloud.example.com or https://x:8443.
func DashboardHostFromURL(raw string) (host string, port int32, err error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", 0, nil
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", 0, err
	}
	h := strings.ToLower(u.Hostname())
	if h == "" {
		return "", 0, fmt.Errorf("missing host")
	}
	if u.Port() == "" {
		return h, 0, nil
	}
	p, err := strconv.Atoi(u.Port())
	if err != nil || p <= 0 || p > 65535 {
		return "", 0, fmt.Errorf("invalid port")
	}
	return h, int32(p), nil
}
