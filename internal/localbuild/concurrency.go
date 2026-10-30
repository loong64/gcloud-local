package localbuild

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

func buildLockDir(socket string) string {
	// Use a stable location across working directories and TMPDIR overrides.
	return filepath.Join("/tmp", fmt.Sprintf("gcloud-local-builds-%d-%x", os.Getuid(), sha256.Sum256([]byte(socket))))
}

// Locks persist on disk: unlinking a slot could let another process lock a new
// inode while the old one is still held. The kernel releases locks on exit.
func acquireBuildSlot(ctx context.Context, dir string, limit int, logs io.Writer) (func(), error) {
	if limit < 1 {
		limit = 1 // Also protect callers constructing CLI directly.
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, fmt.Errorf("build concurrency lock: %w", err)
	}
	waiting := false
	for {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("waiting for build slot: %w", err)
		}
		for slot := 0; slot < limit; slot++ {
			f, err := os.OpenFile(filepath.Join(dir, fmt.Sprintf("%d.lock", slot)), os.O_CREATE|os.O_RDWR, 0600)
			if err != nil {
				return nil, fmt.Errorf("build concurrency lock: %w", err)
			}
			err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
			if err == nil {
				return func() { f.Close() }, nil
			}
			f.Close()
			if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EINTR) {
				return nil, fmt.Errorf("build concurrency lock: %w", err)
			}
		}
		if !waiting {
			fmt.Fprintf(logs, "Queued locally: waiting for build slot (max concurrent builds: %d).\n", limit)
			waiting = true
		}
		timer := time.NewTimer(100 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, fmt.Errorf("waiting for build slot: %w", ctx.Err())
		case <-timer.C:
		}
	}
}
