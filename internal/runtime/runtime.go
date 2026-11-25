// Package runtime abstracts the container runtime. The production
// implementation is Docker (NewDocker); the reconciler is tested against the
// in-memory Fake.
//
// Every container managed by AetherixCloud carries the labels below so the
// node can map containers back to store rows after a daemon restart.
package runtime

import (
	"context"
	"errors"
	"io"
	"time"
)

// Container labels.
const (
	LabelManaged     = "aetherix.managed" // "true"
	LabelCluster     = "aetherix.cluster"
	LabelMachine     = "aetherix.machine"
	LabelProject     = "aetherix.project"
	LabelService     = "aetherix.service"      // service ID
	LabelServiceName = "aetherix.service.name" // human name
	LabelDeployment  = "aetherix.deployment"
	LabelReplica     = "aetherix.replica"
	LabelFence       = "aetherix.fence"
)

// NetworkName is the per-node Docker bridge network all services attach to.
const NetworkName = "ax"

// ErrNotFound is returned for unknown containers/images.
var ErrNotFound = errors.New("runtime: not found")

// NetworkSpec describes the per-node container network.
type NetworkSpec struct {
	Name    string
	Subnet  string // e.g. 10.210.3.0/24
	Gateway string // e.g. 10.210.3.2
	IPRange string // e.g. 10.210.3.128/25
}

// PortBinding publishes a container port on the host (mode "host" ports).
type PortBinding struct {
	ContainerPort int
	HostPort      int
	Protocol      string // tcp|udp
	HostIP        string
}

// Mount is a volume or bind mount.
type Mount struct {
	Source    string
	Target    string
	ReadOnly  bool
	SizeBytes int64 // soft limit; applied at named-volume create time when > 0
}

// HealthSpec is a container-level health check.
type HealthSpec struct {
	Command     []string // CMD form, run inside the container
	Interval    time.Duration
	Timeout     time.Duration
	Retries     int
	StartPeriod time.Duration
}

// ContainerSpec is everything needed to create a container.
type ContainerSpec struct {
	Name          string
	Image         string
	Command       []string
	Entrypoint    []string
	Env           map[string]string
	Labels        map[string]string
	Ports         []PortBinding
	Mounts        []Mount
	Network       string
	Hostname      string
	DNS           []string
	CPUMillis     int64
	MemoryBytes   int64
	RestartPolicy string // always|on-failure|no|unless-stopped
	StopTimeout   time.Duration
	User          string
	WorkingDir    string
	Health        *HealthSpec
}

// Container is the observed state of a container.
type Container struct {
	ID        string
	Name      string
	Image     string
	Labels    map[string]string
	State     string // created|running|paused|restarting|exited|dead
	Health    string // ""|starting|healthy|unhealthy
	IP        string // address on the ax network
	ExitCode  int
	StartedAt time.Time
	Restarts  int
}

// Running reports whether the container is running.
func (c Container) Running() bool { return c.State == "running" }

// LogOptions selects log lines.
type LogOptions struct {
	Follow     bool
	Tail       int // <0 => all, 0 => default (100)
	Since      time.Time
	Timestamps bool
}

// LogLine is one demultiplexed log line.
type LogLine struct {
	Time   time.Time
	Stream string // stdout|stderr
	Line   string
}

// ExecOptions describes an exec session.
type ExecOptions struct {
	Command []string
	Env     []string
	TTY     bool
	Stdin   bool
	Rows    int
	Cols    int
}

// ExecSession is a running exec. For TTY sessions stderr is merged into
// Stdout and Stderr is nil.
type ExecSession interface {
	Stdin() io.WriteCloser
	Stdout() io.Reader
	Stderr() io.Reader
	Resize(ctx context.Context, rows, cols int) error
	// Wait blocks until the process exits and returns its exit code.
	Wait(ctx context.Context) (int, error)
	Close() error
}

// Runtime is the container runtime used by the reconciler and API.
type Runtime interface {
	// EnsureNetwork creates the per-node network if missing (idempotent).
	EnsureNetwork(ctx context.Context, spec NetworkSpec) error
	// ListContainers returns containers (any state) having all given labels.
	ListContainers(ctx context.Context, labels map[string]string) ([]Container, error)
	InspectContainer(ctx context.Context, id string) (Container, error)
	// PullImage pulls ref. Implementations may be given peer registries by
	// wrapping (see reconciler.Puller).
	PullImage(ctx context.Context, ref string) error
	ImageExists(ctx context.Context, ref string) (bool, error)
	// TagImage adds dst as an alias of the local image src.
	TagImage(ctx context.Context, src, dst string) error
	// CreateContainer creates (does not start) a container and returns its ID.
	CreateContainer(ctx context.Context, spec ContainerSpec) (string, error)
	StartContainer(ctx context.Context, id string) error
	StopContainer(ctx context.Context, id string, timeout time.Duration) error
	RemoveContainer(ctx context.Context, id string, force bool) error
	StreamLogs(ctx context.Context, id string, opts LogOptions) (<-chan LogLine, error)
	Exec(ctx context.Context, id string, opts ExecOptions) (ExecSession, error)
	Close() error
}
