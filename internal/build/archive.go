package build

import (
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	v1 "github.com/aetherix/aetherixcloud/gen/aetherix/v1"
	"github.com/aetherix/aetherixcloud/internal/backup"
	"github.com/aetherix/aetherixcloud/internal/store"
)

// archiveImage uploads a docker save of image when cluster Settings → Build
// images (or legacy per-service BuildSpec retain) is enabled, then prunes
// older S3 archives and local registry tags beyond retain.
func (w *Worker) archiveImage(ctx context.Context, svc *v1.Service, image, digest string, log io.Writer) {
	bs := svc.Spec.GetBuild()
	overrideRetain, overrideTarget := 0, ""
	if bs != nil {
		overrideRetain = int(bs.ImageRetain)
		overrideTarget = bs.ImageS3TargetId
	}
	enabled, retain, targetID, err := ResolveImageArchive(ctx, w.Store, overrideRetain, overrideTarget)
	if err != nil || !enabled {
		return
	}
	tgt, err := backup.ResolveTarget(ctx, w.Store, targetID)
	if err != nil {
		fmt.Fprintf(log, "==> image archive skipped: %v\n", err)
		return
	}
	s3 := tgt.S3()
	if !s3.Valid() {
		fmt.Fprintf(log, "==> image archive skipped: S3 target %q is incomplete\n", tgt.Name)
		return
	}

	proj, _ := store.GetProto[v1.Project](ctx, w.Store, store.TableProjects, svc.ProjectId)
	projName := svc.ProjectId
	if proj != nil && proj.Name != "" {
		projName = proj.Name
	}
	objName := archiveObjectName(digest, image)
	key := s3ImageKey(s3, projName, svc.Name, objName)

	fmt.Fprintf(log, "==> archiving image to s3://%s/%s (retain %d)\n", tgt.Bucket, key, retain)
	if err := w.uploadImageArchive(ctx, image, s3, key, log); err != nil {
		fmt.Fprintf(log, "==> image archive failed: %v\n", err)
		w.Logger.Warn("image archive failed", "service", svc.Id, "image", image, "err", err)
		return
	}
	fmt.Fprintf(log, "==> image archived (%s)\n", key)
	prefix := s3ImageKey(s3, projName, svc.Name, "") // ends with /
	pruneImageArchives(ctx, s3, prefix, retain, w.Logger)
	w.pruneLocalImages(ctx, projName, svc.Name, image, retain, log)
}

func archiveObjectName(digest, image string) string {
	if d := strings.TrimPrefix(digest, "sha256:"); d != "" && len(d) >= 12 {
		return d[:12] + ".tar.gz"
	}
	// Fall back to the tag portion of the ref.
	tag := image
	if i := strings.LastIndex(image, ":"); i >= 0 {
		tag = image[i+1:]
	}
	tag = sanitizePath(tag)
	if tag == "" {
		tag = "image"
	}
	return tag + ".tar.gz"
}

func s3ImageKey(s3 backup.S3Config, project, service, file string) string {
	// Mirror backup.S3Config.key layout without exporting it: prefix/images/...
	parts := []string{"images", sanitizePath(project), sanitizePath(service)}
	if file != "" {
		parts = append(parts, file)
	} else {
		// directory prefix for ListObjects
		p := joinS3(s3, parts...)
		if !strings.HasSuffix(p, "/") {
			p += "/"
		}
		return p
	}
	return joinS3(s3, parts...)
}

func joinS3(s3 backup.S3Config, parts ...string) string {
	rest := path.Join(parts...)
	p := strings.Trim(s3.Prefix, "/")
	if p == "" {
		return rest
	}
	return p + "/" + rest
}

func sanitizePath(s string) string {
	s = strings.TrimSpace(s)
	s = strings.ToLower(s)
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-.")
	if out == "" {
		return "x"
	}
	return out
}

