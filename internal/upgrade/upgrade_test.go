package upgrade

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	v1 "github.com/aetherix/aetherixcloud/gen/aetherix/v1"
	"github.com/aetherix/aetherixcloud/internal/store"
	"github.com/stretchr/testify/require"
)

func TestNormalizeEqual(t *testing.T) {
	require.Equal(t, "v0.4.0", Normalize("0.4.0"))
	require.Equal(t, "v0.4.0", Normalize("v0.4.0"))
	require.Equal(t, "dev", Normalize("dev"))
	require.True(t, Equal("v0.4.0", "0.4.0"))
	require.False(t, Equal("v0.4.0", "v0.4.1"))
	require.Equal(t, "aetherixcloud_0.4.0_linux_amd64.tar.gz", ArchiveName("v0.4.0", "amd64"))
}

func TestConfigTargets(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	require.NoError(t, PutConfig(ctx, st, Config{DesiredVersion: "v1.0.0", MachineIDs: []string{"m2"}}))
	cfg, err := GetConfig(ctx, st)
	require.NoError(t, err)
	require.False(t, cfg.Targets("m1"))
	require.True(t, cfg.Targets("m2"))
	cfg.MachineIDs = nil
	require.True(t, cfg.Targets("m1"))
}

func TestApplyInstallsBinaries(t *testing.T) {
	dir := t.TempDir()
	payload := buildReleaseTar(t, dir)
	sum := sha256.Sum256(payload)
	archive := "aetherixcloud_1.2.3_linux_amd64.tar.gz"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/"+archive:
			_, _ = w.Write(payload)
		case r.URL.Path == "/checksums.txt":
			_, _ = fmt.Fprintf(w, "%s  %s\n", hex.EncodeToString(sum[:]), archive)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	binDir := t.TempDir()
	require.NoError(t, Apply(context.Background(), ApplyOptions{
		Version:      "v1.2.3",
		Arch:         "amd64",
		DownloadBase: srv.URL,
		BinDir:       binDir,
		SkipRestart:  true,
		HTTPClient:   srv.Client(),
	}))
	require.FileExists(t, filepath.Join(binDir, "aetherixd"))
	require.FileExists(t, filepath.Join(binDir, "ax"))
}

func TestWatcherAppliesDesired(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	require.NoError(t, store.PutMachine(ctx, st, &v1.Machine{Id: "m1", Name: "n1", AgentVersion: "v0.1.0"}))
	require.NoError(t, PutConfig(ctx, st, Config{DesiredVersion: "v0.2.0"}))

	applied := make(chan ApplyOptions, 8)
	w := &Watcher{
		Store: st, Self: "m1", Version: "v0.1.0", Arch: "amd64",
		interval: 10 * time.Millisecond,
		Apply: func(_ context.Context, opt ApplyOptions) error {
			applied <- opt
			return ErrPendingRestart
		},
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() { _ = w.Run(runCtx) }()

	select {
	case opt := <-applied:
		require.Equal(t, "v0.2.0", Normalize(opt.Version))
	case <-time.After(2 * time.Second):
		t.Fatal("upgrade not applied")
	}
	require.Eventually(t, func() bool {
		m, err := store.GetMachine(ctx, st, "m1")
		return err == nil && m.UpgradeStatus == "pending_restart"
	}, 2*time.Second, 20*time.Millisecond)

	// Must not re-enter Apply while waiting for restart (loop regression).
	time.Sleep(80 * time.Millisecond)
	require.Empty(t, applied)
}

func buildReleaseTar(t *testing.T, dir string) []byte {
	t.Helper()
	path := filepath.Join(dir, "rel.tgz")
	f, err := os.Create(path)
	require.NoError(t, err)
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	for _, name := range []string{"aetherixd", "ax"} {
		body := []byte("#!/bin/sh\necho " + name + "\n")
		require.NoError(t, tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(body))}))
		_, err = tw.Write(body)
		require.NoError(t, err)
	}
	require.NoError(t, tw.Close())
	require.NoError(t, gz.Close())
	require.NoError(t, f.Close())
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	return b
}
