package build

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	v1 "github.com/aetherix/aetherixcloud/gen/aetherix/v1"
	"gopkg.in/yaml.v3"
)

// Provider is the detected build type.
type Provider string

const (
	ProviderDockerfile Provider = "dockerfile"
	ProviderCompose    Provider = "compose"
	ProviderRailpack   Provider = "railpack"
	ProviderImage      Provider = "image"
)

// Settings are the user-facing build settings (UI Settings tab / x-build).
type Settings struct {
	// Builder is "" (auto) | dockerfile | railpack | image.
	Builder string
	// RootDir is the build root inside the source (monorepos); BuildSpec.context.
	RootDir      string
	Dockerfile   string // relative to RootDir
	Target       string
	BuildCommand string
	StartCommand string
	Args         map[string]string
	WatchPaths   []string
	Platform     string
	// ServiceName selects the compose service when several define build:.
	ServiceName string
}

// SettingsFromSpec converts a service BuildSpec.
func SettingsFromSpec(b *v1.BuildSpec, serviceName string) Settings {
	s := Settings{ServiceName: serviceName}
	if b == nil {
		return s
	}
	s.Builder, s.RootDir, s.Dockerfile, s.Target = b.Builder, b.Context, b.Dockerfile, b.Target
	s.BuildCommand, s.StartCommand, s.Args, s.WatchPaths, s.Platform = b.BuildCommand, b.StartCommand, b.Args, b.WatchPaths, b.Platform
	return s
}

// ComposeBuild is one compose service with a build: section.
type ComposeBuild struct {
	Service    string            `json:"service"`
	ContextDir string            `json:"context_dir"`
	Dockerfile string            `json:"dockerfile"`
	Target     string            `json:"target,omitempty"`
	Args       map[string]string `json:"args,omitempty"`
}

// Plan is the result of detection.
type Plan struct {
	Provider Provider `json:"provider"`
	// RootDir is the absolute build root.
	RootDir string `json:"root_dir"`
	// ContextDir is the absolute BuildKit context (== RootDir except compose).
	ContextDir string `json:"context_dir"`
	// Dockerfile is the absolute Dockerfile path (dockerfile/compose).
	Dockerfile string            `json:"dockerfile,omitempty"`
	Target     string            `json:"target,omitempty"`
	Args       map[string]string `json:"args,omitempty"`
	// Reason explains the choice (shown in the UI).
	Reason string `json:"reason"`
	// Compose lists every compose service with a build: section (preview).
	Compose []ComposeBuild `json:"compose,omitempty"`
	// ComposeService is the selected compose service (provider compose).
	ComposeService string `json:"compose_service,omitempty"`
}

// ErrNothingToBuild is returned for builder "image".
var ErrNothingToBuild = errors.New("builder is \"image\": nothing to build")

var composeNames = []string{"compose.yaml", "compose.yml", "docker-compose.yaml", "docker-compose.yml"}

// safeJoin joins rel below base and rejects escapes.
func safeJoin(base, rel string) (string, error) {
	p := filepath.Join(base, filepath.FromSlash(rel))
	r, err := filepath.Rel(base, p)
	if err != nil || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q escapes the source directory", rel)
	}
	return p, nil
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

