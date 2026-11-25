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

// ApplyOptions control binary install + restart behaviour.
type ApplyOptions struct {
	Version      string
	Arch         string // default runtime.GOARCH
	DownloadBase string // optional mirror; else GitHub releases
	Repo         string
	BinDir       string // default: directory of current executable
	HTTPClient   *http.Client
	Logger       *slog.Logger
	// SkipRestart only installs binaries.
	SkipRestart bool
	// Restart hooks (optional). Defaults to DefaultRestart.
	Restart func(log *slog.Logger) error
}

// Apply downloads the release for Version, verifies checksums when available,
// replaces aetherixd + ax, then restarts the daemon.
func Apply(ctx context.Context, opt ApplyOptions) error {
	if opt.Version == "" {
		return fmt.Errorf("upgrade: version is required")
	}
	if opt.Arch == "" {
		opt.Arch = runtime.GOARCH
	}
	if opt.BinDir == "" {
		exe, err := os.Executable()
		if err != nil {
			return err
		}
		if resolved, err := filepath.EvalSymlinks(exe); err == nil {
			exe = resolved
		}
		opt.BinDir = filepath.Dir(exe)
	}
	if opt.HTTPClient == nil {
		opt.HTTPClient = &http.Client{Timeout: 5 * time.Minute}
	}
	log := opt.Logger
	if log == nil {
		log = slog.Default()
	}
	version := Normalize(opt.Version)
	work, err := os.MkdirTemp("", "aetherix-upgrade-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(work)

	archive := ArchiveName(version, opt.Arch)
	url := assetURL(opt.DownloadBase, repoOrDefault(opt.Repo), version, archive)
	log.Info("downloading release", "url", url)
	tgz := filepath.Join(work, "release.tgz")
	if err := downloadFile(ctx, opt.HTTPClient, url, tgz); err != nil {
		alt := "aetherixcloud_" + version + "_linux_" + opt.Arch + ".tar.gz"
		url = assetURL(opt.DownloadBase, repoOrDefault(opt.Repo), version, alt)
		log.Info("retrying download", "url", url)
		if err2 := downloadFile(ctx, opt.HTTPClient, url, tgz); err2 != nil {
			return fmt.Errorf("download: %v (retry: %w)", err, err2)
		}
		archive = alt
	}
	if err := verifyChecksum(ctx, opt.HTTPClient, opt.DownloadBase, repoOrDefault(opt.Repo), version, archive, tgz, log); err != nil {
		return err
	}
	extract := filepath.Join(work, "extract")
	if err := os.MkdirAll(extract, 0o755); err != nil {
		return err
	}
	if err := untar(tgz, extract); err != nil {
		return err
	}
	aetherixdBin, err := findFile(extract, "aetherixd")
	if err != nil {
		return err
	}
	axBin, err := findFile(extract, "ax")
	if err != nil {
		return err
	}
	if err := installBinary(aetherixdBin, filepath.Join(opt.BinDir, "aetherixd")); err != nil {
		return fmt.Errorf("install aetherixd: %w", err)
	}
	if err := installBinary(axBin, filepath.Join(opt.BinDir, "ax")); err != nil {
		return fmt.Errorf("install ax: %w", err)
	}
	log.Info("binaries installed", "dir", opt.BinDir, "version", version)
	if opt.SkipRestart {
		return nil
	}
	restart := opt.Restart
	if restart == nil {
		restart = DefaultRestart
	}
	return restart(log)
}

func assetURL(base, repo, version, name string) string {
	if base != "" {
		return strings.TrimRight(base, "/") + "/" + name
	}
	return fmt.Sprintf("https://github.com/%s/releases/download/%s/%s", repo, version, name)
}

func downloadFile(ctx context.Context, hc *http.Client, url, dest string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "aetherixcloud-upgrade")
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(f, resp.Body)
	return err
}

func verifyChecksum(ctx context.Context, hc *http.Client, base, repo, version, archive, tgz string, log *slog.Logger) error {
	sumURL := assetURL(base, repo, version, "checksums.txt")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, sumURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "aetherixcloud-upgrade")
	resp, err := hc.Do(req)
	if err != nil {
		log.Warn("checksums.txt unavailable; skipping verify", "err", err)
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		log.Warn("checksums.txt unavailable; skipping verify", "status", resp.Status)
		return nil
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
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
		log.Warn("checksums.txt has no entry for archive; skipping verify", "archive", archive)
		return nil
	}
	f, err := os.Open(tgz)
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
		return fmt.Errorf("checksum mismatch for %s: got %s want %s", archive, got, want)
	}
	log.Info("checksum verified", "archive", archive)
	return nil
}

func installBinary(src, dest string) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	bak := dest + ".bak"
	_ = os.Remove(bak)
	if _, err := os.Stat(dest); err == nil {
		if err := os.Rename(dest, bak); err != nil {
			if err := copyFile(dest, bak); err != nil {
				return err
			}
		}
	}
	tmp := dest + ".new"
	if err := copyFile(src, tmp); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o755); err != nil {
		return err
	}
	return os.Rename(tmp, dest)
}

func copyFile(src, dest string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

func findFile(root, name string) (string, error) {
	var found string
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		if info.Name() == name && found == "" {
			found = path
		}
		return nil
	})
	if found == "" {
		return "", fmt.Errorf("archive missing %s", name)
	}
	return found, nil
}

// ErrPendingRestart means binaries were installed but the process was not restarted.
var ErrPendingRestart = fmt.Errorf("upgrade installed; restart required")

// DefaultRestart restarts via systemctl when the unit is active; otherwise
// schedules a non-zero exit so systemd Restart= can recover.
// In AX_DEV it returns ErrPendingRestart without killing the DinD container.
func DefaultRestart(log *slog.Logger) error {
	if os.Getenv("AX_DEV") == "1" || strings.EqualFold(os.Getenv("AX_DEV"), "true") {
		return ErrPendingRestart
	}
	if systemdActive("aetherixd") {
		log.Info("restarting via systemctl")
		cmd := exec.Command("systemctl", "restart", "aetherixd")
		if err := cmd.Start(); err != nil {
			return err
		}
		return nil
	}
	log.Info("no systemd unit; exiting for supervisor restart")
	go func() {
		time.Sleep(750 * time.Millisecond)
		os.Exit(1)
	}()
	return nil
}

func systemdActive(unit string) bool {
	cmd := exec.Command("systemctl", "is-active", "--quiet", unit)
	return cmd.Run() == nil
}
