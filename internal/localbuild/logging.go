package localbuild

import (
	"bytes"
	"fmt"
	"io"
	"sync"
)

func stepIdentifier(index int, id string) string {
	label := fmt.Sprintf("Step #%d", index)
	if id != "" {
		label += fmt.Sprintf(" - %q", id)
	}
	return label
}

// Each stream retains incomplete lines; both streams share a lock so even a
// caller using the same writer for stdout and stderr receives whole lines.
type lineWriter struct {
	dst    io.Writer
	mu     *sync.Mutex
	prefix string
	buf    []byte
}

func (w *lineWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			return len(p), nil
		}
		if err := w.emit(w.buf[:i+1]); err != nil {
			return 0, err
		}
		w.buf = w.buf[i+1:]
	}
}

func (w *lineWriter) emit(line []byte) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	_, err := fmt.Fprintf(w.dst, "%s%s", w.prefix, line)
	return err
}

func (w *lineWriter) flush() error {
	if len(w.buf) == 0 {
		return nil
	}
	err := w.emit(append(w.buf, '\n'))
	w.buf = nil
	return err
}
