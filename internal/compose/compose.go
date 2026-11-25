// Package compose parses compose.yaml (Compose Specification subset) with
// AetherixCloud x- extensions into ServiceSpecs.
//
// Supported service keys: image, build, command, entrypoint, environment,
// ports, expose, volumes, depends_on, restart, healthcheck, labels, user,
// working_dir, stop_grace_period, deploy.{mode,replicas,resources,placement.
// constraints,update_config.order}.
//
// Extensions (per service):
//
//	x-ports:         ["[hostname:]port[/https|http|tcp|udp][@host]", ...]
//	                 routed through ingress (http/https) or published (tcp/udp)
//	x-machines:      [name|id, ...]  or "a,b"   -> placement.machines
//	x-labels:        {k: v}                      -> placement.labels (machine labels)
//	x-reschedulable: bool
//	x-build:         {builder, context, dockerfile, target, build_command,
//	                  start_command, args, watch_paths, platform}
//	x-deploy:        {strategy: start-first|stop-first, max_surge: N}
//	x-global:        bool (same as deploy.mode: global)
//
// Top-level volumes may set `x-persistence: backup-restore` and `x-size: 10G`.
// ${VAR}, ${VAR:-d}, ${VAR-d}, ${VAR:?msg} and $$ interpolation is applied
// before parsing.
package compose

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	v1 "github.com/aetherix/aetherixcloud/gen/aetherix/v1"
	"gopkg.in/yaml.v3"
)

// Project is a parsed compose file.
type Project struct {
	Name     string
	Services []*Service // sorted by dependency order, then name
	Warnings []string
}

// Service is one compose service.
type Service struct {
	Name      string
	Spec      *v1.ServiceSpec
	DependsOn []string
}

type file struct {
	Name     string                    `yaml:"name"`
	Services map[string]*serviceDef    `yaml:"services"`
	Volumes  map[string]map[string]any `yaml:"volumes"`
	Extra    map[string]any            `yaml:",inline"`
}

type serviceDef struct {
	Image           string            `yaml:"image"`
	Build           any               `yaml:"build"`
	Command         any               `yaml:"command"`
	Entrypoint      any               `yaml:"entrypoint"`
	Environment     any               `yaml:"environment"`
	Ports           []any             `yaml:"ports"`
	Expose          []any             `yaml:"expose"`
	Volumes         []any             `yaml:"volumes"`
	DependsOn       any               `yaml:"depends_on"`
	Restart         string            `yaml:"restart"`
	Healthcheck     *healthDef        `yaml:"healthcheck"`
	Labels          any               `yaml:"labels"`
	User            string            `yaml:"user"`
	WorkingDir      string            `yaml:"working_dir"`
	StopGracePeriod string            `yaml:"stop_grace_period"`
	Deploy          *deployDef        `yaml:"deploy"`
	XPorts          []string          `yaml:"x-ports"`
	XMachines       any               `yaml:"x-machines"`
	XLabels         map[string]string `yaml:"x-labels"`
	XReschedulable  bool              `yaml:"x-reschedulable"`
	XBuild          map[string]any    `yaml:"x-build"`
	XDeploy         struct {
		Strategy string `yaml:"strategy"`
		MaxSurge int    `yaml:"max_surge"`
	} `yaml:"x-deploy"`
	XGlobal bool           `yaml:"x-global"`
	Extra   map[string]any `yaml:",inline"`
}

type healthDef struct {
	Test        any    `yaml:"test"`
	Interval    string `yaml:"interval"`
	Timeout     string `yaml:"timeout"`
	Retries     int    `yaml:"retries"`
	StartPeriod string `yaml:"start_period"`
	Disable     bool   `yaml:"disable"`
}

type deployDef struct {
	Mode      string `yaml:"mode"`
	Replicas  *int   `yaml:"replicas"`
	Resources struct {
		Limits struct {
			CPUs   any `yaml:"cpus"`
			Memory any `yaml:"memory"`
		} `yaml:"limits"`
	} `yaml:"resources"`
	Placement struct {
		Constraints []string `yaml:"constraints"`
	} `yaml:"placement"`
	UpdateConfig struct {
		Order string `yaml:"order"`
	} `yaml:"update_config"`
}

