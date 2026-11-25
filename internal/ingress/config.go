package ingress

import (
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"

	v1 "github.com/aetherix/aetherixcloud/gen/aetherix/v1"
)

// StorageRef points Caddy at the replicated-store storage module
// (deploy/caddy, module id "aetherix").
type StorageRef struct {
	Endpoint string `json:"endpoint"`
	Token    string `json:"token"`
}

// DNS01 enables DNS-01 challenges (wildcard certificates).
type DNS01 struct {
	// Provider is the caddy-dns module name; only "cloudflare" is wired.
	Provider string
	APIToken string
	// Wildcards are zones to obtain "*.<zone>" certificates for.
	Wildcards []string
}

// Options configure config generation.
type Options struct {
	// Dev uses Caddy's internal CA instead of ACME (local_certs).
	Dev       bool
	AdminAddr string // default 127.0.0.1:2019
	HTTPPort  int    // default 80
	HTTPSPort int    // default 443
	ACMEEmail string
	ACMECA    string
	// Storage, when set, shares certificates and ACME locks via the store.
	Storage *StorageRef
	DNS01   *DNS01
}

// ExtraRoute lets other components (dashboard, MCP) be exposed through the
// same Caddy: Hosts are routed to Dial addresses ("host:port").
type ExtraRoute struct {
	Hosts []string
	Dial  []string
	// TLS dials the upstream with HTTPS and skips verification (the node API
	// uses cluster-CA certificates).
	TLS bool
}

// Input is the store snapshot the config is generated from.
type Input struct {
	Options     Options
	Domains     []*v1.Domain
	Services    map[string]*v1.Service
	Deployments map[string]*v1.Deployment
	Replicas    []*v1.Replica
	Machines    map[string]*v1.Machine
	// Draining are replica IDs that must not receive traffic.
	Draining map[string]bool
	Extra    []ExtraRoute
}

// Route summarises one generated route (for status/tests).
type Route struct {
	Hostname  string
	Upstreams []string
}

func (o Options) admin() string {
	if o.AdminAddr == "" {
		return "127.0.0.1:2019"
	}
	return o.AdminAddr
}

// Upstreams returns the dial addresses of healthy replicas of d's service.
func (in *Input) upstreams(d *v1.Domain) (dials []string, health map[string]any) {
	svc := in.Services[d.ServiceId]
	if svc == nil {
		return nil, nil
	}
	for _, r := range in.Replicas {
		if r.ServiceId != d.ServiceId || !r.DesiredRunning || r.State != v1.ReplicaState_REPLICA_STATE_RUNNING ||
			r.ContainerIp == "" || in.Draining[r.Id] {
			continue
		}
		if m := in.Machines[r.MachineId]; m != nil && m.State == v1.MachineState_MACHINE_STATE_DOWN {
			continue
		}
		spec := svc.Spec
		if dep := in.Deployments[r.DeploymentId]; dep != nil && dep.Spec != nil {
			spec = dep.Spec
		}
		port := d.Port
		if port == 0 {
			if wp := webPorts(spec); len(wp) > 0 {
				port = wp[0].ContainerPort
			} else if len(spec.GetPorts()) > 0 {
				port = spec.Ports[0].ContainerPort
			}
		}
		if port == 0 {
			continue
		}
		dials = append(dials, net.JoinHostPort(r.ContainerIp, strconv.Itoa(int(port))))
		if hc := spec.GetHealthCheck(); hc != nil && hc.HttpPath != "" && health == nil {
			health = map[string]any{"uri": hc.HttpPath, "interval": "5s", "timeout": "3s"}
		}
	}
	sort.Strings(dials)
	return dials, health
}

