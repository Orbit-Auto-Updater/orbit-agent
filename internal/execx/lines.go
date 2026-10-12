package execx

import (
	"bytes"
	"context"
	"sync"
)

// LineFunc receives command output one line at a time as it is produced.
// stream is "stdout" or "stderr".
type LineFunc func(stream, line string)

type sinkKey struct{}

// WithLineSink makes every command run with ctx report its output lines to f
// while it runs (used to stream package-manager output to the dashboard).
func WithLineSink(ctx context.Context, f LineFunc) context.Context {
	return context.WithValue(ctx, sinkKey{}, f)
}

func lineSink(ctx context.Context) LineFunc {
	f, _ := ctx.Value(sinkKey{}).(LineFunc)
	return f
}

// lineWriter splits a byte stream into lines. Carriage returns (progress
// bars) also end a line; empty lines are dropped.
type lineWriter struct {
	mu     *sync.Mutex // shared by stdout and stderr so lines don't interleave mid-call
	stream string
	f      LineFunc
	buf    []byte
}

func (w *lineWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexAny(w.buf, "\r\n")
		if i < 0 {
			break
		}
		w.emit(w.buf[:i])
		w.buf = w.buf[i+1:]
	}
	if len(w.buf) > 64<<10 { // pathological line without newline
		w.emit(w.buf)
		w.buf = nil
	}
	return len(p), nil
}

func (w *lineWriter) flush() {
	if len(w.buf) > 0 {
		w.emit(w.buf)
		w.buf = nil
	}
}

func (w *lineWriter) emit(b []byte) {
	line := string(bytes.TrimRight(b, " \t"))
	if line == "" {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.f(w.stream, line)
}
