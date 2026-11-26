package build

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// BuildRequest is one BuildKit solve.
type BuildRequest struct {
	// Frontend is "dockerfile.v0" or "gateway.v0" (Railpack).
	Frontend       string
	FrontendSource string // gateway.v0: frontend image
	ContextDir     string
	// DockerfileDir is the "dockerfile" local source dir (holds Filename).
	DockerfileDir string
	Filename      string
	Target        string
	Platform      string
	Args          map[string]string
	// Secrets are BuildKit secrets (id -> value); never baked into layers.
	Secrets map[string]string
	// Image is the full ref to push, e.g. 127.0.0.1:5000/proj/svc:tag.
	Image string
	// CacheRef is the registry cache ref ("" disables caching).
	CacheRef string
}

// BuildResult is the outcome of a solve.
type BuildResult struct {
	Digest string
}

// BuildKit is the build engine. The production implementation talks to
// buildkitd; tests use a fake.
type BuildKit interface {
	Build(ctx context.Context, req BuildRequest, out io.Writer) (BuildResult, error)
}

// DefaultBuildkitAddr is the buildkitd socket on builder nodes.
const DefaultBuildkitAddr = "unix:///run/buildkit/buildkitd.sock"

// Buildctl drives buildkitd through the `buildctl` binary shipped in the
// BuildKit release (the same gRPC API as github.com/moby/buildkit/client,
// without pulling its dependency tree into the daemon).
type Buildctl struct {
	Runner Runner
	// Addr is the buildkitd address (default DefaultBuildkitAddr).
	Addr string
	// Binary defaults to "buildctl".
	Binary string
}

var secretEnvRe = regexp.MustCompile(`[^A-Za-z0-9_]`)

// SecretEnvName is the process env var carrying the secret with this ID.
func SecretEnvName(id string) string { return "AXSECRET_" + secretEnvRe.ReplaceAllString(id, "_") }

// Args returns the buildctl argument list for req (exported for tests).
func (b *Buildctl) Args(req BuildRequest, metadataFile string) (args []string, env, redact []string) {
	addr := b.Addr
	if addr == "" {
		addr = DefaultBuildkitAddr
	}
	args = []string{"--addr", addr, "build", "--progress", "plain", "--frontend", req.Frontend,
		"--local", "context=" + req.ContextDir, "--local", "dockerfile=" + req.DockerfileDir}
	if req.FrontendSource != "" {
		args = append(args, "--opt", "source="+req.FrontendSource)
	}
	if req.Filename != "" {
		args = append(args, "--opt", "filename="+req.Filename)
	}
	if req.Target != "" {
		args = append(args, "--opt", "target="+req.Target)
	}
	if req.Platform != "" {
		args = append(args, "--opt", "platform="+req.Platform)
	}
	keys := sortedKeys(req.Args)
	for _, k := range keys {
		args = append(args, "--opt", "build-arg:"+k+"="+req.Args[k])
	}
	skeys := sortedKeys(req.Secrets)
	for _, k := range skeys {
		args = append(args, "--secret", fmt.Sprintf("id=%s,env=%s", k, SecretEnvName(k)))
		env = append(env, SecretEnvName(k)+"="+req.Secrets[k])
		redact = append(redact, req.Secrets[k])
	}
	args = append(args, "--output", "type=image,name="+req.Image+",push=true,registry.insecure=true")
	if req.CacheRef != "" {
		args = append(args,
			"--import-cache", "type=registry,ref="+req.CacheRef+",registry.insecure=true",
			"--export-cache", "type=registry,ref="+req.CacheRef+",mode=max,image-manifest=true,oci-mediatypes=true,registry.insecure=true")
	}
	if metadataFile != "" {
		args = append(args, "--metadata-file", metadataFile)
	}
	return args, env, redact
}

// AddrOrDefault returns the buildkitd address.
func (b *Buildctl) AddrOrDefault() string {
	if b != nil && b.Addr != "" {
		return b.Addr
	}
	return DefaultBuildkitAddr
}