// Generate builds the complete Caddy JSON config.
func Generate(in Input) (map[string]any, []Route) {
	o := in.Options
	var routes []any
	var summary []Route

	domains := append([]*v1.Domain(nil), in.Domains...)
	sort.Slice(domains, func(i, j int) bool { return domains[i].Hostname < domains[j].Hostname })
	hosts := []string{}
	for _, d := range domains {
		if d.Hostname == "" {
			continue
		}
		dials, health := in.upstreams(d)
		summary = append(summary, Route{Hostname: d.Hostname, Upstreams: dials})
		hosts = append(hosts, d.Hostname)
		routes = append(routes, hostRoute([]string{d.Hostname}, dials, health, false))
	}
	for _, e := range in.Extra {
		if len(e.Hosts) == 0 {
			continue
		}
		hosts = append(hosts, e.Hosts...)
		routes = append(routes, hostRoute(e.Hosts, e.Dial, nil, e.TLS))
	}

	httpApp := map[string]any{
		"grace_period": "10s",
		"servers": map[string]any{"ax": map[string]any{
			"listen": []string{":" + strconv.Itoa(orInt(o.HTTPSPort, 443))},
			"routes": routes,
		}},
	}
	if o.HTTPPort != 0 {
		httpApp["http_port"] = o.HTTPPort
	}
	if o.HTTPSPort != 0 {
		httpApp["https_port"] = o.HTTPSPort
	}
	apps := map[string]any{"http": httpApp}
	if tls := tlsApp(o, hosts); tls != nil {
		apps["tls"] = tls
	}
	cfg := map[string]any{
		"admin": map[string]any{"listen": o.admin()},
		"apps":  apps,
	}
	if o.Storage != nil {
		cfg["storage"] = map[string]any{"module": "aetherix", "endpoint": o.Storage.Endpoint, "token": o.Storage.Token}
	}
	return cfg, summary
}

func orInt(v, def int) int {
	if v == 0 {
		return def
	}
	return v
}

func hostRoute(hosts, dials []string, health map[string]any, tlsUpstream bool) map[string]any {
	route := map[string]any{"match": []any{map[string]any{"host": hosts}}, "terminal": true}
	if len(dials) == 0 {
		route["handle"] = []any{map[string]any{"handler": "static_response", "status_code": 503,
			"headers": map[string]any{"Content-Type": []string{"text/plain; charset=utf-8"}, "Retry-After": []string{"5"}},
			"body":    "No healthy replicas are available for this service."}}
		return route
	}
	ups := make([]any, len(dials))
	for i, d := range dials {
		ups[i] = map[string]any{"dial": d}
	}
	rp := map[string]any{
		"handler":   "reverse_proxy",
		"upstreams": ups,
		"load_balancing": map[string]any{
			"selection_policy": map[string]any{"policy": "round_robin"},
			"try_duration":     "5s", "try_interval": "250ms",
		},
		"health_checks": map[string]any{"passive": map[string]any{"fail_duration": "30s", "max_fails": 3}},
	}
	if health != nil {
		rp["health_checks"].(map[string]any)["active"] = health
	}
	if tlsUpstream {
		rp["transport"] = map[string]any{"protocol": "http", "tls": map[string]any{"insecure_skip_verify": true}}
	}
	route["handle"] = []any{rp}
	return route
}

func covered(host, zone string) bool {
	zone = strings.Trim(strings.ToLower(zone), ".")
	host = strings.ToLower(host)
	if !strings.HasSuffix(host, "."+zone) {
		return false
	}
	return !strings.Contains(strings.TrimSuffix(host, "."+zone), ".")
}

func tlsApp(o Options, hosts []string) map[string]any {
	if o.Dev {
		return map[string]any{"automation": map[string]any{"policies": []any{
			map[string]any{"issuers": []any{map[string]any{"module": "internal"}}},
		}}}
	}
	acme := map[string]any{"module": "acme"}
	if o.ACMEEmail != "" {
		acme["email"] = o.ACMEEmail
	}
	if o.ACMECA != "" {
		acme["ca"] = o.ACMECA
	}
	var policies []any
	tlsCfg := map[string]any{}
	if o.DNS01 != nil && o.DNS01.APIToken != "" {
		prov := o.DNS01.Provider
		if prov == "" {
			prov = "cloudflare"
		}
		var subjects []string
		for _, z := range o.DNS01.Wildcards {
			z = strings.Trim(strings.ToLower(z), ".")
			for _, h := range hosts {
				if covered(h, z) {
					subjects = append(subjects, "*."+z)
					break
				}
			}
		}
		if len(subjects) > 0 {
			issuer := map[string]any{"module": "acme", "challenges": map[string]any{"dns": map[string]any{
				"provider": map[string]any{"name": prov, "api_token": o.DNS01.APIToken}}}}
			if o.ACMEEmail != "" {
				issuer["email"] = o.ACMEEmail
			}
			if o.ACMECA != "" {
				issuer["ca"] = o.ACMECA
			}
			policies = append(policies, map[string]any{"subjects": subjects, "issuers": []any{issuer}})
			tlsCfg["certificates"] = map[string]any{"automate": subjects}
		}
	}
	policies = append(policies, map[string]any{"issuers": []any{acme}})
	tlsCfg["automation"] = map[string]any{"policies": policies}
	return tlsCfg
}

// String renders a short description of a route.
func (r Route) String() string {
	return fmt.Sprintf("%s -> %s", r.Hostname, strings.Join(r.Upstreams, ","))
}
