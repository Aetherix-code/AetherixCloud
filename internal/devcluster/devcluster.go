// Package devcluster runs a local multi-node AetherixCloud cluster where each
// "machine" is a privileged Docker container (Docker-in-Docker) running the
// production binaries (see deploy/dev/Dockerfile.node). It backs `ax dev`
// and is meant to be reused by the integration-test harness.
//
// Topology: user-defined bridge network "ax-dev" (172.28.0.0/16); node N has
// the fixed underlay IP 172.28.0.(10+N) and name "node-N" (container
// "ax-node-N"). Node 1 initialises the cluster; the others join with a token.
// Node 1 publishes 80/443 (ingress) and its API on the host.
package devcluster

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"connectrpc.com/connect"
	v1 "github.com/aetherix/aetherixcloud/gen/aetherix/v1"
	"github.com/aetherix/aetherixcloud/gen/aetherix/v1/aetherixv1connect"
	"github.com/aetherix/aetherixcloud/internal/pki"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
	"github.com/docker/go-connections/nat"
)

// Defaults.
const (
	DefaultImage   = "aetherix/node:dev"
	NetworkName    = "ax-dev"
	NetworkSubnet  = "172.28.0.0/16"
	ContainerLabel = "aetherix.dev"
	APIPort        = 8443
)

// Options configure Up and the other operations.
type Options struct {
	Nodes       int
	Image       string
	ClusterName string
	// Build forces an image rebuild; the image is also built when missing.
	Build bool
	// RepoDir is the repository root used as docker build context.
	RepoDir string
	// APIHostPort is the host port mapped to node-1's API (default 8443).
	APIHostPort int
	// HTTPHostPort / HTTPSHostPort are mapped to node-1's 80/443 (0 = 80/443).
	HTTPHostPort  int
	HTTPSHostPort int
	// StateFile stores the admin token and CA (default ~/.config/aetherix/dev.json).
	StateFile string
	// Out receives progress output.
	Out io.Writer
}

func (o *Options) defaults() {
	if o.Nodes == 0 {
		o.Nodes = 3
	}
	if o.Image == "" {
		o.Image = DefaultImage
	}
	if o.ClusterName == "" {
		o.ClusterName = "dev"
	}
	if o.APIHostPort == 0 {
		o.APIHostPort = APIPort
	}
	if o.HTTPHostPort == 0 {
		o.HTTPHostPort = 80
	}
	if o.HTTPSHostPort == 0 {
		o.HTTPSHostPort = 443
	}
	if o.Out == nil {
		o.Out = io.Discard
	}
	if o.StateFile == "" {
		home, _ := os.UserHomeDir()
		o.StateFile = filepath.Join(home, ".config", "aetherix", "dev.json")
	}
}

// State is persisted after `up` so later commands can talk to the cluster.
type State struct {
	APIURL     string `json:"api_url"`
	AdminToken string `json:"admin_token"`
	CAPEM      string `json:"ca_pem"`
	Image      string `json:"image"`
}

// ---- naming helpers (pure; unit tested)

// NodeName returns "node-N".
func NodeName(n int) string { return fmt.Sprintf("node-%d", n) }

// ContainerName returns "ax-node-N".
func ContainerName(n int) string { return "ax-" + NodeName(n) }

// NodeIP returns the fixed underlay IP of node n.
func NodeIP(n int) string { return fmt.Sprintf("172.28.0.%d", 10+n) }

// ParseNode accepts "node-2", "ax-node-2" or "2".
func ParseNode(s string) (int, error) {
	s = strings.TrimPrefix(strings.TrimPrefix(s, "ax-"), "node-")
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 || n > 200 {
		return 0, fmt.Errorf("invalid node %q (expected node-N)", s)
	}
	return n, nil
}

// PartitionScript returns the shell command run inside a container to drop
// all traffic with the given underlay IPs (dedicated iptables chain so heal
// is a flush).
func PartitionScript(peerIPs ...string) string {
	var b strings.Builder
	b.WriteString("iptables -N AX_PARTITION 2>/dev/null; ")
	b.WriteString("iptables -C INPUT -j AX_PARTITION 2>/dev/null || iptables -I INPUT 1 -j AX_PARTITION; ")
	b.WriteString("iptables -C OUTPUT -j AX_PARTITION 2>/dev/null || iptables -I OUTPUT 1 -j AX_PARTITION; ")
	for _, ip := range peerIPs {
		fmt.Fprintf(&b, "iptables -A AX_PARTITION -s %s -j DROP; iptables -A AX_PARTITION -d %s -j DROP; ", ip, ip)
	}
	return strings.TrimSuffix(b.String(), "; ")
}

