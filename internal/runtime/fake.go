package runtime

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"
)

// Fake is an in-memory Runtime for tests. It is safe for concurrent use.
type Fake struct {
	mu         sync.Mutex
	seq        int
	Containers map[string]*Container
	Specs      map[string]ContainerSpec
	Images     map[string]bool
	Networks   map[string]NetworkSpec
	// Failures: image refs for which PullImage fails.
	FailPull map[string]bool
	// AutoHealthy: started containers report Health="healthy" immediately.
	AutoHealthy bool
	// Calls records operations ("create:<name>", "start:<id>", ...).
	Calls []string
}

// NewFake returns an empty fake.
func NewFake() *Fake {
	return &Fake{
		Containers: map[string]*Container{}, Specs: map[string]ContainerSpec{},
		Images: map[string]bool{}, Networks: map[string]NetworkSpec{}, FailPull: map[string]bool{},
	}
}

func (f *Fake) rec(s string) { f.Calls = append(f.Calls, s) }

func (f *Fake) EnsureNetwork(_ context.Context, spec NetworkSpec) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Networks[spec.Name] = spec
	return nil
}

func (f *Fake) ListContainers(_ context.Context, labels map[string]string) ([]Container, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []Container
	for _, c := range f.Containers {
		ok := true
		for k, v := range labels {
			if c.Labels[k] != v {
				ok = false
			}
		}
		if ok {
			out = append(out, *c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (f *Fake) InspectContainer(_ context.Context, id string) (Container, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.Containers[id]
	if !ok {
		return Container{}, ErrNotFound
	}
	return *c, nil
}

func (f *Fake) PullImage(_ context.Context, ref string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rec("pull:" + ref)
	if f.FailPull[ref] {
		return fmt.Errorf("pull %s: denied", ref)
	}
	f.Images[ref] = true
	return nil
}

func (f *Fake) ImageExists(_ context.Context, ref string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.Images[ref], nil
}

func (f *Fake) TagImage(_ context.Context, src, dst string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.Images[src] {
		return ErrNotFound
	}
	f.Images[dst] = true
	f.rec("tag:" + src + "->" + dst)
	return nil
}

func (f *Fake) CreateContainer(_ context.Context, spec ContainerSpec) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.Images[spec.Image] {
		return "", fmt.Errorf("no such image: %s", spec.Image)
	}
	f.seq++
	id := fmt.Sprintf("c%04d", f.seq)
	f.Containers[id] = &Container{ID: id, Name: spec.Name, Image: spec.Image, Labels: spec.Labels, State: "created",
		IP: fmt.Sprintf("10.0.0.%d", f.seq)}
	f.Specs[id] = spec
	f.rec("create:" + spec.Name)
	return id, nil
}

func (f *Fake) StartContainer(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.Containers[id]
	if !ok {
		return ErrNotFound
	}
	c.State = "running"
	c.StartedAt = time.Now()
	if f.AutoHealthy {
		c.Health = "healthy"
	}
	f.rec("start:" + id)
	return nil
}

func (f *Fake) StopContainer(_ context.Context, id string, _ time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.Containers[id]
	if !ok {
		return ErrNotFound
	}
	c.State = "exited"
	f.rec("stop:" + id)
	return nil
}

func (f *Fake) RemoveContainer(_ context.Context, id string, _ bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.Containers[id]; !ok {
		return ErrNotFound
	}
	delete(f.Containers, id)
	delete(f.Specs, id)
	f.rec("remove:" + id)
	return nil
}

// Crash marks a container exited (simulated failure).
func (f *Fake) Crash(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c, ok := f.Containers[id]; ok {
		c.State, c.ExitCode = "exited", 1
	}
}

// CallsWithPrefix returns recorded calls starting with prefix.
func (f *Fake) CallsWithPrefix(prefix string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.Calls {
		if strings.HasPrefix(c, prefix) {
			out = append(out, c)
		}
	}
	return out
}

func (f *Fake) StreamLogs(ctx context.Context, id string, _ LogOptions) (<-chan LogLine, error) {
	f.mu.Lock()
	_, ok := f.Containers[id]
	f.mu.Unlock()
	if !ok {
		return nil, ErrNotFound
	}
	ch := make(chan LogLine, 1)
	ch <- LogLine{Time: time.Now(), Stream: "stdout", Line: "fake log"}
	close(ch)
	return ch, nil
}

func (f *Fake) Exec(context.Context, string, ExecOptions) (ExecSession, error) {
	return nil, fmt.Errorf("fake: exec unsupported")
}

func (f *Fake) Close() error { return nil }

var _ = io.EOF
