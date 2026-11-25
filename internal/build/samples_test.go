package build

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func sampleAppsDir(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	require.True(t, ok)
	root := filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
	dir := filepath.Join(root, "testdata", "apps")
	st, err := os.Stat(dir)
	require.NoError(t, err)
	require.True(t, st.IsDir())
	return dir
}

func cmdsJSON(cmds []string) string {
	if len(cmds) == 0 {
		return "[]"
	}
	parts := make([]string, len(cmds))
	for i, c := range cmds {
		parts[i] = `{"cmd":"` + c + `"}`
	}
	return "[" + strings.Join(parts, ",") + "]"
}

// languageAwareRailpackFake mimics `railpack prepare` based on source markers.
func languageAwareRailpackFake(c Cmd, _ io.Writer) error {
	if c.Name != "railpack" {
		return nil
	}
	src := ""
	if len(c.Args) > 1 {
		src = c.Args[1]
	}
	lang, start := "unknown", "echo start"
	var install, build []string
	switch {
	case fileExists(filepath.Join(src, "package.json")):
		lang, start, install, build = "node", "node server.js", []string{"npm ci"}, []string{"npm run build"}
	case fileExists(filepath.Join(src, "requirements.txt")) || fileExists(filepath.Join(src, "pyproject.toml")):
		lang, start, install = "python", "python main.py", []string{"pip install -r requirements.txt"}
	case fileExists(filepath.Join(src, "go.mod")):
		lang, start, install, build = "go", "./app", []string{"go mod download"}, []string{"go build -o app ."}
	case fileExists(filepath.Join(src, "index.html")):
		lang, start = "static", "npx serve -s ."
	}
	var steps []string
	if len(install) > 0 {
		steps = append(steps, `{"name":"install","commands":`+cmdsJSON(install)+`}`)
	}
	if len(build) > 0 {
		steps = append(steps, `{"name":"build","commands":`+cmdsJSON(build)+`}`)
	}
	plan := `{"steps":[` + strings.Join(steps, ",") + `],"deploy":{"startCommand":"` + start + `"}}`
	info := `{"detectedProviders":["` + lang + `"],"metadata":{"runtime":"` + lang + `"},"resolvedPackages":{"` + lang + `":{"resolvedVersion":"1.0.0"}}}`
	if err := os.WriteFile(argAfter(c.Args, "--plan-out"), []byte(plan), 0o644); err != nil {
		return err
	}
	return os.WriteFile(argAfter(c.Args, "--info-out"), []byte(info), 0o644)
}

func TestSampleAppsDetectRailpack(t *testing.T) {
	base := sampleAppsDir(t)
	for _, name := range []string{"node", "python", "go", "static"} {
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join(base, name)
			p, err := Detect(dir, Settings{})
			require.NoError(t, err)
			require.Equal(t, ProviderRailpack, p.Provider)
			require.Contains(t, p.Reason, "Railpack")
			require.Empty(t, p.Dockerfile)
		})
	}
}

func TestSampleAppsPipelineWithFakes(t *testing.T) {
	base := sampleAppsDir(t)
	want := map[string]struct {
		lang  string
		start string
	}{
		"node":   {lang: "node", start: "node server.js"},
		"python": {lang: "python", start: "python main.py"},
		"go":     {lang: "go", start: "./app"},
		"static": {lang: "static", start: "npx serve -s ."},
	}
	for name, w := range want {
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join(base, name)
			bk := &fakeBK{}
			p := &Pipeline{
				BK: bk, Runner: &fakeRunner{fn: languageAwareRailpackFake},
				WorkDir: t.TempDir(), FrontendImage: DefaultFrontendImage + ":test",
			}
			res, err := p.Run(context.Background(), Job{
				SrcDir: dir, Project: "samples", Service: name, Tag: "t1",
			})
			require.NoError(t, err)
			require.Equal(t, ProviderRailpack, res.Provider)
			require.Equal(t, w.lang, res.Detected.Language)
			require.Equal(t, w.start, res.Detected.StartCommand)
			require.Equal(t, "sha256:abc", res.Digest)
			require.Contains(t, res.Image, "samples/"+name+":")
			require.Len(t, bk.reqs, 1)
			require.Equal(t, "gateway.v0", bk.reqs[0].Frontend)
			require.Equal(t, RailpackPlanFile, bk.reqs[0].Filename)
		})
	}
}

func TestSampleAppsPreview(t *testing.T) {
	base := sampleAppsDir(t)
	p := &Pipeline{Runner: &fakeRunner{fn: languageAwareRailpackFake}, WorkDir: t.TempDir()}
	pv, err := p.Preview(context.Background(), filepath.Join(base, "node"), Settings{}, nil)
	require.NoError(t, err)
	require.Equal(t, ProviderRailpack, pv.Plan.Provider)
	require.Equal(t, "node", pv.Detected.Language)
	require.Equal(t, "node server.js", pv.Detected.StartCommand)
}

func TestSampleAppsDockerfileStillWins(t *testing.T) {
	base := sampleAppsDir(t)
	// Copy node sample into a temp dir and add a Dockerfile — detection must prefer it.
	dst := t.TempDir()
	src := filepath.Join(base, "node")
	ents, err := os.ReadDir(src)
	require.NoError(t, err)
	for _, e := range ents {
		b, err := os.ReadFile(filepath.Join(src, e.Name()))
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(dst, e.Name()), b, 0o644))
	}
	require.NoError(t, os.WriteFile(filepath.Join(dst, "Dockerfile"), []byte("FROM node:20\n"), 0o644))
	p, err := Detect(dst, Settings{})
	require.NoError(t, err)
	require.Equal(t, ProviderDockerfile, p.Provider)
}