// HealScript flushes the partition chain.
const HealScript = "iptables -F AX_PARTITION 2>/dev/null || true"

// LatencyScript adds (or with d == 0 removes) egress delay on eth0.
func LatencyScript(d time.Duration) string {
	if d <= 0 {
		return "tc qdisc del dev eth0 root 2>/dev/null || true"
	}
	return fmt.Sprintf("tc qdisc replace dev eth0 root netem delay %dms", d.Milliseconds())
}

// ---- cluster handle

// Cluster controls a dev cluster.
type Cluster struct {
	cli  *client.Client
	opts Options
}

// New connects to Docker.
func New(opts Options) (*Cluster, error) {
	opts.defaults()
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, err
	}
	return &Cluster{cli: cli, opts: opts}, nil
}

// Close releases the Docker client.
func (c *Cluster) Close() error { return c.cli.Close() }

func (c *Cluster) logf(format string, a ...any) { fmt.Fprintf(c.opts.Out, format+"\n", a...) }

// ContainerLogs returns the last approxLines of container stdout/stderr.
func (c *Cluster) ContainerLogs(ctx context.Context, name string, approxLines int) (string, error) {
	return c.containerLogs(ctx, name, approxLines)
}

// containerLogs returns the last approxLines of container stdout/stderr.
func (c *Cluster) containerLogs(ctx context.Context, name string, approxLines int) (string, error) {
	if approxLines <= 0 {
		approxLines = 50
	}
	rc, err := c.cli.ContainerLogs(ctx, name, container.LogsOptions{
		ShowStdout: true, ShowStderr: true, Tail: strconv.Itoa(approxLines),
	})
	if err != nil {
		return "", err
	}
	defer rc.Close()
	var out bytes.Buffer
	_, _ = stdcopy.StdCopy(&out, &out, rc)
	return out.String(), nil
}

// Node is a running dev node.
type Node struct {
	Index     int
	Name      string
	Container string
	IP        string
	State     string
}

// Nodes lists dev containers sorted by index.
func (c *Cluster) Nodes(ctx context.Context) ([]Node, error) {
	list, err := c.cli.ContainerList(ctx, container.ListOptions{All: true, Filters: filters.NewArgs(filters.Arg("label", ContainerLabel+"=true"))})
	if err != nil {
		return nil, err
	}
	var out []Node
	for _, ct := range list {
		idx, _ := strconv.Atoi(ct.Labels[ContainerLabel+".index"])
		out = append(out, Node{Index: idx, Name: NodeName(idx), Container: strings.TrimPrefix(ct.Names[0], "/"), IP: NodeIP(idx), State: ct.State})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Index < out[j].Index })
	return out, nil
}

// ---- image

func (c *Cluster) ensureImage(ctx context.Context) error {
	if !c.opts.Build {
		if _, err := c.cli.ImageInspect(ctx, c.opts.Image); err == nil {
			return nil
		}
	}
	dir := c.opts.RepoDir
	if dir == "" {
		dir = "."
	}
	c.logf("Building node image %s (this takes a few minutes the first time)...", c.opts.Image)
	cmd := exec.CommandContext(ctx, "docker", "build", "-t", c.opts.Image, "-f", filepath.Join(dir, "deploy/dev/Dockerfile.node"), dir)
	cmd.Stdout, cmd.Stderr = c.opts.Out, c.opts.Out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("docker build: %w", err)
	}
	return nil
}

func (c *Cluster) ensureNetwork(ctx context.Context) error {
	nets, err := c.cli.NetworkList(ctx, network.ListOptions{Filters: filters.NewArgs(filters.Arg("name", NetworkName))})
	if err != nil {
		return err
	}
	for _, n := range nets {
		if n.Name == NetworkName {
			return nil
		}
	}
	_, err = c.cli.NetworkCreate(ctx, NetworkName, network.CreateOptions{Driver: "bridge",
		IPAM:   &network.IPAM{Config: []network.IPAMConfig{{Subnet: NetworkSubnet, Gateway: "172.28.0.1"}}},
		Labels: map[string]string{ContainerLabel: "true"}})
	return err
}

