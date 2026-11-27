// Package build is the auto-build pipeline: source detection (explicit
// builder, Dockerfile, compose build:, Railpack), Railpack prepare -> BuildKit
// build with a registry layer cache, push to the embedded registry, the build
// queue worker (leases, least-loaded builder selection), build logs and the
// `ax up` upload / plan-preview HTTP endpoints.
package build

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"time"
)

// Pipeline turns a source directory into an image in the embedded registry:
// detect -> (Railpack prepare) -> BuildKit solve with registry cache -> push.
type Pipeline struct {
	BK     BuildKit
	Runner Runner
	// WorkDir is scratch space for generated plans (default os.TempDir()).
	WorkDir string
	// FrontendImage is the Railpack BuildKit frontend (default DefaultFrontendImage).
	FrontendImage string
}

// Job is one build.
type Job struct {
	SrcDir   string
	Settings Settings
	// Project and Service name the image repo and the cache ref.
	Project, Service string
	// Tag of the pushed image (e.g. short commit SHA + build id).
	Tag string
	// Secrets are build secrets (passed as BuildKit secrets).
	Secrets map[string]string
	Log     io.Writer
}

// Result of a successful build.
type Result struct {
	Provider Provider      `json:"provider"`
	Image    string        `json:"image"`
	Digest   string        `json:"digest"`
	Plan     *Plan         `json:"plan"`
	Detected *DetectedPlan `json:"detected,omitempty"`
}

func (p *Pipeline) frontend() string {
	if p.FrontendImage != "" {
		return p.FrontendImage
	}
	return DefaultFrontendImage
}

func (p *Pipeline) tmp() (string, error) {
	base := p.WorkDir
	if base != "" {
		if err := os.MkdirAll(base, 0o755); err != nil {
			return "", err
		}
	}
	return os.MkdirTemp(base, "ax-plan-")
}

// Run executes the pipeline.
func (p *Pipeline) Run(ctx context.Context, j Job) (*Result, error) {
	log := j.Log
	if log == nil {
		log = io.Discard
	}
	if _, ok := p.BK.(*Buildctl); ok {
		if err := waitLocalRegistry(ctx, log); err != nil {
			return nil, err
		}
		fmt.Fprintln(log, "==> checking buildkitd")
		if err := p.BK.(*Buildctl).Ping(ctx, log); err != nil {
			return nil, err
		}
	}
	plan, err := Detect(j.SrcDir, j.Settings)
	if err != nil {
		return nil, err
	}
	fmt.Fprintf(log, "==> detected build type: %s (%s)\n", plan.Provider, plan.Reason)
	image := ImageRef(j.Project, j.Service, j.Tag)
	req := BuildRequest{
		Platform: j.Settings.Platform, Args: plan.Args, Secrets: j.Secrets,
		Image: image, CacheRef: CacheRefFor(j.Project, j.Service), Target: plan.Target,
	}
	res := &Result{Provider: plan.Provider, Image: image, Plan: plan}
	if n := len(j.Secrets); n > 0 {
		fmt.Fprintf(log, "==> build env: %d variables\n", n)
	}
	switch plan.Provider {
	case ProviderDockerfile, ProviderCompose:
		req.Frontend = "dockerfile.v0"
		req.ContextDir = plan.ContextDir
		req.DockerfileDir = filepath.Dir(plan.Dockerfile)
		req.Filename = filepath.Base(plan.Dockerfile)
		// Also expose variables as build-args so Dockerfiles can use ARG KEY
		// without RUN --mount=type=secret (secrets remain available as mounts too).
		if len(j.Secrets) > 0 {
			req.Args = mergeArgs(plan.Args, j.Secrets)
		}
	case ProviderRailpack:
		work, err := p.tmp()
		if err != nil {
			return nil, err
		}
		defer os.RemoveAll(work)
		env := map[string]string{}
		for k, v := range plan.Args {
			env[k] = v
		}
		for k, v := range j.Secrets {
			env[k] = v
		}
		fmt.Fprintln(log, "==> railpack prepare")
		rp, err := PrepareRailpack(ctx, p.Runner, plan.RootDir, work, j.Settings, env, log)
		if err != nil {
			return nil, err
		}
		res.Detected = &rp.Detected
		req.Frontend, req.FrontendSource = "gateway.v0", p.frontend()
		req.ContextDir, req.DockerfileDir, req.Filename = plan.RootDir, rp.PlanDir, RailpackPlanFile
		req.Args = nil // Railpack consumes variables through the plan, not build args
	default:
		return nil, fmt.Errorf("unsupported provider %q", plan.Provider)
	}
	fmt.Fprintf(log, "==> building %s with BuildKit (cache %s)\n", image, req.CacheRef)
	br, err := p.BK.Build(ctx, req, log)
	if err != nil {
		return nil, err
	}
	res.Digest = br.Digest
	fmt.Fprintf(log, "==> pushed %s %s\n", image, br.Digest)
	return res, nil
}

func waitLocalRegistry(ctx context.Context, log io.Writer) error {
	addr := LocalRegistry
	deadline := time.Now().Add(20 * time.Second)
	var last error
	for time.Now().Before(deadline) {
		d := net.Dialer{Timeout: time.Second}
		c, err := d.DialContext(ctx, "tcp", addr)
		if err == nil {
			_ = c.Close()
			return nil
		}
		last = err
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(300 * time.Millisecond):
		}
	}
	fmt.Fprintf(log, "==> waiting for local registry %s failed: %v\n", addr, last)
	return fmt.Errorf("local registry %s not reachable: %w (is aetherixd's registry listening?)", addr, last)
}

// Preview is the detected-plan preview for the UI.
type Preview struct {
	Plan     *Plan         `json:"plan"`
	Detected *DetectedPlan `json:"detected,omitempty"`
}

// Preview detects the build type and, for Railpack, runs `railpack prepare`
// (no build) so the UI can show language, version and commands.
func (p *Pipeline) Preview(ctx context.Context, srcDir string, s Settings, env map[string]string) (*Preview, error) {
	plan, err := Detect(srcDir, s)
	if err != nil {
		return nil, err
	}
	pv := &Preview{Plan: plan}
	if plan.Provider == ProviderRailpack {
		work, err := p.tmp()
		if err != nil {
			return nil, err
		}
		defer os.RemoveAll(work)
		e := map[string]string{}
		for k, v := range plan.Args {
			e[k] = v
		}
		for k, v := range env {
			e[k] = v
		}
		rp, err := PrepareRailpack(ctx, p.Runner, plan.RootDir, work, s, e, io.Discard)
		if err != nil {
			return nil, err
		}
		pv.Detected = &rp.Detected
	}
	return pv, nil
}
