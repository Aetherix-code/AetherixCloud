package build

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// MaxUploadBytes bounds an extracted `ax up` upload (uncompressed).
const MaxUploadBytes = 2 << 30

// ExtractTar unpacks a (optionally gzipped) tar stream into dst, rejecting
// path traversal, devices and symlinks that point outside dst. maxBytes caps
// the total uncompressed size (0 => MaxUploadBytes).
func ExtractTar(r io.Reader, dst string, maxBytes int64) error {
	if maxBytes <= 0 {
		maxBytes = MaxUploadBytes
	}
	br := bufio.NewReader(r)
	if magic, _ := br.Peek(2); len(magic) == 2 && magic[0] == 0x1f && magic[1] == 0x8b {
		gz, err := gzip.NewReader(br)
		if err != nil {
			return err
		}
		defer gz.Close()
		r = gz
	} else {
		r = br
	}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	tr := tar.NewReader(r)
	var total int64
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read tar: %w", err)
		}
		name := path.Clean(strings.TrimPrefix(h.Name, "./"))
		if name == "." || name == "" {
			continue
		}
		target, err := safeJoin(dst, name)
		if err != nil {
			return err
		}
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			total += h.Size
			if total > maxBytes {
				return fmt.Errorf("upload exceeds %d bytes", maxBytes)
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			mode := fs.FileMode(h.Mode) & 0o777
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode|0o600)
			if err != nil {
				return err
			}
			_, cerr := io.Copy(f, io.LimitReader(tr, h.Size))
			if err := f.Close(); cerr == nil {
				cerr = err
			}
			if cerr != nil {
				return cerr
			}
		case tar.TypeSymlink:
			if path.IsAbs(h.Linkname) {
				return fmt.Errorf("symlink %q: absolute target not allowed", h.Name)
			}
			if _, err := safeJoin(dst, path.Join(path.Dir(name), h.Linkname)); err != nil {
				return fmt.Errorf("symlink %q: %w", h.Name, err)
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			_ = os.Remove(target)
			if err := os.Symlink(h.Linkname, target); err != nil {
				return err
			}
		default:
			// hard links, devices, fifos: ignored
		}
	}
}

// DefaultIgnore are always excluded from uploads.
var DefaultIgnore = []string{".git", "node_modules", ".DS_Store"}

// ReadIgnore reads .dockerignore patterns (simple globs, no negation).
func ReadIgnore(dir string) []string {
	f, err := os.Open(filepath.Join(dir, ".dockerignore"))
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		l := strings.TrimSpace(sc.Text())
		if l == "" || strings.HasPrefix(l, "#") || strings.HasPrefix(l, "!") {
			continue
		}
		out = append(out, strings.TrimSuffix(strings.TrimPrefix(l, "/"), "/"))
	}
	return out
}

func ignored(rel string, patterns []string) bool {
	for _, p := range patterns {
		if ok, _ := path.Match(p, rel); ok {
			return true
		}
		if ok, _ := path.Match(p, path.Base(rel)); ok && !strings.Contains(p, "/") {
			return true
		}
		if rel == p || strings.HasPrefix(rel, p+"/") {
			return true
		}
	}
	return false
}

// TarDir writes dir as a gzipped tar (used by `ax up`). Entries matching
// ignore (plus DefaultIgnore) are skipped.
func TarDir(w io.Writer, dir string, ignore []string) error {
	patterns := append(append([]string{}, DefaultIgnore...), ignore...)
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relp, _ := filepath.Rel(dir, p)
		if relp == "." {
			return nil
		}
		relp = filepath.ToSlash(relp)
		if ignored(relp, patterns) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		link := ""
		if info.Mode()&fs.ModeSymlink != 0 {
			if link, err = os.Readlink(p); err != nil {
				return err
			}
		} else if !info.Mode().IsRegular() && !info.IsDir() {
			return nil
		}
		h, err := tar.FileInfoHeader(info, link)
		if err != nil {
			return err
		}
		h.Name = relp
		if info.IsDir() {
			h.Name += "/"
		}
		if err := tw.WriteHeader(h); err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			f, err := os.Open(p)
			if err != nil {
				return err
			}
			defer f.Close()
			_, err = io.Copy(tw, f)
			return err
		}
		return nil
	})
	if err != nil {
		return err
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return gz.Close()
}
