package metrics

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"

	"github.com/aetherix/aetherixcloud/internal/store"
)

// Path is the HTTP path served by Handler.
const Path = "/api/metrics"

// API serves local metrics and optionally aggregates peer series.
type API struct {
	Collector *Collector
	Store     store.Store
	Self      string
	// PeerFetch, when set, retrieves SeriesView lists from other machines.
	PeerFetch func(ctx context.Context, machineID, serviceID string) ([]SeriesView, error)
	Authenticate func(*http.Request) error
}

// Handler returns the authenticated metrics endpoint.
func (a *API) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a.Authenticate != nil {
			if err := a.Authenticate(r); err != nil {
				http.Error(w, err.Error(), http.StatusUnauthorized)
				return
			}
		}
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		svc := r.URL.Query().Get("service_id")
		localOnly := r.URL.Query().Get("local") == "1" || r.Header.Get("Ax-Proxied") != ""
		views := []SeriesView{}
		if a.Collector != nil {
			views = a.Collector.Buffer.ForService(svc)
		}
		if !localOnly && a.PeerFetch != nil && a.Store != nil && svc != "" {
			reps, _ := store.ListReplicas(r.Context(), a.Store, map[string]string{"service_id": svc})
			seen := map[string]bool{a.Self: true}
			for _, rep := range reps {
				if seen[rep.MachineId] || rep.MachineId == "" {
					continue
				}
				seen[rep.MachineId] = true
				peer, err := a.PeerFetch(r.Context(), rep.MachineId, svc)
				if err != nil {
					continue
				}
				views = append(views, peer...)
			}
		}
		// Aggregate for the UI: average series across replicas into cpu/mem/net.
		agg := Aggregate(views)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(Response{ServiceID: svc, Series: views, Aggregate: agg})
	})
}

// Response is the JSON body of GET /api/metrics.
type Response struct {
	ServiceID string         `json:"service_id,omitempty"`
	Series    []SeriesView   `json:"series"`
	Aggregate AggregateView  `json:"aggregate"`
}

// AggregateView is downsampled sparkline-friendly series.
type AggregateView struct {
	CPUPercent  []float64 `json:"cpu_percent"`
	MemoryUsage []float64 `json:"memory_usage"`
	NetRxBytes  []float64 `json:"net_rx_bytes"`
	NetTxBytes  []float64 `json:"net_tx_bytes"`
	Times       []int64   `json:"times"` // unix millis
}

// Aggregate merges replica series into aligned averages (up to 60 points).
func Aggregate(views []SeriesView) AggregateView {
	const points = 48
	if len(views) == 0 {
		return AggregateView{}
	}
	// Pick the longest series as the time axis.
	best := views[0].Samples
	for _, v := range views[1:] {
		if len(v.Samples) > len(best) {
			best = v.Samples
		}
	}
	if len(best) == 0 {
		return AggregateView{}
	}
	step := 1
	if len(best) > points {
		step = len(best) / points
	}
	out := AggregateView{}
	for i := 0; i < len(best); i += step {
		t := best[i].Time
		var cpu, mem, rx, tx float64
		var n float64
		for _, v := range views {
			if s, ok := nearest(v.Samples, t); ok {
				cpu += s.CPUPercent
				mem += float64(s.MemoryUsage)
				rx += float64(s.NetRxBytes)
				tx += float64(s.NetTxBytes)
				n++
			}
		}
		if n == 0 {
			continue
		}
		out.Times = append(out.Times, t.UnixMilli())
		out.CPUPercent = append(out.CPUPercent, cpu/n)
		out.MemoryUsage = append(out.MemoryUsage, mem/n)
		out.NetRxBytes = append(out.NetRxBytes, rx/n)
		out.NetTxBytes = append(out.NetTxBytes, tx/n)
	}
	return out
}

func nearest(samples []Sample, t interface{ UnixNano() int64 }) (Sample, bool) {
	if len(samples) == 0 {
		return Sample{}, false
	}
	target := t.UnixNano()
	best, bi := samples[0], 0
	bestDiff := abs64(best.Time.UnixNano() - target)
	for i, s := range samples[1:] {
		d := abs64(s.Time.UnixNano() - target)
		if d < bestDiff {
			best, bestDiff, bi = s, d, i+1
		}
		_ = bi
	}
	return best, true
}

func abs64(x int64) int64 {
	if x < 0 {
		return -x
	}
	return x
}

// Lazy mounts handlers once the collector is ready (same pattern as build.Lazy).
type Lazy struct{ h atomic.Pointer[http.Handler] }

func (l *Lazy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h := l.h.Load(); h != nil {
		(*h).ServeHTTP(w, r)
		return
	}
	http.Error(w, "starting", http.StatusServiceUnavailable)
}

func (l *Lazy) Set(h http.Handler) { l.h.Store(&h) }

// NormalizeServiceID trims query input.
func NormalizeServiceID(s string) string { return strings.TrimSpace(s) }
