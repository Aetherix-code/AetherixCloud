package metrics

import (
	"sync"
	"time"
)

// Sample is one Docker-stats observation for a container.
type Sample struct {
	Time         time.Time `json:"time"`
	CPUPercent   float64   `json:"cpu_percent"`
	MemoryUsage  uint64    `json:"memory_usage"`
	MemoryLimit  uint64    `json:"memory_limit"`
	NetRxBytes   uint64    `json:"net_rx_bytes"`
	NetTxBytes   uint64    `json:"net_tx_bytes"`
	BlockRead    uint64    `json:"block_read"`
	BlockWrite   uint64    `json:"block_write"`
}

// Series is a fixed-capacity ring of samples for one key (replica/container).
type Series struct {
	mu   sync.Mutex
	buf  []Sample
	head int // next write index
	n    int // filled count
}

// NewSeries allocates a ring of capacity cap.
func NewSeries(cap int) *Series {
	if cap < 1 {
		cap = 1
	}
	return &Series{buf: make([]Sample, cap)}
}

// Push appends s, overwriting the oldest when full.
func (s *Series) Push(sample Sample) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.buf[s.head] = sample
	s.head = (s.head + 1) % len(s.buf)
	if s.n < len(s.buf) {
		s.n++
	}
}

// Snapshot returns samples oldest→newest.
func (s *Series) Snapshot() []Sample {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.n == 0 {
		return nil
	}
	out := make([]Sample, s.n)
	start := (s.head - s.n + len(s.buf)) % len(s.buf)
	for i := 0; i < s.n; i++ {
		out[i] = s.buf[(start+i)%len(s.buf)]
	}
	return out
}

// Latest returns the most recent sample, if any.
func (s *Series) Latest() (Sample, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.n == 0 {
		return Sample{}, false
	}
	idx := (s.head - 1 + len(s.buf)) % len(s.buf)
	return s.buf[idx], true
}

// Buffer holds many series keyed by an opaque id (usually replica id).
type Buffer struct {
	mu       sync.Mutex
	cap      int
	series   map[string]*Series
	meta     map[string]Meta
	maxKeys  int
}

// Meta tags a series with service/replica/container ids.
type Meta struct {
	ServiceID   string `json:"service_id"`
	ReplicaID   string `json:"replica_id"`
	ContainerID string `json:"container_id"`
	MachineID   string `json:"machine_id"`
}

// NewBuffer creates a buffer retaining capacity samples per key.
func NewBuffer(capacity, maxKeys int) *Buffer {
	if capacity < 8 {
		capacity = 8
	}
	if maxKeys < 16 {
		maxKeys = 16
	}
	return &Buffer{cap: capacity, series: map[string]*Series{}, meta: map[string]Meta{}, maxKeys: maxKeys}
}

// Record stores a sample under key.
func (b *Buffer) Record(key string, m Meta, s Sample) {
	b.mu.Lock()
	ser, ok := b.series[key]
	if !ok {
		if len(b.series) >= b.maxKeys {
			// Drop an arbitrary oldest-looking key (map iteration order).
			for k := range b.series {
				delete(b.series, k)
				delete(b.meta, k)
				break
			}
		}
		ser = NewSeries(b.cap)
		b.series[key] = ser
	}
	b.meta[key] = m
	b.mu.Unlock()
	ser.Push(s)
}

// ForService returns series snapshots for every key matching serviceID.
func (b *Buffer) ForService(serviceID string) []SeriesView {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []SeriesView
	for k, ser := range b.series {
		m := b.meta[k]
		if serviceID != "" && m.ServiceID != serviceID {
			continue
		}
		out = append(out, SeriesView{Meta: m, Samples: ser.Snapshot()})
	}
	return out
}

// SeriesView is the JSON shape returned by the metrics API.
type SeriesView struct {
	Meta    Meta     `json:"meta"`
	Samples []Sample `json:"samples"`
}
