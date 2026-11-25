package runtime

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
	"github.com/docker/go-connections/nat"
)

// Docker implements Runtime on the local Docker engine.
type Docker struct {
	cli *client.Client
}

// NewDocker connects using the standard environment (DOCKER_HOST, ...).
func NewDocker() (*Docker, error) {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, err
	}
	return &Docker{cli: cli}, nil
}

// Client exposes the underlying client (devcluster, build pipeline).
func (d *Docker) Client() *client.Client { return d.cli }

func (d *Docker) Close() error { return d.cli.Close() }

func notFound(err error) error {
	if err != nil && client.IsErrNotFound(err) {
		return fmt.Errorf("%w: %v", ErrNotFound, err)
	}
	return err
}

func (d *Docker) EnsureNetwork(ctx context.Context, spec NetworkSpec) error {
	nets, err := d.cli.NetworkList(ctx, network.ListOptions{Filters: filters.NewArgs(filters.Arg("name", spec.Name))})
	if err != nil {
		return err
	}
	for _, n := range nets {
		if n.Name == spec.Name {
			return nil
		}
	}
	cfg := network.IPAMConfig{Subnet: spec.Subnet, Gateway: spec.Gateway, IPRange: spec.IPRange}
	_, err = d.cli.NetworkCreate(ctx, spec.Name, network.CreateOptions{
		Driver: "bridge",
		IPAM:   &network.IPAM{Driver: "default", Config: []network.IPAMConfig{cfg}},
		Labels: map[string]string{LabelManaged: "true"},
	})
	return err
}

func labelFilters(labels map[string]string) filters.Args {
	f := filters.NewArgs()
	for k, v := range labels {
		f.Add("label", k+"="+v)
	}
	return f
}

func toContainer(c container.Summary) Container {
	out := Container{ID: c.ID, Image: c.Image, Labels: c.Labels, State: c.State}
	if len(c.Names) > 0 {
		out.Name = strings.TrimPrefix(c.Names[0], "/")
	}
	if c.NetworkSettings != nil {
		if n, ok := c.NetworkSettings.Networks[NetworkName]; ok {
			out.IP = n.IPAddress
		}
	}
	// Status text carries health: "Up 3 minutes (healthy)".
	switch {
	case strings.Contains(c.Status, "(healthy)"):
		out.Health = "healthy"
	case strings.Contains(c.Status, "(unhealthy)"):
		out.Health = "unhealthy"
	case strings.Contains(c.Status, "(health: starting)"):
		out.Health = "starting"
	}
	return out
}