// Parse parses compose YAML. vars supplies interpolation values (typically
// from the environment / variables UI).
func Parse(data []byte, vars map[string]string) (*Project, error) {
	text, err := Interpolate(string(data), vars)
	if err != nil {
		return nil, err
	}
	var f file
	if err := yaml.Unmarshal([]byte(text), &f); err != nil {
		return nil, fmt.Errorf("compose: invalid yaml: %w", err)
	}
	if len(f.Services) == 0 {
		return nil, fmt.Errorf("compose: no services defined")
	}
	p := &Project{Name: f.Name}
	volPersist := map[string]string{}
	volSize := map[string]int64{}
	for name, v := range f.Volumes {
		if s, ok := v["x-persistence"].(string); ok {
			volPersist[name] = s
		}
		if s, ok := v["x-size"].(string); ok {
			if n, err := ParseBytes(s); err == nil {
				volSize[name] = n
			}
		}
	}
	for name, def := range f.Services {
		if def == nil {
			return nil, fmt.Errorf("compose: service %q is empty", name)
		}
		svc, warns, err := convert(name, def, volPersist, volSize, vars)
		if err != nil {
			return nil, fmt.Errorf("compose: service %q: %w", name, err)
		}
		p.Warnings = append(p.Warnings, warns...)
		p.Services = append(p.Services, svc)
	}
	if err := order(p); err != nil {
		return nil, err
	}
	sort.Strings(p.Warnings)
	return p, nil
}

// order sorts services topologically by depends_on (stable by name).
func order(p *Project) error {
	byName := map[string]*Service{}
	var names []string
	for _, s := range p.Services {
		byName[s.Name] = s
		names = append(names, s.Name)
	}
	sort.Strings(names)
	var out []*Service
	state := map[string]int{}
	var visit func(string, []string) error
	visit = func(n string, path []string) error {
		switch state[n] {
		case 2:
			return nil
		case 1:
			return fmt.Errorf("compose: dependency cycle: %s", strings.Join(append(path, n), " -> "))
		}
		state[n] = 1
		s := byName[n]
		deps := append([]string(nil), s.DependsOn...)
		sort.Strings(deps)
		for _, d := range deps {
			if _, ok := byName[d]; !ok {
				return fmt.Errorf("compose: service %q depends on unknown service %q", n, d)
			}
			if err := visit(d, append(path, n)); err != nil {
				return err
			}
		}
		state[n] = 2
		out = append(out, s)
		return nil
	}
	for _, n := range names {
		if err := visit(n, nil); err != nil {
			return err
		}
	}
	p.Services = out
	return nil
}