// Ping checks that buildctl can reach buildkitd.
func (b *Buildctl) Ping(ctx context.Context, out io.Writer) error {
	bin := "buildctl"
	if b != nil && b.Binary != "" {
		bin = b.Binary
	}
	run := Runner(ExecRunner{})
	if b != nil && b.Runner != nil {
		run = b.Runner
	}
	cap := &captureWriter{w: out}
	err := run.Run(ctx, Cmd{Name: bin, Args: []string{"--addr", b.AddrOrDefault(), "debug", "workers"}}, cap)
	if err != nil {
		hint := "is buildkitd running? try: systemctl status buildkitd"
		tail := strings.TrimSpace(lastLines(string(cap.buf), 8))
		if tail != "" {
			return fmt.Errorf("buildkitd unreachable (%s): %w\n%s", hint, err, tail)
		}
		return fmt.Errorf("buildkitd unreachable (%s): %w", hint, err)
	}
	return nil
}

// Build implements BuildKit.
func (b *Buildctl) Build(ctx context.Context, req BuildRequest, out io.Writer) (BuildResult, error) {
	dir, err := os.MkdirTemp("", "ax-buildmeta-")
	if err != nil {
		return BuildResult{}, err
	}
	defer os.RemoveAll(dir)
	meta := filepath.Join(dir, "metadata.json")
	args, env, redact := b.Args(req, meta)
	bin := b.Binary
	if bin == "" {
		bin = "buildctl"
	}
	cap := &captureWriter{w: out}
	if err := b.Runner.Run(ctx, Cmd{Name: bin, Args: args, Env: env, Redact: redact}, cap); err != nil {
		tail := strings.TrimSpace(lastLines(string(cap.buf), 12))
		if looksLikeConnErr(tail) || looksLikeConnErr(err.Error()) {
			return BuildResult{}, fmt.Errorf("buildkit build: %w (is buildkitd running? systemctl status buildkitd)\n%s", err, tail)
		}
		if tail != "" {
			return BuildResult{}, fmt.Errorf("buildkit build: %w\n%s", err, tail)
		}
		return BuildResult{}, fmt.Errorf("buildkit build: %w", err)
	}
	var res BuildResult
	if raw, err := os.ReadFile(meta); err == nil {
		var m map[string]any
		if json.Unmarshal(raw, &m) == nil {
			if d, ok := m["containerimage.digest"].(string); ok {
				res.Digest = d
			}
		}
	}
	return res, nil
}

func looksLikeConnErr(s string) bool {
	s = strings.ToLower(s)
	for _, p := range []string{"connection refused", "no such file", "unavailable", "dial unix", "connect:"} {
		if strings.Contains(s, p) {
			return true
		}
	}
	return false
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n")
	var keep []string
	for _, l := range lines {
		if strings.TrimSpace(l) != "" {
			keep = append(keep, l)
		}
	}
	if len(keep) > n {
		keep = keep[len(keep)-n:]
	}
	return strings.Join(keep, "\n")
}

func sortedKeys(m map[string]string) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

// ImageRef builds the registry reference for a build: lowercase, only
// [a-z0-9._-/] in the repository path, "<LocalRegistry>/<project>/<service>:<tag>".
func ImageRef(project, service, tag string) string {
	return fmt.Sprintf("%s/%s/%s:%s", LocalRegistry, sanitizeRepo(project), sanitizeRepo(service), sanitizeTag(tag))
}

// CacheRefFor returns the per-service registry cache ref.
func CacheRefFor(project, service string) string {
	return fmt.Sprintf("%s/_cache/%s/%s:buildcache", LocalRegistry, sanitizeRepo(project), sanitizeRepo(service))
}

// LocalRegistry is the embedded registry host valid on every machine.
const LocalRegistry = "127.0.0.1:5000"

var repoBad = regexp.MustCompile(`[^a-z0-9._-]+`)

func sanitizeRepo(s string) string {
	s = strings.Trim(repoBad.ReplaceAllString(strings.ToLower(s), "-"), "-._")
	if s == "" {
		s = "app"
	}
	return s
}

func sanitizeTag(s string) string {
	s = regexp.MustCompile(`[^A-Za-z0-9._-]+`).ReplaceAllString(s, "-")
	if len(s) > 100 {
		s = s[:100]
	}
	if s == "" {
		s = "latest"
	}
	return s
}
