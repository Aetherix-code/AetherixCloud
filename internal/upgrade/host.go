package upgrade

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Pinned host-tool versions. Bump these when the release needs newer builders;
// install.sh and deploy/dev/Dockerfile.node should match.
const (
	DefaultBuildkitVersion = "0.35.1"
	DefaultRailpackVersion = "0.43.1"
	defaultBuildkitAddr    = "unix:///run/buildkit/buildkitd.sock"
)

const defaultBuildkitTOML = `[registry."127.0.0.1:5000"]
  http = true
  insecure = true

[worker.oci]
  gc = true
  [[worker.oci.gcpolicy]]
    keepBytes = 10737418240
    all = true
`

// HostOptions control SyncHostDeps (BuildKit, Railpack, systemd units, env).
// Designed so future host tools can be added beside the existing steps.
type HostOptions struct {
	BinDir            string // default: directory of current executable
	UnitDir           string // default /etc/systemd/system
	BuildkitConfigDir string // default /etc/buildkit
	Arch              string // amd64|arm64; default runtime.GOARCH
	BuildkitVersion   string
	RailpackVersion   string
	HTTPClient        *http.Client
	Logger            *slog.Logger
	// ReleaseDir is an extracted release tree; when set, unit/toml files are
	// preferred from there (deploy/*.service, deploy/*.toml, or top-level).
	ReleaseDir string
	// SkipStart writes units but does not enable/start buildkitd.
	SkipStart bool
}

// SyncHostDeps installs or refreshes node host tooling required by this
// aetherixd version (BuildKit + Railpack today). Idempotent: no-ops when
// binaries already match the pinned versions. Skipped when AX_SKIP_HOST_DEPS
// is set, on non-Linux, or when not running as root.
//
// Called from Apply (cluster upgrades) and once on daemon start so the first
// upgrade onto a release that adds host deps still provisions them.
func SyncHostDeps(ctx context.Context, opt HostOptions) error {
	if v := strings.TrimSpace(os.Getenv("AX_SKIP_HOST_DEPS")); v != "" && v != "0" && !strings.EqualFold(v, "false") {
		return nil
	}
	if runtime.GOOS != "linux" {
		return nil
	}
	if os.Geteuid() != 0 {
		return nil
	}
	opt = opt.withDefaults()
	log := opt.Logger
	if err := ensureBuildkit(ctx, opt, log); err != nil {
		return fmt.Errorf("buildkit: %w", err)
	}
	if err := ensureRailpack(ctx, opt, log); err != nil {
		return fmt.Errorf("railpack: %w", err)
	}
	if err := ensureBuildkitConfig(opt, log); err != nil {
		return err
	}
	if err := ensureBuilderEnv(opt, log); err != nil {
		return err
	}
	if opt.SkipStart {
		return nil
	}
	return enableBuildkitd(log)
}

func (opt HostOptions) withDefaults() HostOptions {
	if opt.BinDir == "" {
		if exe, err := os.Executable(); err == nil {
			if r, err := filepath.EvalSymlinks(exe); err == nil {
				exe = r
			}
			opt.BinDir = filepath.Dir(exe)
		} else {
			opt.BinDir = "/usr/local/bin"
		}
	}
	if opt.UnitDir == "" {
		opt.UnitDir = "/etc/systemd/system"
	}
	if opt.BuildkitConfigDir == "" {
		opt.BuildkitConfigDir = "/etc/buildkit"
	}
	if opt.Arch == "" {
		opt.Arch = runtime.GOARCH
	}
	if opt.BuildkitVersion == "" {
		opt.BuildkitVersion = DefaultBuildkitVersion
	}
	if opt.RailpackVersion == "" {
		opt.RailpackVersion = DefaultRailpackVersion
	}
	if opt.HTTPClient == nil {
		opt.HTTPClient = &http.Client{Timeout: 5 * time.Minute}
	}
	if opt.Logger == nil {
		opt.Logger = slog.Default()
	}
	return opt
}

func toolVersionOK(bin, want string) bool {
	if want == "" {
		return false
	}
	path, err := exec.LookPath(bin)
	if err != nil {
		// Also accept BinDir-installed tools not yet on PATH.
		return false
	}
	out, err := exec.Command(path, "--version").CombinedOutput()
	if err != nil {
		return false
	}
	return strings.Contains(string(out), want)
}

func binPath(opt HostOptions, name string) string {
	return filepath.Join(opt.BinDir, name)
}