// startNode creates and starts container for node n. env carries
// AX_INIT / AX_JOIN_TOKEN.
func (c *Cluster) startNode(ctx context.Context, n int, env map[string]string) error {
	name := ContainerName(n)
	envList := []string{"AX_NODE_NAME=" + NodeName(n), "AX_ADVERTISE=" + NodeIP(n), "AX_CLUSTER_NAME=" + c.opts.ClusterName, "AX_DEV=1"}
	for k, v := range env {
		envList = append(envList, k+"="+v)
	}
	exposed := nat.PortSet{}
	bindings := nat.PortMap{}
	if n == 1 {
		for cp, hp := range map[string]int{"80/tcp": c.opts.HTTPHostPort, "443/tcp": c.opts.HTTPSHostPort, "8443/tcp": c.opts.APIHostPort} {
			p := nat.Port(cp)
			exposed[p] = struct{}{}
			bindings[p] = []nat.PortBinding{{HostIP: "127.0.0.1", HostPort: strconv.Itoa(hp)}}
		}
	}
	cfg := &container.Config{Image: c.opts.Image, Env: envList, Hostname: NodeName(n), ExposedPorts: exposed,
		Labels: map[string]string{ContainerLabel: "true", ContainerLabel + ".index": strconv.Itoa(n)}}
	host := &container.HostConfig{
		Privileged: true, PortBindings: bindings, RestartPolicy: container.RestartPolicy{Name: "no"},
		Mounts: []mount.Mount{
			{Type: mount.TypeVolume, Source: name + "-data", Target: "/var/lib/aetherix"},
			{Type: mount.TypeVolume, Source: name + "-docker", Target: "/var/lib/docker"},
		},
		Resources:    container.Resources{Devices: []container.DeviceMapping{{PathOnHost: "/dev/net/tun", PathInContainer: "/dev/net/tun", CgroupPermissions: "rwm"}}},
		CgroupnsMode: "host",
	}
	nc := &network.NetworkingConfig{EndpointsConfig: map[string]*network.EndpointSettings{
		NetworkName: {IPAMConfig: &network.EndpointIPAMConfig{IPv4Address: NodeIP(n)}, Aliases: []string{NodeName(n)}}}}
	resp, err := c.cli.ContainerCreate(ctx, cfg, host, nc, nil, name)
	if err != nil {
		return fmt.Errorf("create %s: %w", name, err)
	}
	return c.cli.ContainerStart(ctx, resp.ID, container.StartOptions{})
}

// ---- exec

// Exec runs a shell command inside node n's container and returns combined output.
func (c *Cluster) Exec(ctx context.Context, n int, script string) (string, error) {
	return c.execArgs(ctx, ContainerName(n), []string{"sh", "-c", script})
}

func (c *Cluster) execArgs(ctx context.Context, name string, args []string) (string, error) {
	ex, err := c.cli.ContainerExecCreate(ctx, name, container.ExecOptions{Cmd: args, AttachStdout: true, AttachStderr: true})
	if err != nil {
		return "", err
	}
	hj, err := c.cli.ContainerExecAttach(ctx, ex.ID, container.ExecAttachOptions{})
	if err != nil {
		return "", err
	}
	defer hj.Close()
	var out bytes.Buffer
	_, _ = stdcopy.StdCopy(&out, &out, hj.Reader)
	ins, err := c.cli.ContainerExecInspect(ctx, ex.ID)
	if err != nil {
		return out.String(), err
	}
	if ins.ExitCode != 0 {
		return out.String(), fmt.Errorf("exit code %d: %s", ins.ExitCode, strings.TrimSpace(out.String()))
	}
	return out.String(), nil
}

// ---- API access

func (c *Cluster) apiClient(st *State) (*http.Client, error) {
	tlsCfg, err := pki.ClientTLS([]byte(st.CAPEM))
	if err != nil {
		return nil, err
	}
	tlsCfg.ServerName = "127.0.0.1"
	return &http.Client{Timeout: 60 * time.Second, Transport: &http.Transport{TLSClientConfig: tlsCfg, ForceAttemptHTTP2: true}}, nil
}

func authed(token string) connect.Option {
	return connect.WithInterceptors(connect.UnaryInterceptorFunc(func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			req.Header().Set("Authorization", "Bearer "+token)
			return next(ctx, req)
		}
	}))
}

