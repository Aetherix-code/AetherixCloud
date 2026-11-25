package ingress

import (
	"context"
	"fmt"
	"net"
	"strings"
)

// Resolver is the subset of net.Resolver used for domain verification.
type Resolver interface {
	LookupHost(ctx context.Context, host string) ([]string, error)
}

// DNSHints explains which records a user must create for hostname. ips are
// the ingress machines' public addresses; defaultHost (optional) is the
// generated hostname usable as a CNAME target.
func DNSHints(hostname string, ips []string, defaultHost string) []string {
	var out []string
	if strings.HasSuffix(hostname, ".localhost") || hostname == "localhost" {
		return []string{hostname + " resolves to 127.0.0.1 automatically; no DNS setup needed"}
	}
	for _, ip := range ips {
		typ := "A"
		if strings.Contains(ip, ":") {
			typ = "AAAA"
		}
		out = append(out, fmt.Sprintf("%s  %s  %s", hostname, typ, ip))
	}
	if defaultHost != "" {
		out = append(out, fmt.Sprintf("or: %s  CNAME  %s (not at the zone apex)", hostname, defaultHost))
	}
	if len(out) == 0 {
		out = append(out, fmt.Sprintf("Point an A/AAAA record for %s at any ingress machine's public IP", hostname))
	}
	if strings.HasPrefix(hostname, "*.") {
		out = append(out, "wildcard hostnames need a DNS-01 provider (set AX_DNS_CLOUDFLARE_TOKEN)")
	}
	return out
}

// VerifyResult is the outcome of VerifyDomain.
type VerifyResult struct {
	Verified bool
	Resolved []string
	Message  string
}

// VerifyDomain checks that hostname resolves to one of ips.
func VerifyDomain(ctx context.Context, r Resolver, hostname string, ips []string) VerifyResult {
	if strings.HasSuffix(hostname, ".localhost") {
		return VerifyResult{Verified: true, Resolved: []string{"127.0.0.1"}, Message: "localhost names need no DNS"}
	}
	if r == nil {
		r = net.DefaultResolver
	}
	got, err := r.LookupHost(ctx, strings.TrimPrefix(hostname, "*."))
	if err != nil {
		return VerifyResult{Message: fmt.Sprintf("%s does not resolve yet: %v", hostname, err)}
	}
	want := map[string]bool{}
	for _, ip := range ips {
		want[ip] = true
	}
	for _, g := range got {
		if want[g] {
			return VerifyResult{Verified: true, Resolved: got, Message: "DNS points at this cluster"}
		}
	}
	return VerifyResult{Resolved: got, Message: fmt.Sprintf("%s resolves to %s, expected one of %s",
		hostname, strings.Join(got, ", "), strings.Join(ips, ", "))}
}
