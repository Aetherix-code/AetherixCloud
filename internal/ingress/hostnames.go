package ingress

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"net/netip"
	"sort"
	"strings"

	v1 "github.com/aetherix/aetherixcloud/gen/aetherix/v1"
	"github.com/aetherix/aetherixcloud/internal/store"
	"google.golang.org/protobuf/proto"
)

// HostOptions control default hostname generation.
type HostOptions struct {
	// Dev: "<service>-<project>.localhost".
	Dev bool
	// ClusterDomain (cluster.domain) wins over sslip.io when set:
	// "<service>-<project>.<domain>".
	ClusterDomain string
	// PublicIP is the ingress address used for sslip.io names.
	PublicIP string
}

// Slug makes a DNS label from a name.
func Slug(s string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			dash = false
		} else if !dash && b.Len() > 0 {
			b.WriteByte('-')
			dash = true
		}
	}
	out := strings.TrimRight(b.String(), "-")
	if len(out) > 60 {
		out = strings.TrimRight(out[:60], "-")
	}
	return out
}

// DefaultHostname returns the generated hostname for a service, or "" when no
// suitable base is known (no cluster domain and no public IP outside dev).
func DefaultHostname(service, project string, o HostOptions) string {
	label := Slug(service)
	if p := Slug(project); p != "" {
		label += "-" + p
	}
	if len(label) > 63 {
		label = strings.TrimRight(label[:63], "-")
	}
	switch {
	case o.Dev:
		return label + ".localhost"
	case o.ClusterDomain != "":
		return label + "." + strings.Trim(strings.ToLower(o.ClusterDomain), ".")
	case o.PublicIP != "":
		ip := o.PublicIP
		if a, err := netip.ParseAddr(ip); err == nil && a.Is6() {
			ip = strings.ReplaceAll(a.String(), ":", "-")
		}
		return label + "." + ip + ".sslip.io"
	}
	return ""
}

// IngressIPs lists the public IPs advertised by ingress machines (from their
// WireGuard endpoint candidates), sorted by machine ID then position.
func IngressIPs(machines []*v1.Machine) []string {
	ms := append([]*v1.Machine(nil), machines...)
	sort.Slice(ms, func(i, j int) bool { return ms[i].Id < ms[j].Id })
	var out []string
	seen := map[string]bool{}
	for _, m := range ms {
		if !IsIngress(m) || m.State == v1.MachineState_MACHINE_STATE_DOWN {
			continue
		}
		for _, ep := range m.Endpoints {
			host, _, err := net.SplitHostPort(ep)
			if err != nil {
				host = ep
			}
			if a, err := netip.ParseAddr(host); err == nil && !seen[a.String()] && !a.IsLoopback() && !a.IsPrivate() {
				seen[a.String()] = true
				out = append(out, a.String())
			}
		}
	}
	if len(out) == 0 { // private-only clusters (LAN, dev): fall back to any address
		for _, m := range ms {
			for _, ep := range m.Endpoints {
				host, _, err := net.SplitHostPort(ep)
				if err != nil {
					host = ep
				}
				if a, err := netip.ParseAddr(host); err == nil && !seen[a.String()] {
					seen[a.String()] = true
					out = append(out, a.String())
				}
			}
		}
	}
	return out
}

// webPorts returns the HTTP-routed ports of spec (mode http/https).
func webPorts(spec *v1.ServiceSpec) []*v1.ServicePort {
	var out []*v1.ServicePort
	for _, p := range spec.GetPorts() {
		if p.Mode == "http" || p.Mode == "https" {
			out = append(out, p)
		}
	}
	return out
}

func shortHash(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:4])
}

// EnsureDomains creates/updates the generated and port-declared Domain rows
// for every service with http/https ports. Row IDs are deterministic so
// concurrent runs converge. Run it under a lease (Controller does).
func EnsureDomains(ctx context.Context, st store.Store, o HostOptions) (changed int, err error) {
	services, err := store.ListServices(ctx, st, nil)
	if err != nil {
		return 0, err
	}
	projects, err := store.ListProto[v1.Project](ctx, st, store.TableProjects, store.Query{})
	if err != nil {
		return 0, err
	}
	pname := map[string]string{}
	for _, p := range projects {
		pname[p.Id] = p.Name
	}
	domains, err := store.ListDomains(ctx, st, "")
	if err != nil {
		return 0, err
	}
	byID := map[string]*v1.Domain{}
	byHost := map[string]*v1.Domain{}
	for _, d := range domains {
		byID[d.Id] = d
		byHost[d.Hostname] = d
	}
	put := func(d *v1.Domain) error {
		if err := store.PutDomain(ctx, st, d); err != nil {
			return err
		}
		changed++
		return nil
	}
	for _, svc := range services {
		ports := webPorts(svc.Spec) // legacy http/https port modes
		id := "gen-" + svc.Id
		cur, hasGen := byID[id]
		// Default HTTPS domains are owned by the Domains UI (AddDomain with
		// empty hostname). Keep refreshing hostname when IP/cluster domain
		// changes. Legacy: still auto-create when http/https ports exist.
		if hasGen || len(ports) > 0 {
			if host := DefaultHostname(svc.Name, pname[svc.ProjectId], o); host != "" {
				port := int32(0)
				if hasGen {
					port = cur.Port
				}
				if port == 0 && len(ports) > 0 {
					port = ports[0].ContainerPort
				}
				if hasGen {
					if cur.Hostname != host {
						n := proto.Clone(cur).(*v1.Domain)
						n.Hostname = host
						if err := put(n); err != nil {
							return changed, err
						}
					}
				} else if _, taken := byHost[host]; !taken && port > 0 {
					if err := put(&v1.Domain{Id: id, ServiceId: svc.Id, Hostname: host, Port: port,
						TlsState: "pending", Generated: true}); err != nil {
						return changed, err
					}
				}
			}
		}
		for _, p := range ports {
			h := strings.ToLower(strings.TrimSpace(p.Hostname))
			if h == "" {
				continue
			}
			if _, taken := byHost[h]; taken {
				continue
			}
			if err := put(&v1.Domain{Id: fmt.Sprintf("port-%s-%d-%s", svc.Id, p.ContainerPort, shortHash(h)),
				ServiceId: svc.Id, Hostname: h, Port: p.ContainerPort, TlsState: "pending"}); err != nil {
				return changed, err
			}
		}
	}
	return changed, nil
}
