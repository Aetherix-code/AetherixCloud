package build

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	v1 "github.com/aetherix/aetherixcloud/gen/aetherix/v1"
	"github.com/aetherix/aetherixcloud/internal/deploy"
	"github.com/aetherix/aetherixcloud/internal/lease"
	"github.com/aetherix/aetherixcloud/internal/store"
	"github.com/stretchr/testify/require"
)

// ---- fakes

type fakeRunner struct {
	mu   sync.Mutex
	cmds []Cmd
	fn   func(c Cmd, out io.Writer) error
}

func (f *fakeRunner) Run(_ context.Context, c Cmd, out io.Writer) error {
	f.mu.Lock()
	f.cmds = append(f.cmds, c)
	f.mu.Unlock()
	if f.fn != nil {
		return f.fn(c, out)
	}
	return nil
}

func argAfter(args []string, flag string) string {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

// railpackFake writes plan and info files like `railpack prepare` would.
func railpackFake(c Cmd, out io.Writer) error {
	if c.Name != "railpack" {
		return nil
	}
	plan := `{"steps":[{"name":"install","commands":[{"cmd":"npm ci"}]},{"name":"build","commands":[{"cmd":"npm run build"}]}],"deploy":{"startCommand":"node server.js"},"secrets":["API_KEY"]}`
	info := `{"detectedProviders":["node"],"metadata":{"nodeRuntime":"node"},"resolvedPackages":{"node":{"resolvedVersion":"22.1.0"}}}`
	if err := os.WriteFile(argAfter(c.Args, "--plan-out"), []byte(plan), 0o644); err != nil {
		return err
	}
	return os.WriteFile(argAfter(c.Args, "--info-out"), []byte(info), 0o644)
}

type fakeBK struct {
	mu   sync.Mutex
	reqs []BuildRequest
	err  error
}

func (f *fakeBK) Build(_ context.Context, r BuildRequest, out io.Writer) (BuildResult, error) {
	f.mu.Lock()
	f.reqs = append(f.reqs, r)
	f.mu.Unlock()
	io.WriteString(out, "#1 building\n")
	if f.err != nil {
		return BuildResult{}, f.err
	}
	return BuildResult{Digest: "sha256:abc"}, nil
}

func write(t *testing.T, dir, name, content string) {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(name))
	require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
	require.NoError(t, os.WriteFile(p, []byte(content), 0o644))
}

// ---- detection

func TestDetectOrder(t *testing.T) {
	// nothing -> railpack
	d := t.TempDir()
	p, err := Detect(d, Settings{})
	require.NoError(t, err)
	require.Equal(t, ProviderRailpack, p.Provider)

	// Dockerfile wins over compose
	write(t, d, "Dockerfile", "FROM scratch")
	write(t, d, "compose.yaml", "services:\n  web:\n    build: .\n")
	p, err = Detect(d, Settings{ServiceName: "web"})
	require.NoError(t, err)
	require.Equal(t, ProviderDockerfile, p.Provider)
	require.Equal(t, filepath.Join(d, "Dockerfile"), p.Dockerfile)

	// explicit railpack beats Dockerfile
	p, err = Detect(d, Settings{Builder: "railpack"})
	require.NoError(t, err)
	require.Equal(t, ProviderRailpack, p.Provider)

	// explicit image
	_, err = Detect(d, Settings{Builder: "image"})
	require.ErrorIs(t, err, ErrNothingToBuild)

	// explicit dockerfile, missing
	_, err = Detect(t.TempDir(), Settings{Builder: "dockerfile"})
	require.Error(t, err)
}

func TestDetectCompose(t *testing.T) {
	d := t.TempDir()
	write(t, d, "docker-compose.yml", `services:
  api:
    build:
      context: ./api
      dockerfile: Dockerfile.prod
      target: runtime
      args:
        - A=1
  db:
    image: postgres
  worker:
    build: ./worker
`)
	p, err := Detect(d, Settings{ServiceName: "api", Args: map[string]string{"B": "2"}})
	require.NoError(t, err)
	require.Equal(t, ProviderCompose, p.Provider)
	require.Equal(t, "api", p.ComposeService)
	require.Equal(t, filepath.Join(d, "api", "Dockerfile.prod"), p.Dockerfile)
	require.Equal(t, filepath.Join(d, "api"), p.ContextDir)
	require.Equal(t, "runtime", p.Target)
	require.Equal(t, map[string]string{"A": "1", "B": "2"}, p.Args)
	require.Len(t, p.Compose, 2)

	// ambiguous name -> falls through to Railpack but still lists builds
	p, err = Detect(d, Settings{ServiceName: "other"})
	require.NoError(t, err)
	require.Equal(t, ProviderRailpack, p.Provider)
	require.Len(t, p.Compose, 2)
}

