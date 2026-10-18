// Package applog provides the process-wide logger.
//
// A tray application has no console, so logs go to a file under
// %APPDATA%\Gatehouse and, in parallel, into a small in-memory ring buffer that
// the settings window displays. Diagnosing a portal that rejects the login
// request is otherwise close to impossible.
package applog

import (
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

const (
	// FileName is the log file written next to the config.
	FileName = "gatehouse.log"
	// ringLines is how many recent lines the settings window can show.
	ringLines = 500
	// maxLogBytes is the size at which the log is rotated once.
	maxLogBytes = 1 << 20 // 1 MiB
	// maxPartialBytes bounds a writer that never emits a newline.
	maxPartialBytes = 64 << 10
)

var (
	ring *Ring
	once sync.Once
)

// Ring is a fixed-size in-memory buffer of the most recent log lines.
type Ring struct {
	mu      sync.Mutex
	lines   []string
	partial []byte
	max     int
}

// Write implements io.Writer, splitting on newlines.
func (r *Ring) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.partial = append(r.partial, p...)

	consumed := 0
	for {
		i := bytes.IndexByte(r.partial[consumed:], '\n')
		if i < 0 {
			break
		}
		end := consumed + i
		r.push(strings.TrimRight(string(r.partial[consumed:end]), "\r"))
		consumed = end + 1
	}
	if consumed > 0 {
		r.partial = append(r.partial[:0], r.partial[consumed:]...)
	}

	if len(r.partial) > maxPartialBytes {
		r.push(string(r.partial))
		r.partial = r.partial[:0]
	}
	return len(p), nil
}

// push appends one line, dropping the oldest once the buffer is full.
func (r *Ring) push(line string) {
	r.lines = append(r.lines, line)
	if len(r.lines) > r.max {
		// Shift in place rather than reslicing, which would hold on to the
		// whole backing array forever.
		n := copy(r.lines, r.lines[len(r.lines)-r.max:])
		r.lines = r.lines[:n]
	}
}

// Snapshot returns a copy of the buffered lines, oldest first.
func (r *Ring) Snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()

	out := make([]string, len(r.lines))
	copy(out, r.lines)
	return out
}

// String returns the buffered lines joined by newlines.
func (r *Ring) String() string {
	return strings.Join(r.Snapshot(), "\n")
}

// LogPath returns where the log file is written.
func LogPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("locate config directory: %w", err)
	}
	return filepath.Join(dir, "Gatehouse", FileName), nil
}

// Init installs the logger as slog's default and returns the ring buffer the UI
// reads from.
//
// Failing to open the log file is deliberately not fatal: a read-only profile
// should still get a working tray application. The returned error reports the
// degraded setup so the caller can surface it.
func Init() (*Ring, error) {
	once.Do(func() {
		ring = &Ring{max: ringLines}
	})

	opts := &slog.HandlerOptions{Level: slog.LevelDebug}

	path, err := LogPath()
	if err != nil {
		slog.SetDefault(slog.New(slog.NewTextHandler(ring, opts)))
		return ring, err
	}

	f, ferr := openLogFile(path)
	if ferr != nil {
		slog.SetDefault(slog.New(slog.NewTextHandler(ring, opts)))
		return ring, ferr
	}

	slog.SetDefault(slog.New(slog.NewTextHandler(io.MultiWriter(f, ring), opts)))
	return ring, nil
}

// openLogFile opens the log for appending, rotating it once first if it has
// grown past maxLogBytes.
func openLogFile(path string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create log directory: %w", err)
	}

	if fi, err := os.Stat(path); err == nil && fi.Size() > maxLogBytes {
		// One generation of rotation is enough for a single-user tray app.
		if err := os.Rename(path, path+".1"); err != nil {
			return nil, fmt.Errorf("rotate log: %w", err)
		}
	}

	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open log file: %w", err)
	}
	return f, nil
}

// Ring returns the shared ring buffer, or nil if Init has not run.
func Get() *Ring {
	return ring
}
