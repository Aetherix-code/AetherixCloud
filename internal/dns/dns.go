// Package dns is the embedded service-discovery resolver.
//
// Names under ".internal" resolve to the container IPs of RUNNING replicas
// from the replicated store, in round-robin order:
//
//	<service>.internal                 all services with that name
//	<service>.<project>.internal       service within a project
//	<machine>.machine.internal         machine mesh IP
//
// Everything else is forwarded to the upstream resolvers. The server listens
// on the machine's mesh IP (containers get it via --dns).
package dns

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync/atomic"
	"time"

	v1 "github.com/aetherix/aetherixcloud/gen/aetherix/v1"
	"github.com/aetherix/aetherixcloud/internal/store"
	mdns "github.com/miekg/dns"
)

// Suffix is the internal zone.
const Suffix = "internal."

// Server is the resolver.
type Server struct {
	Store    store.Store
	Upstream []string // "ip:port"; defaults to system resolv.conf / 1.1.1.1
	TTL      uint32
	Logger   *slog.Logger

	rr atomic.Uint64
}

// Resolve returns the A records for name and whether the name is in the
// internal zone and exists. Exposed for tests and for other components.
func (s *Server) Resolve(ctx context.Context, name string) (ips []net.IP, authoritative bool, err error) {
	name = strings.ToLower(mdns.Fqdn(name))
	if !strings.HasSuffix(name, "."+Suffix) && name != Suffix {
		return nil, false, nil
	}
	labels := strings.Split(strings.TrimSuffix(name, "."+Suffix), ".")
	switch {
	case len(labels) == 2 && labels[1] == "machine":
		ms, err := store.ListMachines(ctx, s.Store)
		if err != nil {
			return nil, true, err
		}
		for _, m := range ms {
			if m.Name == labels[0] {
				if ip := net.ParseIP(m.MeshIp); ip != nil {
					ips = append(ips, ip)
				}
			}
		}
		return ips, true, nil
	case len(labels) == 1 || len(labels) == 2:
		where := map[string]string{"name": labels[0]}
		if len(labels) == 2 {
			ps, err := store.ListProto[v1.Project](ctx, s.Store, store.TableProjects, store.Query{Where: map[string]string{"name": labels[1]}})
			if err != nil {
				return nil, true, err
			}
			if len(ps) == 0 {
				return nil, true, nil
			}
			where["project_id"] = ps[0].Id
		}
		svcs, err := store.ListServices(ctx, s.Store, where)
		if err != nil {
			return nil, true, err
		}
		for _, svc := range svcs {
			reps, err := store.ListReplicas(ctx, s.Store, map[string]string{"service_id": svc.Id})
			if err != nil {
				return nil, true, err
			}
			for _, r := range reps {
				if r.DesiredRunning && r.State == v1.ReplicaState_REPLICA_STATE_RUNNING && r.ContainerIp != "" {
					if ip := net.ParseIP(r.ContainerIp); ip != nil {
						ips = append(ips, ip)
					}
				}
			}
		}
		// stable order, then rotate for round-robin
		sortIPs(ips)
		if n := len(ips); n > 1 {
			k := int(s.rr.Add(1) % uint64(n))
			ips = append(ips[k:], ips[:k]...)
		}
		return ips, true, nil
	}
	return nil, true, nil
}

func sortIPs(ips []net.IP) {
	for i := 1; i < len(ips); i++ {
		for j := i; j > 0 && strings.Compare(ips[j-1].String(), ips[j].String()) > 0; j-- {
			ips[j-1], ips[j] = ips[j], ips[j-1]
		}
	}
}

// ServeDNS implements dns.Handler.
func (s *Server) ServeDNS(w mdns.ResponseWriter, r *mdns.Msg) {
	if len(r.Question) != 1 {
		m := new(mdns.Msg)
		m.SetRcode(r, mdns.RcodeFormatError)
		_ = w.WriteMsg(m)
		return
	}
	q := r.Question[0]
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ips, auth, err := s.Resolve(ctx, q.Name)
	if !auth {
		s.forward(w, r)
		return
	}
	m := new(mdns.Msg)
	m.SetReply(r)
	m.Authoritative = true
	ttl := s.TTL
	if ttl == 0 {
		ttl = 1
	}
	switch {
	case err != nil:
		m.Rcode = mdns.RcodeServerFailure
	case q.Qtype == mdns.TypeA || q.Qtype == mdns.TypeANY:
		for _, ip := range ips {
			if v4 := ip.To4(); v4 != nil {
				m.Answer = append(m.Answer, &mdns.A{Hdr: mdns.RR_Header{Name: q.Name, Rrtype: mdns.TypeA, Class: mdns.ClassINET, Ttl: ttl}, A: v4})
			}
		}
		if len(m.Answer) == 0 {
			m.Rcode = mdns.RcodeNameError
		}
	default: // AAAA etc.: name exists check
		if len(ips) == 0 {
			m.Rcode = mdns.RcodeNameError
		}
	}
	_ = w.WriteMsg(m)
}

func (s *Server) upstreams() []string {
	if len(s.Upstream) > 0 {
		return s.Upstream
	}
	if cfg, err := mdns.ClientConfigFromFile("/etc/resolv.conf"); err == nil && len(cfg.Servers) > 0 {
		var out []string
		for _, sv := range cfg.Servers {
			out = append(out, net.JoinHostPort(sv, cfg.Port))
		}
		return out
	}
	return []string{"1.1.1.1:53", "8.8.8.8:53"}
}

func (s *Server) forward(w mdns.ResponseWriter, r *mdns.Msg) {
	c := &mdns.Client{Timeout: 3 * time.Second}
	for _, up := range s.upstreams() {
		if resp, _, err := c.Exchange(r, up); err == nil {
			_ = w.WriteMsg(resp)
			return
		}
	}
	m := new(mdns.Msg)
	m.SetRcode(r, mdns.RcodeServerFailure)
	_ = w.WriteMsg(m)
}

// ListenAndServe serves UDP and TCP on addr until ctx is done.
func (s *Server) ListenAndServe(ctx context.Context, addr string) error {
	mux := mdns.NewServeMux()
	mux.Handle(".", s)
	udp := &mdns.Server{Addr: addr, Net: "udp", Handler: mux}
	tcp := &mdns.Server{Addr: addr, Net: "tcp", Handler: mux}
	errc := make(chan error, 2)
	go func() { errc <- udp.ListenAndServe() }()
	go func() { errc <- tcp.ListenAndServe() }()
	select {
	case <-ctx.Done():
		_ = udp.Shutdown()
		_ = tcp.Shutdown()
		return nil
	case err := <-errc:
		_ = udp.Shutdown()
		_ = tcp.Shutdown()
		return fmt.Errorf("dns: %w", err)
	}
}