func convert(name string, d *serviceDef, volPersist map[string]string, volSize map[string]int64, vars map[string]string) (*Service, []string, error) {
	var warns []string
	spec := &v1.ServiceSpec{
		Image:         d.Image,
		Mode:          v1.ServiceMode_SERVICE_MODE_REPLICATED,
		Replicas:      1,
		Reschedulable: d.XReschedulable,
		User:          d.User,
		WorkingDir:    d.WorkingDir,
		RestartPolicy: normaliseRestart(d.Restart),
		Strategy:      v1.DeployStrategy_DEPLOY_STRATEGY_START_FIRST,
	}
	if d.Image == "" && d.Build == nil && d.XBuild == nil {
		return nil, nil, fmt.Errorf("one of image or build is required")
	}
	for k := range d.Extra {
		if strings.HasPrefix(k, "x-") {
			continue
		}
		warns = append(warns, fmt.Sprintf("service %q: unsupported key %q ignored", name, k))
	}

	// build
	if d.Build != nil || d.XBuild != nil {
		b := &v1.BuildSpec{}
		switch bv := d.Build.(type) {
		case string:
			b.Context = bv
		case map[string]any:
			b.Context, _ = bv["context"].(string)
			b.Dockerfile, _ = bv["dockerfile"].(string)
			b.Target, _ = bv["target"].(string)
			b.Args = stringMap(bv["args"])
			if pl, ok := bv["platforms"].([]any); ok && len(pl) > 0 {
				b.Platform = fmt.Sprint(pl[0])
			}
		}
		if x := d.XBuild; x != nil {
			setStr := func(k string, dst *string) {
				if s, ok := x[k].(string); ok {
					*dst = s
				}
			}
			setStr("builder", &b.Builder)
			setStr("context", &b.Context)
			setStr("dockerfile", &b.Dockerfile)
			setStr("target", &b.Target)
			setStr("build_command", &b.BuildCommand)
			setStr("start_command", &b.StartCommand)
			setStr("platform", &b.Platform)
			if a := stringMap(x["args"]); len(a) > 0 {
				b.Args = a
			}
			if w, ok := x["watch_paths"].([]any); ok {
				for _, e := range w {
					b.WatchPaths = append(b.WatchPaths, fmt.Sprint(e))
				}
			}
		}
		spec.Build = b
		if d.Image == "" {
			// image is assigned by the build pipeline
			spec.Source = &v1.ServiceSource{}
		}
	}
	if d.Image != "" {
		spec.Source = &v1.ServiceSource{Image: d.Image}
	}

	var err error
	if spec.Command, err = stringList(d.Command); err != nil {
		return nil, nil, fmt.Errorf("command: %w", err)
	}
	if spec.Entrypoint, err = stringList(d.Entrypoint); err != nil {
		return nil, nil, fmt.Errorf("entrypoint: %w", err)
	}
	if spec.Env, err = envMap(d.Environment, vars); err != nil {
		return nil, nil, fmt.Errorf("environment: %w", err)
	}
	spec.Labels = labelMap(d.Labels)

	// ports: compose `ports` publish on the host; `expose` is internal.
	for _, p := range d.Ports {
		sp, err := parsePort(p)
		if err != nil {
			return nil, nil, fmt.Errorf("ports: %w", err)
		}
		spec.Ports = append(spec.Ports, sp)
	}
	for _, e := range d.Expose {
		n, err := strconv.Atoi(strings.TrimSpace(fmt.Sprint(e)))
		if err != nil {
			return nil, nil, fmt.Errorf("expose: %v", e)
		}
		spec.Ports = append(spec.Ports, &v1.ServicePort{ContainerPort: int32(n), Protocol: v1.PortProtocol_PORT_PROTOCOL_TCP, Mode: "internal"})
	}
	for _, xp := range d.XPorts {
		sp, err := ParseXPort(xp)
		if err != nil {
			return nil, nil, fmt.Errorf("x-ports: %w", err)
		}
		spec.Ports = append(spec.Ports, sp)
	}

	for _, v := range d.Volumes {
		vm, err := parseVolume(v)
		if err != nil {
			return nil, nil, fmt.Errorf("volumes: %w", err)
		}
		if per, ok := volPersist[vm.Source]; ok {
			vm.Persistence = per
		}
		if sz, ok := volSize[vm.Source]; ok {
			vm.SizeBytes = sz
		}
		spec.Volumes = append(spec.Volumes, vm)
	}

	// health check
	if h := d.Healthcheck; h != nil && !h.Disable {
		hc := &v1.HealthCheck{Retries: int32(h.Retries)}
		var cmd []string
		switch t := h.Test.(type) {
		case string:
			cmd = []string{"sh", "-c", t}
		case []any:
			var items []string
			for _, e := range t {
				items = append(items, fmt.Sprint(e))
			}
			switch {
			case len(items) == 0 || items[0] == "NONE":
			case items[0] == "CMD":
				cmd = items[1:]
			case items[0] == "CMD-SHELL":
				cmd = []string{"sh", "-c", strings.Join(items[1:], " ")}
			default:
				cmd = items
			}
		}
		hc.Command = cmd
		{
			hc.IntervalSeconds = secs(h.Interval)
			hc.TimeoutSeconds = secs(h.Timeout)
			hc.StartPeriodSeconds = secs(h.StartPeriod)
		}
		if len(cmd) > 0 {
			spec.HealthCheck = hc
		}
	}
	if d.StopGracePeriod != "" {
		spec.StopGraceSeconds = secs(d.StopGracePeriod)
	}

	// deploy
	if dp := d.Deploy; dp != nil {
		if dp.Mode == "global" {
			spec.Mode = v1.ServiceMode_SERVICE_MODE_GLOBAL
		}
		if dp.Replicas != nil {
			spec.Replicas = int32(*dp.Replicas)
		}
		if c := dp.Resources.Limits.CPUs; c != nil {
			f, err := strconv.ParseFloat(fmt.Sprint(c), 64)
			if err != nil {
				return nil, nil, fmt.Errorf("deploy.resources.limits.cpus: %v", c)
			}
			spec.Resources = ensureRes(spec.Resources)
			spec.Resources.CpuMillis = int64(f * 1000)
		}
		if m := dp.Resources.Limits.Memory; m != nil {
			b, err := ParseBytes(fmt.Sprint(m))
			if err != nil {
				return nil, nil, fmt.Errorf("deploy.resources.limits.memory: %w", err)
			}
			spec.Resources = ensureRes(spec.Resources)
			spec.Resources.MemoryBytes = b
		}
		for _, c := range dp.Placement.Constraints {
			// node.hostname == x | node.labels.k == v
			c = strings.TrimSpace(c)
			parts := strings.SplitN(c, "==", 2)
			if len(parts) != 2 {
				warns = append(warns, fmt.Sprintf("service %q: placement constraint %q ignored", name, c))
				continue
			}
			k, v := strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
			ensurePlacement(spec)
			switch {
			case k == "node.hostname":
				spec.Placement.Machines = append(spec.Placement.Machines, v)
			case strings.HasPrefix(k, "node.labels."):
				spec.Placement.Labels[strings.TrimPrefix(k, "node.labels.")] = v
			default:
				warns = append(warns, fmt.Sprintf("service %q: placement constraint %q ignored", name, c))
			}
		}
		switch dp.UpdateConfig.Order {
		case "stop-first":
			spec.Strategy = v1.DeployStrategy_DEPLOY_STRATEGY_STOP_FIRST
		case "start-first":
			spec.Strategy = v1.DeployStrategy_DEPLOY_STRATEGY_START_FIRST
		}
	}
	if d.XGlobal {
		spec.Mode = v1.ServiceMode_SERVICE_MODE_GLOBAL
	}
	switch d.XDeploy.Strategy {
	case "stop-first":
		spec.Strategy = v1.DeployStrategy_DEPLOY_STRATEGY_STOP_FIRST
	case "start-first":
		spec.Strategy = v1.DeployStrategy_DEPLOY_STRATEGY_START_FIRST
	case "":
	default:
		return nil, nil, fmt.Errorf("x-deploy.strategy: %q must be start-first or stop-first", d.XDeploy.Strategy)
	}
	spec.MaxSurge = int32(d.XDeploy.MaxSurge)

	// placement extensions
	if ms, err := stringList(d.XMachines); err != nil {
		return nil, nil, fmt.Errorf("x-machines: %w", err)
	} else if len(ms) > 0 {
		var all []string
		for _, m := range ms {
			for _, s := range strings.Split(m, ",") {
				if s = strings.TrimSpace(s); s != "" {
					all = append(all, s)
				}
			}
		}
		ensurePlacement(spec)
		spec.Placement.Machines = append(spec.Placement.Machines, all...)
	}
	if len(d.XLabels) > 0 {
		ensurePlacement(spec)
		for k, v := range d.XLabels {
			spec.Placement.Labels[k] = v
		}
	}

	if spec.Mode == v1.ServiceMode_SERVICE_MODE_GLOBAL {
		spec.Replicas = 0
	}
	// Exclusive local volumes: default to stop-first unless set.
	if len(spec.Volumes) > 0 && d.XDeploy.Strategy == "" && (d.Deploy == nil || d.Deploy.UpdateConfig.Order == "") {
		spec.Strategy = v1.DeployStrategy_DEPLOY_STRATEGY_STOP_FIRST
	}

	svc := &Service{Name: name, Spec: spec}
	switch dv := d.DependsOn.(type) {
	case []any:
		for _, e := range dv {
			svc.DependsOn = append(svc.DependsOn, fmt.Sprint(e))
		}
	case map[string]any:
		for k := range dv {
			svc.DependsOn = append(svc.DependsOn, k)
		}
	}
	sort.Strings(svc.DependsOn)
	spec.DependsOn = svc.DependsOn
	return svc, warns, nil
}

