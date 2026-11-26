package api

import (
	"context"
	"errors"
	"sync"
)

// ErrBuildUnavailable is returned when a GitHub-sourced service needs a build
// but the build hook has not registered a trigger yet.
var ErrBuildUnavailable = errors.New("build pipeline unavailable: trigger a build after the GitHub integration is ready, or set spec.image")

// BuildTrigger is a shared slot the GitHub/build hook fills so CreateService
// and Deploy can enqueue a build for services that have no image yet.
type BuildTrigger struct {
	mu sync.RWMutex
	fn func(ctx context.Context, serviceID, ref string) (buildID string, err error)
}

// Set registers the enqueue callback (called once from the github hook).
func (t *BuildTrigger) Set(fn func(ctx context.Context, serviceID, ref string) (buildID string, err error)) {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.fn = fn
	t.mu.Unlock()
}

// Enqueue runs the registered build trigger.
func (t *BuildTrigger) Enqueue(ctx context.Context, serviceID, ref string) (string, error) {
	if t == nil {
		return "", ErrBuildUnavailable
	}
	t.mu.RLock()
	fn := t.fn
	t.mu.RUnlock()
	if fn == nil {
		return "", ErrBuildUnavailable
	}
	return fn(ctx, serviceID, ref)
}
