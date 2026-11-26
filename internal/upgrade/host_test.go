package upgrade

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSyncHostDepsSkipped(t *testing.T) {
	t.Setenv("AX_SKIP_HOST_DEPS", "1")
	require.NoError(t, SyncHostDeps(context.Background(), HostOptions{}))
}

func TestReadReleaseFile(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "deploy"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "deploy", "buildkitd.toml"), []byte("x=1\n"), 0o644))
	b, err := readReleaseFile(dir, "buildkitd.toml")
	require.NoError(t, err)
	require.Equal(t, "x=1\n", string(b))
	_, err = readReleaseFile(dir, "missing.toml")
	require.Error(t, err)
}

func TestHostConfigAndEnv(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("linux host deps")
	}
	binDir := t.TempDir()
	unitDir := t.TempDir()
	cfgDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(binDir, "railpack"), []byte("#!/bin/sh\necho railpack 8.8.8\n"), 0o755))
	require.True(t, versionAt(filepath.Join(binDir, "railpack"), "8.8.8"))
	require.False(t, versionAt(filepath.Join(binDir, "railpack"), "9.9.9"))

	opt := HostOptions{
		BinDir: binDir, UnitDir: unitDir, BuildkitConfigDir: cfgDir,
		Arch: runtime.GOARCH, RailpackVersion: "8.8.8",
	}.withDefaults()
	require.NoError(t, ensureBuildkitConfig(opt, opt.Logger))
	require.FileExists(t, filepath.Join(cfgDir, "buildkitd.toml"))
	require.FileExists(t, filepath.Join(unitDir, "buildkitd.service"))
	require.NoError(t, ensureBuilderEnv(opt, opt.Logger))
	body, err := os.ReadFile(filepath.Join(unitDir, "aetherixd.service.d", "builder.conf"))
	require.NoError(t, err)
	require.Contains(t, string(body), "railpack-frontend:v8.8.8")
	require.Contains(t, string(body), "AX_BUILDKIT_ADDR=")
}

func TestVerifyFileChecksum(t *testing.T) {
	dir := t.TempDir()
	payload := []byte("hello-railpack")
	path := filepath.Join(dir, "f.tgz")
	require.NoError(t, os.WriteFile(path, payload, 0o644))
	sum := sha256.Sum256(payload)
	sumFile := filepath.Join(dir, "checksums.txt")
	require.NoError(t, os.WriteFile(sumFile, []byte(hex.EncodeToString(sum[:])+"  f.tgz\n"), 0o644))
	require.NoError(t, verifyFileChecksum(sumFile, "f.tgz", path))
	require.Error(t, verifyFileChecksum(sumFile, "other.tgz", path))
}