func (w *Worker) uploadImageArchive(ctx context.Context, image string, s3 backup.S3Config, key string, log io.Writer) error {
	run := w.Pipeline.Runner
	if run == nil {
		return fmt.Errorf("no command runner")
	}
	dir, err := os.MkdirTemp(w.WorkDir, "ax-archive-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	tarPath := filepath.Join(dir, "image.tar")
	gzPath := filepath.Join(dir, "image.tar.gz")

	fmt.Fprintln(log, "==> docker save "+image)
	if err := run.Run(ctx, Cmd{Name: "docker", Args: []string{"save", "-o", tarPath, image}}, log); err != nil {
		return fmt.Errorf("docker save: %w", err)
	}
	if err := gzipFile(tarPath, gzPath); err != nil {
		return err
	}
	_ = os.Remove(tarPath)
	fi, err := os.Stat(gzPath)
	if err != nil {
		return err
	}
	f, err := os.Open(gzPath)
	if err != nil {
		return err
	}
	defer f.Close()
	cli := backup.NewClient(s3)
	return cli.PutObject(ctx, key, f, fi.Size(), "application/gzip")
}

func gzipFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	zw := gzip.NewWriter(out)
	if _, err := io.Copy(zw, in); err != nil {
		_ = zw.Close()
		return err
	}
	if err := zw.Close(); err != nil {
		return err
	}
	return out.Close()
}

// pruneLocalImages removes older tags for this service from the local Docker
// daemon / embedded registry, keeping the current image and up to retain-1
// other recent tags. Best-effort — failures are logged only.
func (w *Worker) pruneLocalImages(ctx context.Context, project, service, keepImage string, retain int, log io.Writer) {
	run := w.Pipeline.Runner
	if run == nil || retain <= 0 {
		return
	}
	repo := ImageRef(project, service, "") // ends with ":" if we use empty tag — use prefix without tag
	// ImageRef with empty tag still produces host/repo: — strip trailing colon.
	repo = strings.TrimSuffix(ImageRef(project, service, "x"), ":x")
	var listed captureWriter
	listed.w = io.Discard
	if err := run.Run(ctx, Cmd{Name: "docker", Args: []string{"images", "--format", "{{.Repository}}:{{.Tag}}", repo}}, &listed); err != nil {
		fmt.Fprintf(log, "==> local image prune list skipped: %v\n", err)
		return
	}
	var tags []string
	for _, line := range strings.Split(string(listed.buf), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasSuffix(line, ":<none>") {
			continue
		}
		tags = append(tags, line)
	}
	sort.Strings(tags) // lexicographic; tags include timestamps when using shortTag
	// Always keep the just-built image.
	keep := map[string]bool{keepImage: true}
	// Keep the newest retain-1 others (last in sorted order for time-based tags).
	extra := retain - 1
	for i := len(tags) - 1; i >= 0 && extra > 0; i-- {
		if keep[tags[i]] {
			continue
		}
		keep[tags[i]] = true
		extra--
	}
	for _, t := range tags {
		if keep[t] {
			continue
		}
		fmt.Fprintf(log, "==> pruning local image %s\n", t)
		_ = run.Run(ctx, Cmd{Name: "docker", Args: []string{"rmi", "-f", t}}, log)
	}
}

func pruneImageArchives(ctx context.Context, s3 backup.S3Config, prefix string, retain int, log *slog.Logger) {
	if retain <= 0 {
		return
	}
	if log == nil {
		log = slog.Default()
	}
	cli := backup.NewClient(s3)
	keys, err := cli.ListObjects(ctx, prefix)
	if err != nil {
		log.Warn("image archive prune list failed", "prefix", prefix, "err", err)
		return
	}
	sort.Strings(keys)
	if len(keys) <= retain {
		return
	}
	for _, k := range keys[:len(keys)-retain] {
		if err := cli.DeleteObject(ctx, k); err != nil {
			log.Warn("image archive prune delete failed", "key", k, "err", err)
		}
	}
}