func TestDetectRootDirAndEscape(t *testing.T) {
	d := t.TempDir()
	write(t, d, "apps/web/Dockerfile", "FROM scratch")
	p, err := Detect(d, Settings{RootDir: "apps/web"})
	require.NoError(t, err)
	require.Equal(t, ProviderDockerfile, p.Provider)
	require.Equal(t, filepath.Join(d, "apps", "web"), p.ContextDir)
	_, err = Detect(d, Settings{RootDir: "../etc"})
	require.Error(t, err)
	_, err = Detect(d, Settings{RootDir: "apps/web", Dockerfile: "../../../x"})
	require.Error(t, err)
	_, err = Detect(d, Settings{RootDir: "nope"})
	require.Error(t, err)
	// configured Dockerfile that does not exist is an error, not a silent Railpack
	_, err = Detect(d, Settings{RootDir: "apps/web", Dockerfile: "Dockerfile.missing"})
	require.Error(t, err)
}

// ---- buildctl args

func TestBuildctlArgs(t *testing.T) {
	b := &Buildctl{Addr: "unix:///x.sock"}
	args, env, redact := b.Args(BuildRequest{Frontend: "gateway.v0", FrontendSource: "ghcr.io/railwayapp/railpack-frontend",
		ContextDir: "/src", DockerfileDir: "/plan", Filename: "railpack-plan.json", Platform: "linux/arm64", Target: "t",
		Args: map[string]string{"A": "1"}, Secrets: map[string]string{"API-KEY": "s3cr3t"},
		Image: "127.0.0.1:5000/p/s:t", CacheRef: "127.0.0.1:5000/ax-cache/p/s:buildcache"}, "/meta.json")
	joined := strings.Join(args, " ")
	for _, want := range []string{"--addr unix:///x.sock", "--frontend gateway.v0", "--opt source=ghcr.io/railwayapp/railpack-frontend",
		"--local context=/src", "--local dockerfile=/plan", "--opt filename=railpack-plan.json", "--opt platform=linux/arm64",
		"--opt target=t", "--opt build-arg:A=1", "--secret id=API-KEY,env=AXSECRET_API_KEY",
		"--output type=image,name=127.0.0.1:5000/p/s:t,push=true,registry.insecure=true",
		"--import-cache type=registry,ref=127.0.0.1:5000/ax-cache/p/s:buildcache,registry.insecure=true",
		"--export-cache type=registry,ref=127.0.0.1:5000/ax-cache/p/s:buildcache,mode=max", "--metadata-file /meta.json"} {
		require.Contains(t, joined, want)
	}
	require.NotContains(t, joined, "s3cr3t", "secret values never appear in argv")
	require.Equal(t, []string{"AXSECRET_API_KEY=s3cr3t"}, env)
	require.Equal(t, []string{"s3cr3t"}, redact)
}

func TestBuildctlDigest(t *testing.T) {
	r := &fakeRunner{fn: func(c Cmd, out io.Writer) error {
		return os.WriteFile(argAfter(c.Args, "--metadata-file"), []byte(`{"containerimage.digest":"sha256:feed"}`), 0o644)
	}}
	res, err := (&Buildctl{Runner: r}).Build(context.Background(), BuildRequest{Frontend: "dockerfile.v0", Image: "x"}, io.Discard)
	require.NoError(t, err)
	require.Equal(t, "sha256:feed", res.Digest)
}

func TestImageRef(t *testing.T) {
	require.Equal(t, "127.0.0.1:5000/my-proj/web_app:abc1234-x", ImageRef("My Proj", "Web_App", "abc1234-x"))
	require.Equal(t, "127.0.0.1:5000/ax-cache/p/s:buildcache", CacheRefFor("p", "s"))
}

// ---- pipeline

