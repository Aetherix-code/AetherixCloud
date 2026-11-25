package devcluster

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"connectrpc.com/connect"
	v1 "github.com/aetherix/aetherixcloud/gen/aetherix/v1"
	"github.com/aetherix/aetherixcloud/gen/aetherix/v1/aetherixv1connect"
	"github.com/docker/docker/client"
)

// IntegrationEnabled reports whether Docker-backed integration tests should run.
// Default go test ./... never builds this path (see //go:build integration).
// When the integration tag is set:
//   - AX_INTEGRATION=0 forces a skip
//   - otherwise Docker must be reachable; missing image skips unless AX_INTEGRATION_BUILD=1
func IntegrationEnabled() (ok bool, reason string) {
	if os.Getenv("AX_INTEGRATION") == "0" {
		return false, "AX_INTEGRATION=0"
	}
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return false, "docker client: " + err.Error()
	}
	defer cli.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := cli.Ping(ctx); err != nil {
		return false, "docker not reachable: " + err.Error()
	}
	return true, ""
}

// ImagePresent reports whether the given (or default) node image exists locally.
func ImagePresent(ctx context.Context, image string) bool {
	if image == "" {
		image = DefaultImage
	}
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return false
	}
	defer cli.Close()
	_, err = cli.ImageInspect(ctx, image)
	return err == nil
}

// HarnessOptions configure a test cluster.
type HarnessOptions struct {
	Nodes         int
	Image         string
	Build         bool // force rebuild; also set when AX_INTEGRATION_BUILD=1
	APIHostPort   int
	HTTPHostPort  int
	HTTPSHostPort int
	RepoDir       string
	Out           io.Writer
}

func (o *HarnessOptions) applyEnv() {
	if o.Nodes == 0 {
		o.Nodes = 3
	}
	if o.Image == "" {
		o.Image = DefaultImage
	}
	if os.Getenv("AX_INTEGRATION_BUILD") == "1" {
		o.Build = true
	}
	if o.APIHostPort == 0 {
		o.APIHostPort = 18443 // avoid clobbering a developer `ax dev` on 8443
	}
	if o.HTTPHostPort == 0 {
		o.HTTPHostPort = 18080
	}
	if o.HTTPSHostPort == 0 {
		o.HTTPSHostPort = 18443 + 1 // 18444
	}
	if o.RepoDir == "" {
		o.RepoDir = findRepoRoot()
	}
	if o.Out == nil {
		o.Out = io.Discard
	}
}

func findRepoRoot() string {
	wd, err := os.Getwd()
	if err != nil {
		return "."
	}
	dir := wd
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return wd
		}
		dir = parent
	}
}

// Harness is a disposable multi-node cluster for integration tests.
type Harness struct {
	Cluster *Cluster
	State   *State
	opts    HarnessOptions
	tmpDir  string
}

// StartHarness brings up a fresh cluster. Caller must Close.
func StartHarness(ctx context.Context, opts HarnessOptions) (*Harness, error) {
	opts.applyEnv()
	if !opts.Build && !ImagePresent(ctx, opts.Image) {
		return nil, fmt.Errorf("image %s not present (set AX_INTEGRATION_BUILD=1 to build, or run `make dev-image`)", opts.Image)
	}
	tmp, err := os.MkdirTemp("", "ax-integration-*")
	if err != nil {
		return nil, err
	}
	c, err := New(Options{
		Nodes:         opts.Nodes,
		Image:         opts.Image,
		Build:         opts.Build,
		RepoDir:       opts.RepoDir,
		APIHostPort:   opts.APIHostPort,
		HTTPHostPort:  opts.HTTPHostPort,
		HTTPSHostPort: opts.HTTPSHostPort,
		StateFile:     filepath.Join(tmp, "dev.json"),
		ClusterName:   "integration",
		Out:           opts.Out,
	})
	if err != nil {
		_ = os.RemoveAll(tmp)
		return nil, err
	}
	// Tear down any leftover labelled containers from a previous crashed run.
	_ = c.Down(ctx, true)
	up, err := c.Up(ctx)
	if err != nil {
		_ = c.Down(ctx, true)
		_ = c.Close()
		_ = os.RemoveAll(tmp)
		return nil, err
	}
	return &Harness{Cluster: c, State: &up.State, opts: opts, tmpDir: tmp}, nil
}

// Close tears down containers/volumes and the Docker client.
func (h *Harness) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	_ = h.Cluster.Down(ctx, true)
	err := h.Cluster.Close()
	_ = os.RemoveAll(h.tmpDir)
	return err
}

func (h *Harness) httpClient() (*http.Client, error) {
	return h.Cluster.apiClient(h.State)
}

// Clients returns authenticated Connect clients for the cluster API.
func (h *Harness) Clients() (aetherixv1connect.ClusterServiceClient, aetherixv1connect.MachineServiceClient, aetherixv1connect.ProjectServiceClient, aetherixv1connect.ServiceServiceClient, aetherixv1connect.DeployServiceClient, error) {
	hc, err := h.httpClient()
	if err != nil {
		return nil, nil, nil, nil, nil, err
	}
	opt := authed(h.State.AdminToken)
	return aetherixv1connect.NewClusterServiceClient(hc, h.State.APIURL, opt),
		aetherixv1connect.NewMachineServiceClient(hc, h.State.APIURL, opt),
		aetherixv1connect.NewProjectServiceClient(hc, h.State.APIURL, opt),
		aetherixv1connect.NewServiceServiceClient(hc, h.State.APIURL, opt),
		aetherixv1connect.NewDeployServiceClient(hc, h.State.APIURL, opt),
		nil
}

