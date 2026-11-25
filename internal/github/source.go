package github

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/aetherix/aetherixcloud/internal/build"
)

// GitSource clones repositories with an installation access token. It
// implements build.Source.
type GitSource struct {
	Module *Module
	Runner build.Runner
	// CloneBase defaults to the module's web base (https://github.com).
	CloneBase string
}

type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

// infoMarker prefixes the machine-readable `git log` line.
const infoMarker = "AXINFO\x1f"

// Fetch implements build.Source: a shallow fetch of the requested commit (or
// ref) authenticated with an installation token passed through an HTTP header
// (never written to the remote URL, git config or logs).
func (g *GitSource) Fetch(ctx context.Context, job *build.JobMeta, dir string, log io.Writer) (build.SourceInfo, error) {
	cli, err := g.Module.Client(ctx)
	if err != nil {
		return build.SourceInfo{}, err
	}
	instID, err := g.Module.resolveInstallation(ctx, job.InstallationID, job.Repo)
	if err != nil {
		return build.SourceInfo{}, err
	}
	tok, err := cli.InstallationToken(ctx, instID)
	if err != nil {
		return build.SourceInfo{}, fmt.Errorf("installation token: %w", err)
	}
	base := strings.TrimRight(firstNonEmpty(g.CloneBase, g.Module.webBase()), "/")
	remote := fmt.Sprintf("%s/%s.git", base, job.Repo)
	basic := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + tok))
	hdr := "http.extraheader=AUTHORIZATION: basic " + basic
	env := []string{"GIT_TERMINAL_PROMPT=0"}
	redact := []string{basic, tok}
	run := func(out io.Writer, args ...string) error {
		return g.Runner.Run(ctx, build.Cmd{Name: "git", Args: args, Dir: dir, Env: env, Redact: redact}, out)
	}
	if err := run(log, "init", "--quiet", dir); err != nil {
		return build.SourceInfo{}, err
	}
	fetch := func(what string) error {
		return run(log, "-c", hdr, "fetch", "--quiet", "--depth", "1", "--no-tags", remote, what)
	}
	switch {
	case job.SHA != "":
		if err := fetch(job.SHA); err != nil {
			if job.Ref == "" {
				return build.SourceInfo{}, fmt.Errorf("fetch %s: %w", job.SHA, err)
			}
			fmt.Fprintf(log, "fetching %s by SHA failed, trying %s\n", short(job.SHA), job.Ref)
			if err := fetch(job.Ref); err != nil {
				return build.SourceInfo{}, fmt.Errorf("fetch %s: %w", job.Ref, err)
			}
		}
	case job.Ref != "":
		if err := fetch(job.Ref); err != nil {
			return build.SourceInfo{}, fmt.Errorf("fetch %s: %w", job.Ref, err)
		}
	default:
		return build.SourceInfo{}, fmt.Errorf("build job has neither sha nor ref")
	}
	if err := run(log, "checkout", "--quiet", "--detach", "FETCH_HEAD"); err != nil {
		return build.SourceInfo{}, err
	}
	var out lockedBuf
	if err := run(&out, "log", "-1", "--format="+infoMarker+"%H%x1f%an%x1f%s"); err != nil {
		return build.SourceInfo{}, err
	}
	return parseInfo(out.b.String(), job), nil
}

func parseInfo(out string, job *build.JobMeta) build.SourceInfo {
	info := build.SourceInfo{SHA: job.SHA, Message: job.Message, Author: job.Author}
	for _, line := range strings.Split(out, "\n") {
		if rest, ok := strings.CutPrefix(line, infoMarker); ok {
			p := strings.SplitN(rest, "\x1f", 3)
			if len(p) == 3 {
				info.SHA, info.Author, info.Message = p[0], p[1], p[2]
			}
		}
	}
	return info
}

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}

// resolveInstallation returns id, or finds the installation that can access repo.
func (m *Module) resolveInstallation(ctx context.Context, id int64, repo string) (int64, error) {
	if id != 0 {
		return id, nil
	}
	ins, err := listInstallations(ctx, m.Store)
	if err != nil {
		return 0, err
	}
	if len(ins) == 1 {
		return ins[0].Id, nil
	}
	c, err := m.Client(ctx)
	if err != nil {
		return 0, err
	}
	for _, in := range ins {
		repos, err := c.ListRepos(ctx, in.Id)
		if err != nil {
			continue
		}
		for _, r := range repos {
			if strings.EqualFold(r.FullName, repo) {
				return in.Id, nil
			}
		}
	}
	return 0, fmt.Errorf("no GitHub App installation has access to %s", repo)
}