func ensureRes(r *v1.Resources) *v1.Resources {
	if r == nil {
		return &v1.Resources{}
	}
	return r
}

func ensurePlacement(s *v1.ServiceSpec) {
	if s.Placement == nil {
		s.Placement = &v1.Placement{}
	}
	if s.Placement.Labels == nil {
		s.Placement.Labels = map[string]string{}
	}
}

func normaliseRestart(r string) string {
	switch {
	case r == "" || r == "always" || r == "unless-stopped":
		return "always"
	case r == "no" || r == "false":
		return "no"
	case strings.HasPrefix(r, "on-failure"):
		return "on-failure"
	}
	return "always"
}

func secs(s string) int32 {
	if s == "" {
		return 0
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0
	}
	return int32(d.Seconds())
}

func stringMap(v any) map[string]string {
	out := map[string]string{}
	switch m := v.(type) {
	case map[string]any:
		for k, e := range m {
			if e == nil {
				out[k] = ""
			} else {
				out[k] = fmt.Sprint(e)
			}
		}
	case []any:
		for _, e := range m {
			k, val, _ := strings.Cut(fmt.Sprint(e), "=")
			out[k] = val
		}
	}
	return out
}

func labelMap(v any) map[string]string {
	m := stringMap(v)
	if len(m) == 0 {
		return nil
	}
	return m
}