func ensureBuildkit(ctx context.Context, opt HostOptions, log *slog.Logger) error {
	if versionAt(binPath(opt, "buildkitd"), opt.BuildkitVersion) && versionAt(binPath(opt, "buildctl"), opt.BuildkitVersion) {
		return nil
	}
	if toolVersionOK("buildkitd", opt.BuildkitVersion) && toolVersionOK("buildctl", opt.BuildkitVersion) {
		return nil
	}
	url := fmt.Sprintf("https://github.com/moby/buildkit/releases/download/v%s/buildkit-v%s.linux-%s.tar.gz",
		opt.BuildkitVersion, opt.BuildkitVersion, opt.Arch)
	log.Info("installing BuildKit", "version", opt.BuildkitVersion, "url", url)
	work, err := os.MkdirTemp("", "ax-buildkit-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(work)
	tgz := filepath.Join(work, "buildkit.tgz")
	if err := downloadFile(ctx, opt.HTTPClient, url, tgz); err != nil {
		return err
	}
	extract := filepath.Join(work, "extract")
	if err := os.MkdirAll(extract, 0o755); err != nil {
		return err
	}
	if err := untar(tgz, extract); err != nil {
		return err
	}
	for _, name := range []string{"buildkitd", "buildctl"} {
		src, err := findFile(extract, name)
		if err != nil {
			return err
		}
		if err := installBinary(src, binPath(opt, name)); err != nil {
			return fmt.Errorf("install %s: %w", name, err)
		}
	}
	log.Info("BuildKit installed", "version", opt.BuildkitVersion)
	return nil
}

func ensureRailpack(ctx context.Context, opt HostOptions, log *slog.Logger) error {
	if versionAt(binPath(opt, "railpack"), opt.RailpackVersion) || toolVersionOK("railpack", opt.RailpackVersion) {
		return nil
	}
	rpArch := opt.Arch
	if rpArch == "amd64" {
		rpArch = "x86_64"
	}
	name := fmt.Sprintf("railpack-v%s-%s-unknown-linux-musl.tar.gz", opt.RailpackVersion, rpArch)
	base := fmt.Sprintf("https://github.com/railwayapp/railpack/releases/download/v%s", opt.RailpackVersion)
	log.Info("installing Railpack", "version", opt.RailpackVersion)
	work, err := os.MkdirTemp("", "ax-railpack-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(work)
	tgz := filepath.Join(work, name)
	if err := downloadFile(ctx, opt.HTTPClient, base+"/"+name, tgz); err != nil {
		return err
	}
	sumPath := filepath.Join(work, "checksums.txt")
	if err := downloadFile(ctx, opt.HTTPClient, base+"/checksums.txt", sumPath); err != nil {
		log.Warn("railpack checksums unavailable; skipping verify", "err", err)
	} else if err := verifyFileChecksum(sumPath, name, tgz); err != nil {
		return err
	}
	extract := filepath.Join(work, "extract")
	if err := os.MkdirAll(extract, 0o755); err != nil {
		return err
	}
	if err := untar(tgz, extract); err != nil {
		return err
	}
	src, err := findFile(extract, "railpack")
	if err != nil {
		return err
	}
	if err := installBinary(src, binPath(opt, "railpack")); err != nil {
		return fmt.Errorf("install railpack: %w", err)
	}
	log.Info("Railpack installed", "version", opt.RailpackVersion)
	return nil
}

func versionAt(path, want string) bool {
	if want == "" {
		return false
	}
	st, err := os.Stat(path)
	if err != nil || st.IsDir() {
		return false
	}
	out, err := exec.Command(path, "--version").CombinedOutput()
	if err != nil {
		return false
	}
	return strings.Contains(string(out), want)
}

func verifyFileChecksum(sumFile, archive, path string) error {
	body, err := os.ReadFile(sumFile)
	if err != nil {
		return err
	}
	want := ""
	for _, line := range strings.Split(string(body), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[len(fields)-1] == archive {
			want = fields[0]
			break
		}
	}
	if want == "" {
		return fmt.Errorf("checksums.txt has no entry for %s", archive)
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	got := hex.EncodeToString(h.Sum(nil))
	if !strings.EqualFold(got, want) {
		return fmt.Errorf("checksum mismatch for %s", archive)
	}
	return nil
}

func ensureBuildkitConfig(opt HostOptions, log *slog.Logger) error {
	if err := os.MkdirAll(opt.BuildkitConfigDir, 0o755); err != nil {
		return err
	}
	tomlPath := filepath.Join(opt.BuildkitConfigDir, "buildkitd.toml")
	if _, err := os.Stat(tomlPath); err != nil {
		body := []byte(defaultBuildkitTOML)
		if b, err := readReleaseFile(opt.ReleaseDir, "buildkitd.toml"); err == nil {
			body = b
		}
		if err := os.WriteFile(tomlPath, body, 0o644); err != nil {
			return err
		}
		log.Info("wrote buildkitd.toml", "path", tomlPath)
	}
	if err := os.MkdirAll(opt.UnitDir, 0o755); err != nil {
		return err
	}
	unitPath := filepath.Join(opt.UnitDir, "buildkitd.service")
	body := []byte(buildkitdUnit(opt.BinDir, opt.BuildkitConfigDir))
	if b, err := readReleaseFile(opt.ReleaseDir, "buildkitd.service"); err == nil {
		body = b
	}
	if err := os.WriteFile(unitPath, body, 0o644); err != nil {
		return err
	}
	if opt.BinDir != "/usr/local/bin" {
		drop := filepath.Join(opt.UnitDir, "buildkitd.service.d")
		if err := os.MkdirAll(drop, 0o755); err != nil {
			return err
		}
		override := fmt.Sprintf("[Service]\nExecStart=\nExecStart=%s --config %s/buildkitd.toml --addr %s --root /var/lib/buildkit\n",
			binPath(opt, "buildkitd"), opt.BuildkitConfigDir, defaultBuildkitAddr)
		if err := os.WriteFile(filepath.Join(drop, "override.conf"), []byte(override), 0o644); err != nil {
			return err
		}
	}
	return nil
}

func buildkitdUnit(binDir, cfgDir string) string {
	return fmt.Sprintf(`[Unit]
Description=AetherixCloud BuildKit daemon (builder nodes)
Documentation=https://github.com/Aetherix-code/AetherixCloud
After=network-online.target docker.service
Wants=network-online.target
Requires=docker.service

[Service]
Type=simple
ExecStart=%s/buildkitd --config %s/buildkitd.toml --addr %s --root /var/lib/buildkit
Restart=always
RestartSec=5
TimeoutStopSec=30
LimitNOFILE=1048576
RuntimeDirectory=buildkit
StateDirectory=buildkit

[Install]
WantedBy=multi-user.target
`, binDir, cfgDir, defaultBuildkitAddr)
}

func ensureBuilderEnv(opt HostOptions, log *slog.Logger) error {
	drop := filepath.Join(opt.UnitDir, "aetherixd.service.d")
	if err := os.MkdirAll(drop, 0o755); err != nil {
		return err
	}
	frontend := "ghcr.io/railwayapp/railpack-frontend:v" + opt.RailpackVersion
	body := fmt.Sprintf(`[Service]
Environment=AX_CADDY_BIN=%s/caddy
Environment=AX_BUILDKIT_ADDR=%s
Environment=AX_RAILPACK_FRONTEND=%s
`, opt.BinDir, defaultBuildkitAddr, frontend)
	path := filepath.Join(drop, "builder.conf")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		return err
	}
	log.Info("wrote aetherixd builder env", "path", path, "frontend", frontend)
	return nil
}

func enableBuildkitd(log *slog.Logger) error {
	if _, err := exec.LookPath("systemctl"); err != nil {
		log.Warn("systemctl not found; start buildkitd manually")
		return nil
	}
	_ = exec.Command("systemctl", "daemon-reload").Run()
	if err := exec.Command("systemctl", "enable", "--now", "buildkitd").Run(); err != nil {
		// Best-effort restart if enable --now failed (already enabled).
		if err2 := exec.Command("systemctl", "restart", "buildkitd").Run(); err2 != nil {
			log.Warn("could not start buildkitd", "err", err, "restart_err", err2)
			return nil
		}
	}
	log.Info("buildkitd enabled")
	return nil
}

func readReleaseFile(releaseDir, name string) ([]byte, error) {
	if releaseDir == "" {
		return nil, os.ErrNotExist
	}
	candidates := []string{
		filepath.Join(releaseDir, "deploy", name),
		filepath.Join(releaseDir, name),
	}
	var last error
	for _, p := range candidates {
		b, err := os.ReadFile(p)
		if err == nil {
			return b, nil
		}
		last = err
	}
	return nil, last
}
