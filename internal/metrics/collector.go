package metrics

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"time"

	"github.com/aetherix/aetherixcloud/internal/runtime"
	"github.com/aetherix/aetherixcloud/internal/store"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
)

// Collector samples Docker stats for local replicas into a ring buffer.
type Collector struct {
	Store    store.Store
	Runtime  runtime.Runtime
	Self     string
	Buffer   *Buffer
	Interval time.Duration
	Logger   *slog.Logger

	// dockerCLI is set when Runtime exposes a Docker client (production path).
	dockerCLI *client.Client
}

// NewCollector builds a collector. capacity samples ≈ Interval*capacity window.
func NewCollector(st store.Store, rt runtime.Runtime, self string, log *slog.Logger) *Collector {
	c := &Collector{
		Store: st, Runtime: rt, Self: self,
		Buffer: NewBuffer(360, 512), // ~1h at 10s
		Interval: 10 * time.Second,
		Logger: log,
	}
	if log == nil {
		c.Logger = slog.Default()
	}
	if d, ok := rt.(interface{ Client() *client.Client }); ok {
		c.dockerCLI = d.Client()
	}
	return c
}

// Run samples until ctx is done.
func (c *Collector) Run(ctx context.Context) {
	if c.Runtime == nil {
		return
	}
	t := time.NewTicker(c.Interval)
	defer t.Stop()
	c.sample(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.sample(ctx)
		}
	}
}

func (c *Collector) sample(ctx context.Context) {
	reps, err := store.ListReplicas(ctx, c.Store, map[string]string{"machine_id": c.Self})
	if err != nil {
		return
	}
	for _, r := range reps {
		if r.ContainerId == "" || !r.DesiredRunning {
			continue
		}
		s, err := c.statsOnce(ctx, r.ContainerId)
		if err != nil {
			continue
		}
		c.Buffer.Record(r.Id, Meta{
			ServiceID: r.ServiceId, ReplicaID: r.Id,
			ContainerID: r.ContainerId, MachineID: r.MachineId,
		}, s)
	}
}

func (c *Collector) statsOnce(ctx context.Context, id string) (Sample, error) {
	if c.dockerCLI == nil {
		// Fake / no-docker: synthesise a tiny sample so the API still works in tests.
		return Sample{Time: time.Now(), CPUPercent: 0, MemoryUsage: 0}, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	resp, err := c.dockerCLI.ContainerStats(ctx, id, false)
	if err != nil {
		return Sample{}, err
	}
	defer resp.Body.Close()
	var st container.StatsResponse
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil && err != io.EOF {
		return Sample{}, err
	}
	return fromDocker(st), nil
}

func fromDocker(st container.StatsResponse) Sample {
	s := Sample{Time: time.Now()}
	if st.Read.IsZero() {
		s.Time = time.Now()
	} else {
		s.Time = st.Read
	}
	s.MemoryUsage = st.MemoryStats.Usage
	s.MemoryLimit = st.MemoryStats.Limit
	s.CPUPercent = calcCPU(st)
	for _, n := range st.Networks {
		s.NetRxBytes += n.RxBytes
		s.NetTxBytes += n.TxBytes
	}
	for _, b := range st.BlkioStats.IoServiceBytesRecursive {
		switch b.Op {
		case "read", "Read":
			s.BlockRead += b.Value
		case "write", "Write":
			s.BlockWrite += b.Value
		}
	}
	return s
}

func calcCPU(st container.StatsResponse) float64 {
	cpuDelta := float64(st.CPUStats.CPUUsage.TotalUsage - st.PreCPUStats.CPUUsage.TotalUsage)
	sysDelta := float64(st.CPUStats.SystemUsage - st.PreCPUStats.SystemUsage)
	if cpuDelta <= 0 || sysDelta <= 0 {
		return 0
	}
	ncpu := float64(st.CPUStats.OnlineCPUs)
	if ncpu == 0 {
		ncpu = float64(len(st.CPUStats.CPUUsage.PercpuUsage))
	}
	if ncpu == 0 {
		ncpu = 1
	}
	return (cpuDelta / sysDelta) * ncpu * 100.0
}
