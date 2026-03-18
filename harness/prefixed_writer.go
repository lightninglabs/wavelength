package harness

import (
	"bytes"
	"fmt"
	"io"
	"sync"
)

// prefixedWriter writes each completed line to the underlying writer with a
// stable instance label prefix. This keeps interleaved stdout from multiple
// in-process daemons readable in integration tests.
type prefixedWriter struct {
	dst    io.Writer
	prefix []byte

	mu      sync.Mutex
	pending []byte
}

func newPrefixedWriter(dst io.Writer, label string) io.Writer {
	return &prefixedWriter{
		dst:    dst,
		prefix: []byte(fmt.Sprintf("[%s] ", label)),
	}
}

func (w *prefixedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.pending = append(w.pending, p...)

	for {
		newline := bytes.IndexByte(w.pending, '\n')
		if newline == -1 {
			return len(p), nil
		}

		line := w.pending[:newline+1]
		if _, err := w.dst.Write(w.prefix); err != nil {
			return 0, err
		}

		if _, err := w.dst.Write(line); err != nil {
			return 0, err
		}

		w.pending = w.pending[newline+1:]
	}
}