func (d *Docker) ListContainers(ctx context.Context, labels map[string]string) ([]Container, error) {
	list, err := d.cli.ContainerList(ctx, container.ListOptions{All: true, Filters: labelFilters(labels)})
	if err != nil {
		return nil, err
	}
	out := make([]Container, 0, len(list))
	for _, c := range list {
		out = append(out, toContainer(c))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (d *Docker) InspectContainer(ctx context.Context, id string) (Container, error) {
	j, err := d.cli.ContainerInspect(ctx, id)
	if err != nil {
		return Container{}, notFound(err)
	}
	c := Container{ID: j.ID, Name: strings.TrimPrefix(j.Name, "/"), Image: j.Config.Image, Labels: j.Config.Labels, Restarts: j.RestartCount}
	if j.State != nil {
		c.State, c.ExitCode = j.State.Status, j.State.ExitCode
		if t, err := time.Parse(time.RFC3339Nano, j.State.StartedAt); err == nil {
			c.StartedAt = t
		}
		if j.State.Health != nil {
			c.Health = j.State.Health.Status
		}
	}
	if j.NetworkSettings != nil {
		if n, ok := j.NetworkSettings.Networks[NetworkName]; ok {
			c.IP = n.IPAddress
		}
	}
	return c, nil
}

func (d *Docker) PullImage(ctx context.Context, ref string) error {
	rc, err := d.cli.ImagePull(ctx, ref, image.PullOptions{})
	if err != nil {
		return err
	}
	defer rc.Close()
	_, err = io.Copy(io.Discard, rc)
	return err
}

func (d *Docker) ImageExists(ctx context.Context, ref string) (bool, error) {
	_, err := d.cli.ImageInspect(ctx, ref)
	if err == nil {
		return true, nil
	}
	if client.IsErrNotFound(err) {
		return false, nil
	}
	return false, err
}

func (d *Docker) TagImage(ctx context.Context, src, dst string) error {
	return notFound(d.cli.ImageTag(ctx, src, dst))
}

func (d *Docker) CreateContainer(ctx context.Context, s ContainerSpec) (string, error) {
	var env []string
	keys := make([]string, 0, len(s.Env))
	for k := range s.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		env = append(env, k+"="+s.Env[k])
	}
	exposed := nat.PortSet{}
	bindings := nat.PortMap{}
	for _, p := range s.Ports {
		proto := p.Protocol
		if proto == "" {
			proto = "tcp"
		}
		port := nat.Port(fmt.Sprintf("%d/%s", p.ContainerPort, proto))
		exposed[port] = struct{}{}
		if p.HostPort > 0 {
			bindings[port] = append(bindings[port], nat.PortBinding{HostIP: p.HostIP, HostPort: strconv.Itoa(p.HostPort)})
		}
	}
	var mounts []mount.Mount
	for _, m := range s.Mounts {
		t := mount.TypeVolume
		if strings.HasPrefix(m.Source, "/") {
			t = mount.TypeBind
		} else if err := d.ensureVolume(ctx, m.Source, m.SizeBytes); err != nil {
			return "", fmt.Errorf("ensure volume %s: %w", m.Source, err)
		}
		mounts = append(mounts, mount.Mount{Type: t, Source: m.Source, Target: m.Target, ReadOnly: m.ReadOnly})
	}
	cfg := &container.Config{
		Image: s.Image, Cmd: s.Command, Entrypoint: s.Entrypoint, Env: env, Labels: s.Labels,
		ExposedPorts: exposed, User: s.User, WorkingDir: s.WorkingDir, Hostname: s.Hostname,
	}
	if s.StopTimeout > 0 {
		sec := int(s.StopTimeout.Seconds())
		cfg.StopTimeout = &sec
	}
	if s.Health != nil && len(s.Health.Command) > 0 {
		cfg.Healthcheck = &container.HealthConfig{
			Test:     append([]string{"CMD"}, s.Health.Command...),
			Interval: s.Health.Interval, Timeout: s.Health.Timeout, Retries: s.Health.Retries, StartPeriod: s.Health.StartPeriod,
		}
	}
	host := &container.HostConfig{
		PortBindings: bindings, Mounts: mounts, DNS: s.DNS,
		RestartPolicy: container.RestartPolicy{Name: container.RestartPolicyMode(restartName(s.RestartPolicy))},
		Resources: container.Resources{
			NanoCPUs: s.CPUMillis * 1_000_000, Memory: s.MemoryBytes,
		},
	}
	if s.RestartPolicy == "on-failure" {
		host.RestartPolicy.MaximumRetryCount = 5
	}
	netName := s.Network
	if netName == "" {
		netName = NetworkName
	}
	host.NetworkMode = container.NetworkMode(netName)
	nc := &network.NetworkingConfig{EndpointsConfig: map[string]*network.EndpointSettings{netName: {}}}
	resp, err := d.cli.ContainerCreate(ctx, cfg, host, nc, nil, s.Name)
	if err != nil {
		return "", err
	}
	return resp.ID, nil
}

// ensureVolume creates a named Docker volume if missing. When sizeBytes > 0,
// the local driver's "size" option is set (enforced only on hosts with quota
// support). Existing volumes are left unchanged — Docker cannot resize them.
func (d *Docker) ensureVolume(ctx context.Context, name string, sizeBytes int64) error {
	_, err := d.cli.VolumeInspect(ctx, name)
	if err == nil {
		return nil
	}
	if !client.IsErrNotFound(err) {
		return err
	}
	opts := volume.CreateOptions{
		Name:   name,
		Labels: map[string]string{LabelManaged: "true"},
	}
	if sizeBytes > 0 {
		opts.DriverOpts = map[string]string{"size": formatDockerSize(sizeBytes)}
		opts.Labels["aetherix.size_bytes"] = strconv.FormatInt(sizeBytes, 10)
	}
	_, err = d.cli.VolumeCreate(ctx, opts)
	return err
}

// formatDockerSize converts bytes to a Docker local-driver size string (e.g. "10G").
func formatDockerSize(n int64) string {
	const (
		ki = 1024
		mi = 1024 * ki
		gi = 1024 * mi
		ti = 1024 * gi
	)
	switch {
	case n >= ti && n%ti == 0:
		return fmt.Sprintf("%dT", n/ti)
	case n >= gi && n%gi == 0:
		return fmt.Sprintf("%dG", n/gi)
	case n >= mi && n%mi == 0:
		return fmt.Sprintf("%dM", n/mi)
	case n >= ki && n%ki == 0:
		return fmt.Sprintf("%dK", n/ki)
	default:
		return fmt.Sprintf("%dB", n)
	}
}

func restartName(p string) string {
	switch p {
	case "", "always":
		return "unless-stopped"
	case "no":
		return "no"
	default:
		return p
	}
}

func (d *Docker) StartContainer(ctx context.Context, id string) error {
	return notFound(d.cli.ContainerStart(ctx, id, container.StartOptions{}))
}

func (d *Docker) StopContainer(ctx context.Context, id string, timeout time.Duration) error {
	sec := int(timeout.Seconds())
	return notFound(d.cli.ContainerStop(ctx, id, container.StopOptions{Timeout: &sec}))
}

func (d *Docker) RemoveContainer(ctx context.Context, id string, force bool) error {
	return notFound(d.cli.ContainerRemove(ctx, id, container.RemoveOptions{Force: force, RemoveVolumes: false}))
}

func (d *Docker) StreamLogs(ctx context.Context, id string, o LogOptions) (<-chan LogLine, error) {
	j, err := d.cli.ContainerInspect(ctx, id)
	if err != nil {
		return nil, notFound(err)
	}
	tail := "100"
	switch {
	case o.Tail < 0:
		tail = "all"
	case o.Tail > 0:
		tail = strconv.Itoa(o.Tail)
	}
	opts := container.LogsOptions{ShowStdout: true, ShowStderr: true, Follow: o.Follow, Tail: tail, Timestamps: true}
	if !o.Since.IsZero() {
		opts.Since = o.Since.Format(time.RFC3339Nano)
	}
	rc, err := d.cli.ContainerLogs(ctx, id, opts)
	if err != nil {
		return nil, notFound(err)
	}
	out := make(chan LogLine, 128)
	pump := func(r io.Reader, stream string, done chan<- struct{}) {
		defer close(done)
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 64<<10), 4<<20)
		for sc.Scan() {
			ll := parseLogLine(sc.Text(), stream)
			select {
			case out <- ll:
			case <-ctx.Done():
				return
			}
		}
	}
	go func() {
		defer close(out)
		defer rc.Close()
		if j.Config != nil && j.Config.Tty {
			done := make(chan struct{})
			go pump(rc, "stdout", done)
			<-done
			return
		}
		or, ow := io.Pipe()
		er, ew := io.Pipe()
		d1, d2 := make(chan struct{}), make(chan struct{})
		go pump(or, "stdout", d1)
		go pump(er, "stderr", d2)
		_, _ = stdcopy.StdCopy(ow, ew, rc)
		ow.Close()
		ew.Close()
		<-d1
		<-d2
	}()
	return out, nil
}