// Detect picks the build type: explicit builder, Dockerfile at the configured
// path, compose build: sections, then Railpack.
func Detect(srcDir string, s Settings) (*Plan, error) {
	root, err := safeJoin(srcDir, s.RootDir)
	if err != nil {
		return nil, err
	}
	if st, err := os.Stat(root); err != nil || !st.IsDir() {
		return nil, fmt.Errorf("root directory %q does not exist in the source", s.RootDir)
	}
	p := &Plan{RootDir: root, ContextDir: root, Target: s.Target, Args: s.Args}
	dockerfile := func() (string, bool, error) {
		name := s.Dockerfile
		if name == "" {
			name = "Dockerfile"
		}
		f, err := safeJoin(root, name)
		if err != nil {
			return "", false, err
		}
		return f, fileExists(f), nil
	}
	switch s.Builder {
	case "image":
		return nil, ErrNothingToBuild
	case "dockerfile":
		f, ok, err := dockerfile()
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, fmt.Errorf("builder is dockerfile but %s does not exist", f)
		}
		p.Provider, p.Dockerfile, p.Reason = ProviderDockerfile, f, "builder set to dockerfile"
		return p, nil
	case "railpack":
		p.Provider, p.Reason = ProviderRailpack, "builder set to railpack"
		return p, nil
	case "", "auto":
	default:
		return nil, fmt.Errorf("unknown builder %q (dockerfile, railpack, image)", s.Builder)
	}
	// Dockerfile at the configured path.
	f, ok, err := dockerfile()
	if err != nil {
		return nil, err
	}
	if ok {
		p.Provider, p.Dockerfile = ProviderDockerfile, f
		p.Reason = "Dockerfile found at " + rel(root, f)
		return p, nil
	}
	if s.Dockerfile != "" {
		return nil, fmt.Errorf("configured Dockerfile %q not found", s.Dockerfile)
	}
	// Compose build: sections.
	for _, n := range composeNames {
		cf := filepath.Join(root, n)
		if !fileExists(cf) {
			continue
		}
		list, err := parseComposeBuilds(cf)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", n, err)
		}
		if len(list) == 0 {
			continue
		}
		p.Compose = list
		pick := pickCompose(list, s.ServiceName)
		if pick == nil {
			continue // several services, none matches: fall through to Railpack
		}
		p.Provider, p.ComposeService = ProviderCompose, pick.Service
		p.ContextDir, p.Dockerfile, p.Target = pick.ContextDir, pick.Dockerfile, pick.Target
		p.Args = mergeArgs(pick.Args, s.Args)
		p.Reason = fmt.Sprintf("%s service %q has a build: section", n, pick.Service)
		return p, nil
	}
	p.Provider, p.Reason = ProviderRailpack, "no Dockerfile or compose build: found; using Railpack"
	return p, nil
}

func rel(base, p string) string {
	r, err := filepath.Rel(base, p)
	if err != nil {
		return p
	}
	return filepath.ToSlash(r)
}

func mergeArgs(base, over map[string]string) map[string]string {
	if len(base) == 0 && len(over) == 0 {
		return nil
	}
	out := map[string]string{}
	for k, v := range base {
		out[k] = v
	}
	for k, v := range over {
		out[k] = v
	}
	return out
}

func pickCompose(list []ComposeBuild, name string) *ComposeBuild {
	for i := range list {
		if name != "" && list[i].Service == name {
			return &list[i]
		}
	}
	if len(list) == 1 {
		return &list[0]
	}
	return nil
}

type composeFile struct {
	Services map[string]struct {
		Build any `yaml:"build"`
	} `yaml:"services"`
}

func parseComposeBuilds(path string) ([]ComposeBuild, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cf composeFile
	if err := yaml.Unmarshal(b, &cf); err != nil {
		return nil, err
	}
	dir := filepath.Dir(path)
	var out []ComposeBuild
	for name, svc := range cf.Services {
		if svc.Build == nil {
			continue
		}
		cb := ComposeBuild{Service: name, ContextDir: dir}
		df := "Dockerfile"
		switch v := svc.Build.(type) {
		case string:
			cb.ContextDir = filepath.Join(dir, v)
		case map[string]any:
			if c, ok := v["context"].(string); ok && c != "" {
				cb.ContextDir = filepath.Join(dir, c)
			}
			if d, ok := v["dockerfile"].(string); ok && d != "" {
				df = d
			}
			cb.Target, _ = v["target"].(string)
			cb.Args = composeArgs(v["args"])
		default:
			return nil, fmt.Errorf("service %q: invalid build section", name)
		}
		cb.Dockerfile = filepath.Join(cb.ContextDir, df)
		out = append(out, cb)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Service < out[j].Service })
	return out, nil
}

func composeArgs(v any) map[string]string {
	out := map[string]string{}
	switch a := v.(type) {
	case map[string]any:
		for k, val := range a {
			out[k] = fmt.Sprint(val)
		}
	case []any:
		for _, e := range a {
			k, val, _ := strings.Cut(fmt.Sprint(e), "=")
			out[k] = val
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