func TestPipelineDockerfile(t *testing.T) {
	d := t.TempDir()
	write(t, d, "Dockerfile", "FROM scratch")
	bk := &fakeBK{}
	p := &Pipeline{BK: bk, Runner: &fakeRunner{}}
	var log bytes.Buffer
	res, err := p.Run(context.Background(), Job{SrcDir: d, Settings: Settings{Args: map[string]string{"X": "1"}, Platform: "linux/amd64"},
		Project: "proj", Service: "web", Tag: "t1", Secrets: map[string]string{"TOKEN": "v"}, Log: &log})
	require.NoError(t, err)
	require.Equal(t, ProviderDockerfile, res.Provider)
	require.Equal(t, "127.0.0.1:5000/proj/web:t1", res.Image)
	require.Equal(t, "sha256:abc", res.Digest)
	r := bk.reqs[0]
	require.Equal(t, "dockerfile.v0", r.Frontend)
	require.Equal(t, d, r.ContextDir)
	require.Equal(t, "Dockerfile", r.Filename)
	require.Equal(t, "linux/amd64", r.Platform)
	require.Equal(t, map[string]string{"TOKEN": "v"}, r.Secrets)
	require.Equal(t, "127.0.0.1:5000/ax-cache/proj/web:buildcache", r.CacheRef)
	require.Contains(t, log.String(), "detected build type: dockerfile")
}

func TestPipelineRailpack(t *testing.T) {
	d := t.TempDir()
	write(t, d, "package.json", "{}")
	rr := &fakeRunner{fn: railpackFake}
	bk := &fakeBK{}
	p := &Pipeline{BK: bk, Runner: rr, WorkDir: t.TempDir(), FrontendImage: "ghcr.io/railwayapp/railpack-frontend:v0.9.0"}
	res, err := p.Run(context.Background(), Job{SrcDir: d, Settings: Settings{BuildCommand: "npm run b", StartCommand: "node x.js",
		Args: map[string]string{"NODE_VERSION": "22"}}, Project: "p", Service: "s", Tag: "t",
		Secrets: map[string]string{"API_KEY": "topsecret"}})
	require.NoError(t, err)
	require.Equal(t, ProviderRailpack, res.Provider)
	require.Equal(t, "node", res.Detected.Language)
	require.Equal(t, "22.1.0", res.Detected.Packages["node"])
	require.Equal(t, "node server.js", res.Detected.StartCommand)
	require.Equal(t, []string{"npm ci"}, res.Detected.InstallCmds)
	require.Equal(t, []string{"npm run build"}, res.Detected.BuildCmds)

	prep := rr.cmds[0]
	require.Equal(t, "railpack", prep.Name)
	require.Equal(t, "prepare", prep.Args[0])
	require.Equal(t, d, prep.Args[1])
	require.Equal(t, "npm run b", argAfter(prep.Args, "--build-cmd"))
	require.Equal(t, "node x.js", argAfter(prep.Args, "--start-cmd"))
	require.Contains(t, prep.Args, "NODE_VERSION=22")
	require.Contains(t, prep.Args, "API_KEY=topsecret")
	require.NotContains(t, prep.String(), "topsecret", "railpack command line is redacted for logs")

	r := bk.reqs[0]
	require.Equal(t, "gateway.v0", r.Frontend)
	require.Equal(t, "ghcr.io/railwayapp/railpack-frontend:v0.9.0", r.FrontendSource)
	require.Equal(t, RailpackPlanFile, r.Filename)
	require.Equal(t, d, r.ContextDir)
	require.NotEqual(t, d, r.DockerfileDir)
	require.Equal(t, map[string]string{"API_KEY": "topsecret"}, r.Secrets)
	require.Empty(t, r.Args)
}

func TestPipelineFailureAndPreview(t *testing.T) {
	d := t.TempDir()
	write(t, d, "Dockerfile", "FROM scratch")
	p := &Pipeline{BK: &fakeBK{err: io.ErrUnexpectedEOF}, Runner: &fakeRunner{}}
	_, err := p.Run(context.Background(), Job{SrcDir: d, Project: "p", Service: "s", Tag: "t"})
	require.ErrorIs(t, err, io.ErrUnexpectedEOF)

	rd := t.TempDir()
	pv, err := (&Pipeline{Runner: &fakeRunner{fn: railpackFake}, WorkDir: t.TempDir()}).Preview(context.Background(), rd, Settings{}, nil)
	require.NoError(t, err)
	require.Equal(t, ProviderRailpack, pv.Plan.Provider)
	require.Equal(t, "node", pv.Detected.Language)

	pv, err = (&Pipeline{}).Preview(context.Background(), d, Settings{}, nil)
	require.NoError(t, err)
	require.Equal(t, ProviderDockerfile, pv.Plan.Provider)
	require.Nil(t, pv.Detected)
}