// parseLogLine splits the RFC3339Nano timestamp Docker prepends.
func parseLogLine(s, stream string) LogLine {
	ll := LogLine{Stream: stream, Line: s}
	if i := strings.IndexByte(s, ' '); i > 0 {
		if t, err := time.Parse(time.RFC3339Nano, s[:i]); err == nil {
			ll.Time, ll.Line = t, s[i+1:]
		}
	}
	return ll
}

type dockerExec struct {
	d    *Docker
	id   string
	hj   types.HijackedResponse
	tty  bool
	outR *io.PipeReader
	errR *io.PipeReader
}

func (d *Docker) Exec(ctx context.Context, id string, o ExecOptions) (ExecSession, error) {
	ex, err := d.cli.ContainerExecCreate(ctx, id, container.ExecOptions{
		Cmd: o.Command, Env: o.Env, Tty: o.TTY, AttachStdin: o.Stdin, AttachStdout: true, AttachStderr: true,
		ConsoleSize: consoleSize(o),
	})
	if err != nil {
		return nil, notFound(err)
	}
	hj, err := d.cli.ContainerExecAttach(ctx, ex.ID, container.ExecAttachOptions{Tty: o.TTY, ConsoleSize: consoleSize(o)})
	if err != nil {
		return nil, err
	}
	s := &dockerExec{d: d, id: ex.ID, hj: hj, tty: o.TTY}
	if o.TTY {
		s.outR, _ = io.Pipe() // unused; TTY reads straight from the connection
		return s, nil
	}
	var ow, ew *io.PipeWriter
	s.outR, ow = io.Pipe()
	s.errR, ew = io.Pipe()
	go func() {
		_, _ = stdcopy.StdCopy(ow, ew, hj.Reader)
		ow.Close()
		ew.Close()
	}()
	return s, nil
}

func consoleSize(o ExecOptions) *[2]uint {
	if o.Rows > 0 && o.Cols > 0 {
		return &[2]uint{uint(o.Rows), uint(o.Cols)}
	}
	return nil
}

func (e *dockerExec) Stdin() io.WriteCloser { return closeWriter{e.hj} }
func (e *dockerExec) Stdout() io.Reader {
	if e.tty {
		return e.hj.Reader
	}
	return e.outR
}
func (e *dockerExec) Stderr() io.Reader {
	if e.tty {
		return nil
	}
	return e.errR
}
func (e *dockerExec) Resize(ctx context.Context, rows, cols int) error {
	return e.d.cli.ContainerExecResize(ctx, e.id, container.ResizeOptions{Height: uint(rows), Width: uint(cols)})
}
func (e *dockerExec) Wait(ctx context.Context) (int, error) {
	for {
		ins, err := e.d.cli.ContainerExecInspect(ctx, e.id)
		if err != nil {
			return -1, err
		}
		if !ins.Running {
			return ins.ExitCode, nil
		}
		select {
		case <-ctx.Done():
			return -1, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}
func (e *dockerExec) Close() error { e.hj.Close(); return nil }

type closeWriter struct{ hj types.HijackedResponse }

func (c closeWriter) Write(p []byte) (int, error) { return c.hj.Conn.Write(p) }
func (c closeWriter) Close() error                { return c.hj.CloseWrite() }

var _ Runtime = (*Docker)(nil)
var _ = errors.New
