package localbuild

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"text/tabwriter"
	"time"
)

// Follow gcloud's TailerBase framing, with an explicit local title and build ID
// because multiple submissions may share a terminal. CI uses an 80-column frame.
func buildLogBoundary(title string) string {
	const width = 80
	padding := width - len(title)
	if padding < 0 {
		padding = 0
	}
	return strings.Repeat("-", padding/2) + title + strings.Repeat("-", padding-padding/2)
}

func buildDuration(d time.Duration) string {
	seconds := int64(d.Round(time.Second) / time.Second)
	if seconds < 0 {
		seconds = 0
	}
	var result string
	if hours := seconds / 3600; hours > 0 {
		result += fmt.Sprintf("%dH", hours)
	}
	if minutes := seconds / 60 % 60; minutes > 0 {
		result += fmt.Sprintf("%dM", minutes)
	}
	if seconds%60 > 0 || result == "" {
		result += fmt.Sprintf("%dS", seconds%60)
	}
	return result
}

func printBuildSummary(out io.Writer, p Plan, id string, created time.Time, elapsed time.Duration) {
	source, images := p.Source, "-"
	if source == "" {
		source = "-"
	}
	if len(p.Images) > 0 {
		images = p.Images[0]
		if len(p.Images) > 1 {
			images += fmt.Sprintf(" (+%d more)", len(p.Images)-1)
		}
	}
	// Keep control characters in local paths from corrupting the table layout.
	cell := strings.NewReplacer("\t", `\t`, "\r", `\r`, "\n", `\n`)
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tCREATE_TIME\tDURATION\tSOURCE\tIMAGES\tSTATUS")
	fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\tSUCCESS\n", id,
		created.UTC().Format("2006-01-02T15:04:05-07:00"), buildDuration(elapsed), cell.Replace(source), images)
	w.Flush()
}

func finishBuildLog(out, logs io.Writer, p Plan, id string, created, started, finished time.Time, err error) error {
	if !started.IsZero() {
		if err != nil {
			fmt.Fprintln(out, "ERROR")
		}
		fmt.Fprintln(out, buildLogBoundary("")+"\n")
	}
	if err == nil {
		printBuildSummary(out, p, id, created, finished.Sub(started))
		return nil
	}
	status := "FAILURE"
	if errors.Is(err, context.DeadlineExceeded) {
		status = "TIMEOUT"
		fmt.Fprintf(logs, "Build [%s]: Your build timed out. Use the [--timeout=DURATION] flag to change the timeout threshold.\n", id)
	} else if errors.Is(err, context.Canceled) {
		status = "CANCELLED"
		fmt.Fprintf(logs, "Build [%s] was cancelled.\n", id)
	}
	if status == "FAILURE" {
		fmt.Fprintf(logs, "\nBUILD FAILURE: build [%s]: %v\n", id, err)
	}
	return fmt.Errorf("build %s completed with status %q: %w", id, status, err)
}

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