// ---- source tarballs

func TestTarRoundTripAndTraversal(t *testing.T) {
	src := t.TempDir()
	write(t, src, "a/b.txt", "hello")
	write(t, src, "node_modules/x/y.js", "no")
	write(t, src, "skip.log", "no")
	write(t, src, ".DS_Store", "no")
	var buf bytes.Buffer
	require.NoError(t, TarDir(&buf, src, []string{"*.log"}))
	dst := t.TempDir()
	require.NoError(t, ExtractTar(&buf, dst, 0))
	b, err := os.ReadFile(filepath.Join(dst, "a", "b.txt"))
	require.NoError(t, err)
	require.Equal(t, "hello", string(b))
	for _, gone := range []string{"node_modules", "skip.log", ".DS_Store"} {
		_, err := os.Stat(filepath.Join(dst, gone))
		require.True(t, os.IsNotExist(err), gone)
	}

	// path traversal is rejected
	evil := mkTar(t, "../../evil.txt", "x")
	require.Error(t, ExtractTar(evil, t.TempDir(), 0))
	// size cap
	require.Error(t, ExtractTar(mkTar(t, "big", strings.Repeat("a", 100)), t.TempDir(), 10))
}

// ---- logs

func TestLogStoreLiveAndReplay(t *testing.T) {
	ls, err := NewLogStore(t.TempDir())
	require.NoError(t, err)
	w, err := ls.Writer("b1")
	require.NoError(t, err)
	w.Write([]byte("one\ntw"))
	got := make(chan string, 16)
	done := make(chan error, 1)
	go func() {
		done <- ls.Stream(context.Background(), "b1", true, func(l LogLine) error { got <- l.Stream + ":" + l.Line; return nil })
	}()
	require.Equal(t, "stdout:one", <-got) // replayed from memory
	w.Write([]byte("o\nthree\n"))
	w.System("done")
	require.Equal(t, "stdout:two", <-got)
	require.Equal(t, "stdout:three", <-got)
	require.Equal(t, "system:done", <-got)
	require.NoError(t, w.Close())
	require.NoError(t, <-done)

	var lines []string
	require.NoError(t, ls.Stream(context.Background(), "b1", false, func(l LogLine) error { lines = append(lines, l.Line); return nil }))
	require.Equal(t, []string{"one", "two", "three", "done"}, lines)
	require.ErrorIs(t, ls.Stream(context.Background(), "nope", false, func(LogLine) error { return nil }), ErrNoLog)
}

// ---- builder selection

func TestRankBuilders(t *testing.T) {
	builder := []v1.MachineRole{v1.MachineRole_MACHINE_ROLE_BUILDER}
	worker := []v1.MachineRole{v1.MachineRole_MACHINE_ROLE_WORKER}
	ms := []*v1.Machine{
		{Id: "m3", Roles: builder}, {Id: "m1", Roles: builder}, {Id: "m2", Roles: builder},
		{Id: "m4", Roles: builder, State: v1.MachineState_MACHINE_STATE_DOWN},
		{Id: "m5", Roles: worker},
	}
	require.Equal(t, []string{"m1", "m2", "m3"}, RankBuilders(ms, nil, ""))
	require.Equal(t, []string{"m3", "m1", "m2"}, RankBuilders(ms, map[string]int{"m1": 2, "m2": 2}, ""))
	// no builder role anywhere -> every live machine qualifies
	ms2 := []*v1.Machine{{Id: "a", Roles: worker}, {Id: "b", Roles: worker, State: v1.MachineState_MACHINE_STATE_DOWN}}
	require.Equal(t, []string{"a"}, RankBuilders(ms2, nil, ""))
	// self stays eligible even when membership marked it DOWN
	down := []*v1.Machine{{Id: "solo", State: v1.MachineState_MACHINE_STATE_DOWN, Roles: builder}}
	require.Equal(t, []string{"solo"}, RankBuilders(down, nil, "solo"))
	require.Empty(t, RankBuilders(down, nil, ""))
}

// ---- worker end to end

type fakeSource struct{ dockerfile bool }

