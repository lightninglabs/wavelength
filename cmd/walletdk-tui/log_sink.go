package main

import (
	"strings"
	"sync"
)

// logSink captures daemon log writes so Bubble Tea owns terminal output.
type logSink struct {
	mu      sync.Mutex
	lines   chan string
	partial string
}

// newLogSink creates a bounded non-blocking log writer.
func newLogSink(size int) *logSink {
	if size <= 0 {
		size = 1
	}

	return &logSink{
		lines: make(chan string, size),
	}
}

// Write implements io.Writer for daemon log output.
func (l *logSink) Write(p []byte) (int, error) {
	if l == nil {
		return len(p), nil
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	l.partial += string(p)
	for {
		idx := strings.IndexByte(l.partial, '\n')
		if idx < 0 {
			break
		}

		line := strings.TrimRight(l.partial[:idx], "\r")
		l.partial = l.partial[idx+1:]
		l.emit(line)
	}

	return len(p), nil
}

// Lines returns the captured daemon log stream.
func (l *logSink) Lines() <-chan string {
	if l == nil {
		return nil
	}

	return l.lines
}

// emit sends one log line without blocking daemon log writers.
func (l *logSink) emit(line string) {
	if line == "" {
		return
	}

	select {
	case l.lines <- line:
		return

	default:
	}

	select {
	case <-l.lines:
	default:
	}

	select {
	case l.lines <- line:
	default:
	}
}
