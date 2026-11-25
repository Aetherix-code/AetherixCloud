package build

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
)

// Cmd is an external command to run.
type Cmd struct {
	Name string
	Args []string
	Dir  string
	// Env entries are appended to the process environment ("K=V").
	Env []string
	// Redact values are replaced by "***" in any logged command line.
	Redact []string
}

// String renders the command line with secret values redacted.
func (c Cmd) String() string {
	s := c.Name + " " + strings.Join(c.Args, " ")
	for _, r := range c.Redact {
		if r != "" {
			s = strings.ReplaceAll(s, r, "***")
		}
	}
	return s
}

// Runner runs external commands (git, railpack, buildctl). Tests use fakes.
type Runner interface {
	// Run executes c, streaming combined stdout+stderr to out.
	Run(ctx context.Context, c Cmd, out io.Writer) error
}

// ExecRunner is the os/exec Runner.
type ExecRunner struct{}

// Run implements Runner.
func (ExecRunner) Run(ctx context.Context, c Cmd, out io.Writer) error {
	fmt.Fprintf(out, "$ %s\n", c.String())
	cmd := exec.CommandContext(ctx, c.Name, c.Args...)
	cmd.Dir = c.Dir
	cmd.Env = append(os.Environ(), c.Env...)
	pr, pw := io.Pipe()
	cmd.Stdout, cmd.Stderr = pw, pw
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start %s: %w", c.Name, err)
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		sc := bufio.NewScanner(pr)
		sc.Buffer(make([]byte, 64*1024), 1<<20)
		for sc.Scan() {
			_, _ = fmt.Fprintln(out, sc.Text())
		}
	}()
	err := cmd.Wait()
	_ = pw.Close()
	wg.Wait()
	if err != nil {
		return fmt.Errorf("%s: %w", c.Name, err)
	}
	return nil
}

// captureWriter collects output while forwarding it.
type captureWriter struct {
	mu  sync.Mutex
	buf []byte
	w   io.Writer
}

func (c *captureWriter) Write(p []byte) (int, error) {
	c.mu.Lock()
	c.buf = append(c.buf, p...)
	c.mu.Unlock()
	if c.w != nil {
		return c.w.Write(p)
	}
	return len(p), nil
}
