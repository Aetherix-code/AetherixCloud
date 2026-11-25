package build

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// LogLine is one line of build output.
type LogLine struct {
	Time   time.Time
	Stream string // "stdout" | "system"
	Line   string
}

const maxLiveLines = 100000

// LogStore keeps build logs on the builder node: one file per build
// ("<dir>/<id>.log", lines "<unixnano>\t<stream>\t<text>") plus an in-memory
// tail so followers can attach to a running build without races.
type LogStore struct {
	Dir  string
	mu   sync.Mutex
	live map[string]*liveLog
}

type liveLog struct {
	mu    sync.Mutex
	lines []LogLine
	subs  map[chan LogLine]struct{}
	done  bool
	file  *os.File
}

// NewLogStore creates the directory.
func NewLogStore(dir string) (*LogStore, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &LogStore{Dir: dir, live: map[string]*liveLog{}}, nil
}

func (s *LogStore) path(id string) string { return filepath.Join(s.Dir, filepath.Base(id)+".log") }

// Writer starts logging for build id. Close it to mark the log complete.
func (s *LogStore) Writer(id string) (*LogWriter, error) {
	f, err := os.OpenFile(s.path(id), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return nil, err
	}
	ll := &liveLog{subs: map[chan LogLine]struct{}{}, file: f}
	s.mu.Lock()
	s.live[id] = ll
	s.mu.Unlock()
	return &LogWriter{s: s, id: id, ll: ll}, nil
}

// LogWriter is an io.Writer splitting output into lines.
type LogWriter struct {
	s   *LogStore
	id  string
	ll  *liveLog
	buf []byte
}

// Write implements io.Writer.
func (w *LogWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			break
		}
		w.emit("stdout", strings.TrimRight(string(w.buf[:i]), "\r"))
		w.buf = w.buf[i+1:]
	}
	return len(p), nil
}

// System writes a system line.
func (w *LogWriter) System(line string) { w.emit("system", line) }

func (w *LogWriter) emit(stream, line string) {
	l := LogLine{Time: time.Now(), Stream: stream, Line: line}
	ll := w.ll
	ll.mu.Lock()
	defer ll.mu.Unlock()
	if len(ll.lines) < maxLiveLines {
		ll.lines = append(ll.lines, l)
	}
	if ll.file != nil {
		_, _ = ll.file.WriteString(strconv.FormatInt(l.Time.UnixNano(), 10) + "\t" + stream + "\t" + strings.ReplaceAll(line, "\n", " ") + "\n")
	}
	for c := range ll.subs {
		select {
		case c <- l:
		default: // slow follower: drop
		}
	}
}

// Close flushes the remainder, finishes followers and the file.
func (w *LogWriter) Close() error {
	if len(w.buf) > 0 {
		w.emit("stdout", string(w.buf))
		w.buf = nil
	}
	ll := w.ll
	ll.mu.Lock()
	ll.done = true
	for c := range ll.subs {
		close(c)
	}
	ll.subs = map[chan LogLine]struct{}{}
	err := ll.file.Close()
	ll.file = nil
	ll.mu.Unlock()
	w.s.mu.Lock()
	delete(w.s.live, w.id)
	w.s.mu.Unlock()
	return err
}

// ErrNoLog is returned when this node has no log for the build.
var ErrNoLog = errors.New("build log not found on this node")

// Stream replays the log of build id and, if follow and the build is still
// running on this node, continues until it ends or ctx is done.
func (s *LogStore) Stream(ctx context.Context, id string, follow bool, send func(LogLine) error) error {
	s.mu.Lock()
	ll := s.live[id]
	s.mu.Unlock()
	if ll == nil {
		return s.replayFile(id, send)
	}
	ll.mu.Lock()
	snapshot := append([]LogLine(nil), ll.lines...)
	var ch chan LogLine
	if follow && !ll.done {
		ch = make(chan LogLine, 1024)
		ll.subs[ch] = struct{}{}
	}
	ll.mu.Unlock()
	for _, l := range snapshot {
		if err := send(l); err != nil {
			return err
		}
	}
	if ch == nil {
		return nil
	}
	defer func() {
		ll.mu.Lock()
		delete(ll.subs, ch)
		ll.mu.Unlock()
	}()
	for {
		select {
		case l, ok := <-ch:
			if !ok {
				return nil
			}
			if err := send(l); err != nil {
				return err
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// Has reports whether a log (live or on disk) exists for the build.
func (s *LogStore) Has(id string) bool {
	s.mu.Lock()
	_, ok := s.live[id]
	s.mu.Unlock()
	if ok {
		return true
	}
	_, err := os.Stat(s.path(id))
	return err == nil
}

func (s *LogStore) replayFile(id string, send func(LogLine) error) error {
	f, err := os.Open(s.path(id))
	if errors.Is(err, os.ErrNotExist) {
		return ErrNoLog
	}
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		parts := strings.SplitN(sc.Text(), "\t", 3)
		if len(parts) != 3 {
			continue
		}
		ns, _ := strconv.ParseInt(parts[0], 10, 64)
		if err := send(LogLine{Time: time.Unix(0, ns), Stream: parts[1], Line: parts[2]}); err != nil {
			return err
		}
	}
	return sc.Err()
}
