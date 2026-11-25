package build

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Railpack file names and defaults.
const (
	RailpackPlanFile = "railpack-plan.json"
	RailpackInfoFile = "railpack-info.json"
	// DefaultFrontendImage is the BuildKit gateway frontend that turns a
	// railpack-plan.json into LLB. Pin a tag matching the installed CLI with
	// AX_RAILPACK_FRONTEND (see deploy/dev/Dockerfile.node).
	DefaultFrontendImage = "ghcr.io/railwayapp/railpack-frontend"
)

// DetectedStep is one Railpack build step.
type DetectedStep struct {
	Name     string   `json:"name"`
	Commands []string `json:"commands,omitempty"`
}

// DetectedPlan is what the UI shows before the first deploy: language,
// version, install/build/start commands. Fields the user may override map to
// Settings.BuildCommand / Settings.StartCommand / Args (env).
type DetectedPlan struct {
	Providers    []string          `json:"providers"`
	Language     string            `json:"language"`
	Packages     map[string]string `json:"packages,omitempty"`
	Steps        []DetectedStep    `json:"steps,omitempty"`
	InstallCmds  []string          `json:"install_commands,omitempty"`
	BuildCmds    []string          `json:"build_commands,omitempty"`
	StartCommand string            `json:"start_command,omitempty"`
	Secrets      []string          `json:"secrets,omitempty"`
	Metadata     map[string]string `json:"metadata,omitempty"`
}

// RailpackResult is the output of `railpack prepare`.
type RailpackResult struct {
	// PlanDir contains railpack-plan.json; used as BuildKit's "dockerfile"
	// local source.
	PlanDir  string
	Detected DetectedPlan
}

// PrepareRailpack runs `railpack prepare` for dir, writing the plan below
// workDir (never into the source tree). env carries Settings.Args and build
// secrets: Railpack records secret NAMES and a hash in the plan, never values.
func PrepareRailpack(ctx context.Context, r Runner, dir, workDir string, s Settings, env map[string]string, out io.Writer) (*RailpackResult, error) {
	planDir := filepath.Join(workDir, "railpack")
	if err := os.MkdirAll(planDir, 0o755); err != nil {
		return nil, err
	}
	args := []string{"prepare", dir,
		"--plan-out", filepath.Join(planDir, RailpackPlanFile),
		"--info-out", filepath.Join(planDir, RailpackInfoFile)}
	if s.BuildCommand != "" {
		args = append(args, "--build-cmd", s.BuildCommand)
	}
	if s.StartCommand != "" {
		args = append(args, "--start-cmd", s.StartCommand)
	}
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var redact []string
	for _, k := range keys {
		args = append(args, "--env", k+"="+env[k])
		redact = append(redact, env[k])
	}
	if err := r.Run(ctx, Cmd{Name: "railpack", Args: args, Redact: redact}, out); err != nil {
		return nil, fmt.Errorf("railpack prepare: %w", err)
	}
	planB, err := os.ReadFile(filepath.Join(planDir, RailpackPlanFile))
	if err != nil {
		return nil, fmt.Errorf("railpack prepare produced no plan: %w", err)
	}
	infoB, _ := os.ReadFile(filepath.Join(planDir, RailpackInfoFile))
	det, err := ParseRailpack(planB, infoB)
	if err != nil {
		return nil, err
	}
	return &RailpackResult{PlanDir: planDir, Detected: *det}, nil
}

// ParseRailpack extracts the preview from railpack-plan.json and (optional)
// railpack-info.json. Parsing is deliberately lenient: unknown fields and
// shape changes between Railpack versions only reduce the preview detail.
func ParseRailpack(planJSON, infoJSON []byte) (*DetectedPlan, error) {
	var plan struct {
		Steps []struct {
			Name     string            `json:"name"`
			Commands []json.RawMessage `json:"commands"`
		} `json:"steps"`
		Deploy struct {
			StartCmd  string `json:"startCommand"`
			StartCmd2 string `json:"start_command"`
		} `json:"deploy"`
		Secrets []string `json:"secrets"`
	}
	if err := json.Unmarshal(planJSON, &plan); err != nil {
		return nil, fmt.Errorf("invalid railpack plan: %w", err)
	}
	d := &DetectedPlan{StartCommand: firstNonEmpty(plan.Deploy.StartCmd, plan.Deploy.StartCmd2), Secrets: plan.Secrets}
	for _, st := range plan.Steps {
		ds := DetectedStep{Name: st.Name}
		for _, c := range st.Commands {
			var obj struct {
				Cmd string `json:"cmd"`
			}
			var str string
			switch {
			case json.Unmarshal(c, &obj) == nil && obj.Cmd != "":
				ds.Commands = append(ds.Commands, obj.Cmd)
			case json.Unmarshal(c, &str) == nil && str != "":
				ds.Commands = append(ds.Commands, str)
			}
		}
		d.Steps = append(d.Steps, ds)
		switch {
		case strings.Contains(st.Name, "install"):
			d.InstallCmds = append(d.InstallCmds, ds.Commands...)
		case strings.Contains(st.Name, "build"):
			d.BuildCmds = append(d.BuildCmds, ds.Commands...)
		}
	}
	if len(infoJSON) > 0 {
		var info struct {
			Providers []string          `json:"detectedProviders"`
			Metadata  map[string]string `json:"metadata"`
			Packages  map[string]struct {
				Resolved  string `json:"resolvedVersion"`
				Resolved2 string `json:"resolved"`
				Requested string `json:"requestedVersion"`
			} `json:"resolvedPackages"`
		}
		if json.Unmarshal(infoJSON, &info) == nil {
			d.Providers, d.Metadata = info.Providers, info.Metadata
			if len(info.Packages) > 0 {
				d.Packages = map[string]string{}
				for n, p := range info.Packages {
					d.Packages[n] = firstNonEmpty(p.Resolved, p.Resolved2, p.Requested)
				}
			}
		}
	}
	if len(d.Providers) > 0 {
		d.Language = d.Providers[0]
	}
	return d, nil
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}