// WaitMachines waits until want machines are UP.
func (h *Harness) WaitMachines(ctx context.Context, want int, timeout time.Duration) error {
	return h.Cluster.waitMachines(ctx, h.State, want, timeout)
}

// WaitReplicas waits until at least want replicas of serviceID are RUNNING.
func (h *Harness) WaitReplicas(ctx context.Context, serviceID string, want int, timeout time.Duration) error {
	_, _, _, ss, _, err := h.Clients()
	if err != nil {
		return err
	}
	deadline := time.Now().Add(timeout)
	var last error
	for time.Now().Before(deadline) {
		r, err := ss.ListReplicas(ctx, connect.NewRequest(&v1.ListReplicasRequest{ServiceId: serviceID}))
		if err == nil {
			n := 0
			for _, rep := range r.Msg.Replicas {
				if rep.DesiredRunning && rep.State == v1.ReplicaState_REPLICA_STATE_RUNNING {
					n++
				}
			}
			if n >= want {
				return nil
			}
			last = fmt.Errorf("%d/%d replicas running", n, want)
		} else {
			last = err
		}
		select {
		case <-time.After(2 * time.Second):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return fmt.Errorf("timed out waiting for replicas: %w", last)
}

// AppURL is the host-published HTTPS base for node-1 ingress.
// Caddy enables automatic HTTP→HTTPS redirects in AX_DEV; hitting the HTTP
// publish port makes LoadGen follow redirects to :443 (not the harness
// HTTPSHostPort) and every probe fails.
func (h *Harness) AppURL() string {
	return fmt.Sprintf("https://127.0.0.1:%d", h.opts.HTTPSHostPort)
}

// InsecureHTTP is an HTTP client that accepts the local CA (or any TLS).
func (h *Harness) InsecureHTTP() *http.Client {
	return &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // local playground only
		},
	}
}

// LoadGenHTTP returns a client for AppURL probes. When host is a *.localhost
// (or other) name, TLS SNI must match that name — connecting to 127.0.0.1 with
// the default SNI makes Caddy abort the handshake ("tls: internal error").
func LoadGenHTTP(host string) *http.Client {
	cfg := &tls.Config{InsecureSkipVerify: true} //nolint:gosec // local playground only
	if host != "" {
		cfg.ServerName = host
	}
	return &http.Client{
		Timeout:   10 * time.Second,
		Transport: &http.Transport{TLSClientConfig: cfg, ForceAttemptHTTP2: true},
	}
}

// LoadStats is the result of a LoadGen run.
type LoadStats struct {
	OK     int64
	Failed int64
	Total  int64
}

// ZeroFailed reports whether every request succeeded.
func (s LoadStats) ZeroFailed() bool { return s.Failed == 0 && s.Total > 0 }

// LoadGen hammers a URL until Stop; used during rolling updates.
type LoadGen struct {
	URL    string
	Client *http.Client
	Host   string // optional Host header (sslip / .localhost routing)

	ok, failed, total atomic.Int64
	stop              chan struct{}
	wg                sync.WaitGroup
}

// Start begins background GETs at roughly rps requests/second (min 1).
func (g *LoadGen) Start(rps int) {
	if rps < 1 {
		rps = 1
	}
	if g.Client == nil {
		g.Client = &http.Client{Timeout: 5 * time.Second}
	}
	g.stop = make(chan struct{})
	interval := time.Second / time.Duration(rps)
	g.wg.Add(1)
	go func() {
		defer g.wg.Done()
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-g.stop:
				return
			case <-t.C:
				g.hit()
			}
		}
	}()
}

func (g *LoadGen) hit() {
	g.total.Add(1)
	req, err := http.NewRequest(http.MethodGet, g.URL, nil)
	if err != nil {
		g.failed.Add(1)
		return
	}
	if g.Host != "" {
		req.Host = g.Host
		req.Header.Set("Host", g.Host)
	}
	resp, err := g.Client.Do(req)
	if err != nil {
		g.failed.Add(1)
		return
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 500 {
		// 4xx from the app still means the mesh/ingress stayed up; treat 5xx/transport as failure.
		if resp.StatusCode < 500 {
			g.ok.Add(1)
			return
		}
	}
	g.failed.Add(1)
}

// Stop ends the generator and returns totals.
func (g *LoadGen) Stop() LoadStats {
	if g.stop != nil {
		close(g.stop)
		g.wg.Wait()
	}
	return LoadStats{OK: g.ok.Load(), Failed: g.failed.Load(), Total: g.total.Load()}
}

// Stats returns a snapshot without stopping.
func (g *LoadGen) Stats() LoadStats {
	return LoadStats{OK: g.ok.Load(), Failed: g.failed.Load(), Total: g.total.Load()}
}