func envMap(v any, vars map[string]string) (map[string]string, error) {
	out := map[string]string{}
	switch m := v.(type) {
	case nil:
		return nil, nil
	case map[string]any:
		for k, e := range m {
			if e == nil {
				if val, ok := vars[k]; ok {
					out[k] = val
				}
				continue
			}
			out[k] = fmt.Sprint(e)
		}
	case []any:
		for _, e := range m {
			s := fmt.Sprint(e)
			k, val, has := strings.Cut(s, "=")
			if !has {
				if vv, ok := vars[k]; ok {
					out[k] = vv
				}
				continue
			}
			out[k] = val
		}
	default:
		return nil, fmt.Errorf("must be a map or list")
	}
	return out, nil
}

// stringList accepts a string (shell split) or a list.
func stringList(v any) ([]string, error) {
	switch x := v.(type) {
	case nil:
		return nil, nil
	case string:
		return SplitCommand(x)
	case []any:
		out := make([]string, len(x))
		for i, e := range x {
			out[i] = fmt.Sprint(e)
		}
		return out, nil
	}
	return nil, fmt.Errorf("must be a string or list")
}

// SplitCommand splits a command line honouring single/double quotes and
// backslash escapes (no variable expansion).
func SplitCommand(s string) ([]string, error) {
	var out []string
	var cur strings.Builder
	in := false
	var quote rune
	esc := false
	for _, r := range s {
		switch {
		case esc:
			cur.WriteRune(r)
			esc = false
		case r == '\\' && quote != '\'':
			esc = true
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case r == '"' || r == '\'':
			quote, in = r, true
		case r == ' ' || r == '\t' || r == '\n':
			if in || cur.Len() > 0 {
				out = append(out, cur.String())
				cur.Reset()
				in = false
			}
		default:
			cur.WriteRune(r)
		}
	}
	if quote != 0 {
		return nil, fmt.Errorf("unterminated quote in %q", s)
	}
	if in || cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out, nil
}

// ParseBytes parses "512m", "1g", "2GB", "1024".
func ParseBytes(s string) (int64, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	mult := int64(1)
	for _, u := range []struct {
		suf string
		m   int64
	}{{"gb", 1 << 30}, {"g", 1 << 30}, {"mb", 1 << 20}, {"m", 1 << 20}, {"kb", 1 << 10}, {"k", 1 << 10}, {"b", 1}} {
		if strings.HasSuffix(s, u.suf) {
			s, mult = strings.TrimSuffix(s, u.suf), u.m
			break
		}
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return 0, fmt.Errorf("invalid size %q", s)
	}
	return int64(f * float64(mult)), nil
}