func (f fakeSource) Fetch(_ context.Context, j *JobMeta, dir string, log io.Writer) (SourceInfo, error) {
	io.WriteString(log, "cloned "+j.Repo+"\n")
	if f.dockerfile {
		if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM scratch"), 0o644); err != nil {
			return SourceInfo{}, err
		}
	}
	return SourceInfo{SHA: "0123456789abcdef", Message: "msg", Author: "me"}, nil
}

type recReporter struct {
	mu sync.Mutex
	ev []string
}

func (r *recReporter) Report(_ context.Context, _ *JobMeta, e Event) {
	r.mu.Lock()
	r.ev = append(r.ev, e.Stage+":"+e.State)
	r.mu.Unlock()
}
func (r *recReporter) events() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.ev...)
}

func newWorker(t *testing.T, bk BuildKit, src Source, rep Reporter) (*Worker, store.Store) {
	t.Helper()
	st, err := store.OpenSQLite(":memory:")
	require.NoError(t, err)
	require.NoError(t, st.Migrate(context.Background()))
	t.Cleanup(func() { st.Close() })
	logs, err := NewLogStore(t.TempDir())
	require.NoError(t, err)
	w := &Worker{Store: st, Leases: &lease.Manager{Store: st, Self: "m1"}, Self: "m1",
		Pipeline: &Pipeline{BK: bk, Runner: &fakeRunner{fn: railpackFake}, WorkDir: t.TempDir()},
		Logs:     logs, Source: src, Reporter: rep, WorkDir: t.TempDir(), Poll: 50 * time.Millisecond, DeployWait: time.Second}
	require.NoError(t, store.PutMachine(context.Background(), st, &v1.Machine{Id: "m1", Name: "m1",
		Roles: []v1.MachineRole{v1.MachineRole_MACHINE_ROLE_BUILDER}}))
	require.NoError(t, store.PutProject(context.Background(), st, &v1.Project{Id: "p1", Name: "proj"}))
	return w, st
}

func putSvc(t *testing.T, st store.Store) *v1.Service {
	svc := &v1.Service{Id: "s1", ProjectId: "p1", EnvironmentId: "e1", Name: "web",
		Spec: &v1.ServiceSpec{Replicas: 1, Source: &v1.ServiceSource{Github: &v1.GitHubSource{Repo: "o/r", Branch: "main"}}}}
	require.NoError(t, store.PutService(context.Background(), st, svc))
	return svc
}

func waitFor(t *testing.T, f func() bool) {
	t.Helper()
	require.Eventually(t, f, 10*time.Second, 25*time.Millisecond)
}

func TestWorkerBuildsAndDeploys(t *testing.T) {
	bk := &fakeBK{}
	rep := &recReporter{}
	w, st := newWorker(t, bk, fakeSource{dockerfile: true}, rep)
	var deployed *deploy.StartOptions
	var mu sync.Mutex
	w.Deploy = func(ctx context.Context, s store.Store, svc *v1.Service, o deploy.StartOptions) (*v1.Deployment, error) {
		mu.Lock()
		deployed = &o
		mu.Unlock()
		dep := &v1.Deployment{Id: "d1", ServiceId: svc.Id, Spec: svc.Spec, Status: v1.DeploymentStatus_DEPLOYMENT_STATUS_ACTIVE}
		return dep, store.PutDeployment(ctx, s, dep)
	}
	svc := putSvc(t, st)
	require.NoError(t, store.PutProto(context.Background(), st, store.TableVariables, "v1",
		map[string]string{"environment_id": "e1", "service_id": "", "key": BuildSecretPrefix + "NPM_TOKEN"},
		&v1.Variable{Id: "v1", EnvironmentId: "e1", Key: BuildSecretPrefix + "NPM_TOKEN", Value: "tok"}))
	require.NoError(t, store.PutProto(context.Background(), st, store.TableVariables, "v2",
		map[string]string{"environment_id": "e1", "service_id": "", "key": "RUNTIME_ONLY"},
		&v1.Variable{Id: "v2", EnvironmentId: "e1", Key: "RUNTIME_ONLY", Value: "x"}))

	b, created, err := Enqueue(context.Background(), st, JobMeta{ServiceID: svc.Id, Kind: "github", Repo: "o/r", Ref: "refs/heads/main", SHA: "0123456789abcdef"})
	require.NoError(t, err)
	require.True(t, created)
	// idempotent
	b2, created, err := Enqueue(context.Background(), st, JobMeta{BuildID: b.Id, ServiceID: svc.Id})
	require.NoError(t, err)
	require.False(t, created)
	require.Equal(t, b.Id, b2.Id)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Run(ctx)
	waitFor(t, func() bool {
		cur, _ := GetBuild(ctx, st, b.Id)
		return cur != nil && cur.Status == StatusSucceeded
	})
	cur, _ := GetBuild(ctx, st, b.Id)
	require.Equal(t, "m1", cur.BuilderMachineId)
	require.Equal(t, "dockerfile", cur.Provider)
	require.Equal(t, "127.0.0.1:5000/proj/web:0123456-"+b.Id[len(b.Id)-8:], cur.Image)

	waitFor(t, func() bool { mu.Lock(); defer mu.Unlock(); return deployed != nil })
	require.Equal(t, "sha256:abc", deployed.ImageDigest)
	require.Equal(t, b.Id, deployed.BuildID)
	require.Equal(t, "msg", deployed.CommitMsg)
	// only BUILD_SECRET_* variables reach the build, prefix stripped
	require.Equal(t, map[string]string{"NPM_TOKEN": "tok"}, bk.reqs[0].Secrets)
	// the service now points at the built image
	s2, _ := store.GetService(ctx, st, svc.Id)
	require.Equal(t, cur.Image, s2.Spec.Image)

	waitFor(t, func() bool { return len(rep.events()) >= 4 })
	require.Equal(t, []string{"build:pending", "build:success", "deploy:pending", "deploy:success"}, rep.events())

	// log was kept
	var lines []string
	require.NoError(t, w.Logs.Stream(ctx, b.Id, false, func(l LogLine) error { lines = append(lines, l.Line); return nil }))
	require.Contains(t, strings.Join(lines, "\n"), "build succeeded")
	require.Contains(t, strings.Join(lines, "\n"), "cloned o/r")
}