// LoadState reads the persisted dev state.
func (c *Cluster) LoadState() (*State, error) {
	b, err := os.ReadFile(c.opts.StateFile)
	if err != nil {
		return nil, fmt.Errorf("no dev cluster state (%s): run `ax dev up` first", c.opts.StateFile)
	}
	var st State
	return &st, json.Unmarshal(b, &st)
}

func (c *Cluster) saveState(st *State) error {
	if err := os.MkdirAll(filepath.Dir(c.opts.StateFile), 0o700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(st, "", "  ")
	return os.WriteFile(c.opts.StateFile, b, 0o600)
}

func (c *Cluster) machines(ctx context.Context, st *State) ([]*v1.Machine, error) {
	hc, err := c.apiClient(st)
	if err != nil {
		return nil, err
	}
	mc := aetherixv1connect.NewMachineServiceClient(hc, st.APIURL, authed(st.AdminToken))
	r, err := mc.ListMachines(ctx, connect.NewRequest(&v1.ListMachinesRequest{}))
	if err != nil {
		return nil, err
	}
	return r.Msg.Machines, nil
}

func (c *Cluster) waitMachines(ctx context.Context, st *State, want int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var last error
	for time.Now().Before(deadline) {
		ms, err := c.machines(ctx, st)
		if err == nil {
			up := 0
			for _, m := range ms {
				if m.State == v1.MachineState_MACHINE_STATE_UP {
					up++
				}
			}
			if up >= want {
				return nil
			}
			last = fmt.Errorf("%d/%d machines up", up, want)
		} else {
			last = err
		}
		select {
		case <-time.After(2 * time.Second):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return fmt.Errorf("timed out waiting for cluster: %w", last)
}

func (c *Cluster) joinToken(ctx context.Context, st *State, name string) (string, error) {
	hc, err := c.apiClient(st)
	if err != nil {
		return "", err
	}
	mc := aetherixv1connect.NewMachineServiceClient(hc, st.APIURL, authed(st.AdminToken))
	r, err := mc.CreateJoinToken(ctx, connect.NewRequest(&v1.CreateJoinTokenRequest{MachineName: name}))
	if err != nil {
		return "", err
	}
	return r.Msg.Token, nil
}

// UpResult describes a started cluster.
type UpResult struct {
	State State
	Nodes []Node
}

// Up builds the image if needed, starts node 1 (init) and joins the others.
func (c *Cluster) Up(ctx context.Context) (*UpResult, error) {
	if existing, _ := c.Nodes(ctx); len(existing) > 0 {
		return nil, errors.New("a dev cluster already exists: run `ax dev down` first")
	}
	if err := c.ensureImage(ctx); err != nil {
		return nil, err
	}
	if err := c.ensureNetwork(ctx); err != nil {
		return nil, err
	}
	c.logf("Starting node-1 (initialising the cluster)...")
	if err := c.startNode(ctx, 1, map[string]string{"AX_INIT": "1"}); err != nil {
		return nil, err
	}
	var init struct {
		AdminToken string `json:"admin_token"`
		CAPEM      string `json:"ca_pem"`
	}
	deadline := time.Now().Add(3 * time.Minute)
	for {
		out, err := c.Exec(ctx, 1, "cat /var/lib/aetherix/init.json")
		if err == nil && json.Unmarshal([]byte(out), &init) == nil && init.AdminToken != "" {
			break
		}
		if time.Now().After(deadline) {
			return nil, errors.New("node-1 did not initialise in time (check `docker logs ax-node-1`)")
		}
		select {
		case <-time.After(time.Second):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	st := &State{APIURL: fmt.Sprintf("https://127.0.0.1:%d", c.opts.APIHostPort), AdminToken: init.AdminToken, CAPEM: init.CAPEM, Image: c.opts.Image}
	if err := c.saveState(st); err != nil {
		return nil, err
	}
	if err := c.waitMachines(ctx, st, 1, 3*time.Minute); err != nil {
		return nil, err
	}
	for n := 2; n <= c.opts.Nodes; n++ {
		if err := c.addNode(ctx, st, n); err != nil {
			return nil, err
		}
	}
	nodes, _ := c.Nodes(ctx)
	return &UpResult{State: *st, Nodes: nodes}, nil
}

func (c *Cluster) addNode(ctx context.Context, st *State, n int) error {
	c.logf("Joining node-%d...", n)
	tok, err := c.joinToken(ctx, st, NodeName(n))
	if err != nil {
		return err
	}
	if err := c.startNode(ctx, n, map[string]string{"AX_JOIN_TOKEN": tok}); err != nil {
		return err
	}
	if err := c.waitMachines(ctx, st, n, 3*time.Minute); err != nil {
		logs, _ := c.containerLogs(ctx, ContainerName(n), 80)
		if logs != "" {
			c.logf("node-%d logs (tail):\n%s", n, logs)
		}
		for _, idx := range []int{1, n} {
			diag, _ := c.Exec(ctx, idx, "wg show all; echo '---'; ip -br addr; echo '---'; cat /proc/net/wireguard 2>/dev/null | head -5 || true")
			if diag != "" {
				c.logf("node-%d mesh diag:\n%s", idx, diag)
			}
		}
		return err
	}
	return nil
}

// AddNode starts the next node and joins it.
func (c *Cluster) AddNode(ctx context.Context) (Node, error) {
	st, err := c.LoadState()
	if err != nil {
		return Node{}, err
	}
	nodes, err := c.Nodes(ctx)
	if err != nil {
		return Node{}, err
	}
	next := 1
	for _, n := range nodes {
		if n.Index >= next {
			next = n.Index + 1
		}
	}
	if err := c.addNode(ctx, st, next); err != nil {
		return Node{}, err
	}
	return Node{Index: next, Name: NodeName(next), Container: ContainerName(next), IP: NodeIP(next)}, nil
}

// Down removes all dev containers (and volumes with volumes=true) and the network.
func (c *Cluster) Down(ctx context.Context, volumes bool) error {
	nodes, err := c.Nodes(ctx)
	if err != nil {
		return err
	}
	for _, n := range nodes {
		if err := c.cli.ContainerRemove(ctx, n.Container, container.RemoveOptions{Force: true}); err != nil && !client.IsErrNotFound(err) {
			return err
		}
		if volumes {
			for _, v := range []string{n.Container + "-data", n.Container + "-docker"} {
				_ = c.cli.VolumeRemove(ctx, v, true)
			}
		}
	}
	if volumes {
		vols, _ := c.cli.VolumeList(ctx, volume.ListOptions{})
		for _, v := range vols.Volumes {
			if strings.HasPrefix(v.Name, "ax-node-") {
				_ = c.cli.VolumeRemove(ctx, v.Name, true)
			}
		}
		_ = os.Remove(c.opts.StateFile)
	}
	_ = c.cli.NetworkRemove(ctx, NetworkName)
	return nil
}

// Kill kills the node's container (a hard crash).
func (c *Cluster) Kill(ctx context.Context, n int) error {
	return c.cli.ContainerKill(ctx, ContainerName(n), "SIGKILL")
}

// Start restarts a killed/stopped node.
func (c *Cluster) Start(ctx context.Context, n int) error {
	return c.cli.ContainerStart(ctx, ContainerName(n), container.StartOptions{})
}

// Pause freezes the node (like a hung VM); Unpause resumes it.
func (c *Cluster) Pause(ctx context.Context, n int) error {
	return c.cli.ContainerPause(ctx, ContainerName(n))
}
func (c *Cluster) Unpause(ctx context.Context, n int) error {
	return c.cli.ContainerUnpause(ctx, ContainerName(n))
}

// Partition cuts all traffic between nodes a and b (both directions).
func (c *Cluster) Partition(ctx context.Context, a, b int) error {
	if a == b {
		return errors.New("choose two different nodes")
	}
	if _, err := c.Exec(ctx, a, PartitionScript(NodeIP(b))); err != nil {
		return err
	}
	_, err := c.Exec(ctx, b, PartitionScript(NodeIP(a)))
	return err
}

// Heal removes every partition.
func (c *Cluster) Heal(ctx context.Context) error {
	nodes, err := c.Nodes(ctx)
	if err != nil {
		return err
	}
	for _, n := range nodes {
		if n.State != "running" {
			continue
		}
		if _, err := c.Exec(ctx, n.Index, HealScript); err != nil {
			return err
		}
	}
	return nil
}

// Latency adds egress latency on a node (0 clears it).
func (c *Cluster) Latency(ctx context.Context, n int, d time.Duration) error {
	_, err := c.Exec(ctx, n, LatencyScript(d))
	return err
}

// SSHArgs returns the docker CLI args for an interactive shell on node n.
func SSHArgs(n int) []string { return []string{"exec", "-it", ContainerName(n), "bash"} }