func parsePort(p any) (*v1.ServicePort, error) {
	switch x := p.(type) {
	case map[string]any:
		t, err := strconv.Atoi(fmt.Sprint(x["target"]))
		if err != nil {
			return nil, fmt.Errorf("invalid target in %v", x)
		}
		sp := &v1.ServicePort{ContainerPort: int32(t), Protocol: v1.PortProtocol_PORT_PROTOCOL_TCP, Mode: "internal"}
		if pub, ok := x["published"]; ok && pub != nil {
			n, err := strconv.Atoi(fmt.Sprint(pub))
			if err != nil {
				return nil, fmt.Errorf("invalid published in %v", x)
			}
			sp.PublishedPort, sp.Mode = int32(n), "host"
		}
		if pr, _ := x["protocol"].(string); pr == "udp" {
			sp.Protocol = v1.PortProtocol_PORT_PROTOCOL_UDP
		}
		return sp, nil
	default:
		s := fmt.Sprint(p)
		proto := v1.PortProtocol_PORT_PROTOCOL_TCP
		if i := strings.LastIndexByte(s, '/'); i >= 0 {
			if s[i+1:] == "udp" {
				proto = v1.PortProtocol_PORT_PROTOCOL_UDP
			}
			s = s[:i]
		}
		parts := strings.Split(s, ":")
		sp := &v1.ServicePort{Protocol: proto, Mode: "internal"}
		last := parts[len(parts)-1]
		cp, err := strconv.Atoi(last)
		if err != nil {
			return nil, fmt.Errorf("invalid port %q", fmt.Sprint(p))
		}
		sp.ContainerPort = int32(cp)
		if len(parts) >= 2 {
			// [ip:]published:container
			pub, err := strconv.Atoi(parts[len(parts)-2])
			if err != nil {
				return nil, fmt.Errorf("invalid port %q", fmt.Sprint(p))
			}
			sp.PublishedPort, sp.Mode = int32(pub), "host"
		}
		return sp, nil
	}
}

// ParseXPort parses "[hostname:]port[/proto][@host]".
func ParseXPort(s string) (*v1.ServicePort, error) {
	orig := s
	sp := &v1.ServicePort{Protocol: v1.PortProtocol_PORT_PROTOCOL_TCP, Mode: "https"}
	host := false
	if strings.HasSuffix(s, "@host") {
		host, s = true, strings.TrimSuffix(s, "@host")
	}
	if i := strings.LastIndexByte(s, '/'); i >= 0 {
		switch pr := s[i+1:]; pr {
		case "http", "https":
			sp.Mode = pr
		case "tcp":
			sp.Mode = "tcp" // TCP proxy on ingress nodes
		case "udp":
			sp.Mode, sp.Protocol = "udp", v1.PortProtocol_PORT_PROTOCOL_UDP
		default:
			return nil, fmt.Errorf("%q: unknown protocol %q", orig, pr)
		}
		s = s[:i]
	}
	if i := strings.LastIndexByte(s, ':'); i >= 0 {
		sp.Hostname, s = s[:i], s[i+1:]
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 || n > 65535 {
		return nil, fmt.Errorf("%q: invalid port", orig)
	}
	sp.ContainerPort = int32(n)
	if host {
		sp.Mode, sp.PublishedPort = "host", int32(n)
	}
	// mode "tcp": PublishedPort stays 0 so the API assigns a random cluster-wide port.
	return sp, nil
}

func parseVolume(v any) (*v1.VolumeMount, error) {
	switch x := v.(type) {
	case map[string]any:
		vm := &v1.VolumeMount{}
		vm.Source, _ = x["source"].(string)
		vm.Target, _ = x["target"].(string)
		vm.ReadOnly, _ = x["read_only"].(bool)
		if vm.Target == "" {
			return nil, fmt.Errorf("missing target in %v", x)
		}
		return vm, nil
	default:
		parts := strings.Split(fmt.Sprint(v), ":")
		switch len(parts) {
		case 1:
			return nil, fmt.Errorf("anonymous volume %q not supported; give it a name", parts[0])
		case 2, 3:
			vm := &v1.VolumeMount{Source: parts[0], Target: parts[1]}
			if len(parts) == 3 {
				vm.ReadOnly = strings.Contains(parts[2], "ro")
			}
			return vm, nil
		}
		return nil, fmt.Errorf("invalid volume %q", fmt.Sprint(v))
	}
}