func TestWorkerFailureAndLeaseExclusivity(t *testing.T) {
	bk := &fakeBK{err: io.ErrUnexpectedEOF}
	rep := &recReporter{}
	w, st := newWorker(t, bk, fakeSource{dockerfile: true}, rep)
	svc := putSvc(t, st)
	b, _, err := Enqueue(context.Background(), st, JobMeta{ServiceID: svc.Id, Kind: "github", Repo: "o/r", Ref: "refs/heads/main", SHA: "abc"})
	require.NoError(t, err)

	// another machine already holds the build lease: this worker must not run it.
	other := &lease.Manager{Store: st, Self: "m0"}
	l, err := other.Acquire(context.Background(), "build:"+b.Id, time.Minute)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Run(ctx)
	time.Sleep(400 * time.Millisecond)
	cur, _ := GetBuild(ctx, st, b.Id)
	require.Equal(t, StatusQueued, cur.Status)
	require.Empty(t, bk.reqs)

	require.NoError(t, l.Release(ctx))
	waitFor(t, func() bool {
		cur, _ := GetBuild(ctx, st, b.Id)
		return cur.Status == StatusFailed
	})
	require.Equal(t, []string{"build:pending", "build:failure"}, rep.events())
	var lines []string
	require.NoError(t, w.Logs.Stream(ctx, b.Id, false, func(l LogLine) error { lines = append(lines, l.Line); return nil }))
	require.Contains(t, strings.Join(lines, "\n"), "build failed")
}

func TestReapStale(t *testing.T) {
	w, st := newWorker(t, &fakeBK{}, fakeSource{}, nil)
	w.defaults()
	w.LeaseTTL = time.Millisecond // anything older than 2ms without a lease is stale
	svc := putSvc(t, st)
	b := &v1.Build{Id: "bx", ServiceId: svc.Id, Status: StatusRunning, BuilderMachineId: "dead"}
	require.NoError(t, PutBuild(context.Background(), st, b))
	time.Sleep(20 * time.Millisecond)
	w.reapStale(context.Background())
	cur, _ := GetBuild(context.Background(), st, "bx")
	require.Equal(t, StatusFailed, cur.Status)
}

func TestDeterministicBuildID(t *testing.T) {
	require.Equal(t, DeterministicBuildID("d", "s"), DeterministicBuildID("d", "s"))
	require.NotEqual(t, DeterministicBuildID("d", "s"), DeterministicBuildID("d", "s2"))
	require.NotEqual(t, DeterministicBuildID("d", "s"), DeterministicBuildID("d2", "s"))
}
