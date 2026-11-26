// Package hostexec opens an interactive login shell on the local host (PTY).
package hostexec

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"

	"github.com/aetherix/aetherixcloud/internal/runtime"
	"github.com/creack/pty"
)

// Session is a running host PTY shell.
type Session struct {
	cmd  *exec.Cmd
	ptmx *os.File

	waitOnce sync.Once
	exitCode int
	waitErr  error
}

// Start opens a login shell with the given terminal size.
func Start(ctx context.Context, rows, cols int) (runtime.ExecSession, error) {
	if rows <= 0 {
		rows = 24
	}
	if cols <= 0 {
		cols = 80
	}
	shell := pickShell()
	cmd := exec.CommandContext(ctx, shell, "-l")
	cmd.Env = append(os.Environ(),
		"TERM=xterm-256color",
		fmt.Sprintf("COLUMNS=%d", cols),
		fmt.Sprintf("LINES=%d", rows),
	)
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: uint16(rows), Cols: uint16(cols)})
	if err != nil {
		return nil, fmt.Errorf("hostexec: start pty: %w", err)
	}
	return &Session{cmd: cmd, ptmx: ptmx}, nil
}

func pickShell() string {
	if s := os.Getenv("SHELL"); s != "" {
		if _, err := os.Stat(s); err == nil {
			return s
		}
	}
	for _, c := range []string{"/bin/bash", "/bin/zsh", "/bin/sh"} {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	return "/bin/sh"
}

func (s *Session) Stdin() io.WriteCloser  { return s.ptmx }
func (s *Session) Stdout() io.Reader      { return s.ptmx }
func (s *Session) Stderr() io.Reader      { return nil } // TTY merges stderr
func (s *Session) Resize(_ context.Context, rows, cols int) error {
	if rows <= 0 || cols <= 0 {
		return nil
	}
	return pty.Setsize(s.ptmx, &pty.Winsize{Rows: uint16(rows), Cols: uint16(cols)})
}

func (s *Session) Wait(ctx context.Context) (int, error) {
	done := make(chan struct{})
	go func() {
		s.waitOnce.Do(func() {
			err := s.cmd.Wait()
			if err == nil {
				s.exitCode, s.waitErr = 0, nil
				return
			}
			if ee, ok := err.(*exec.ExitError); ok {
				s.exitCode, s.waitErr = ee.ExitCode(), nil
				return
			}
			s.exitCode, s.waitErr = 1, err
		})
		close(done)
	}()
	select {
	case <-ctx.Done():
		_ = s.Close()
		<-done
		return s.exitCode, ctx.Err()
	case <-done:
		return s.exitCode, s.waitErr
	}
}

func (s *Session) Close() error {
	if s.cmd.Process != nil {
		_ = s.cmd.Process.Kill()
	}
	if s.ptmx != nil {
		return s.ptmx.Close()
	}
	return nil
}
