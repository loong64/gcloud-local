package localbuild

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestConcurrencyCLI(t *testing.T) {
	t.Setenv("GCLOUD_LOCAL_MAX_CONCURRENT_BUILDS", "")
	o, err := parseCLI([]string{"builds", "submit"})
	if err != nil || o.MaxConcurrentBuilds != 1 {
		t.Fatalf("default: %+v, %v", o, err)
	}
	t.Setenv("GCLOUD_LOCAL_MAX_CONCURRENT_BUILDS", "2")
	o, err = parseCLI([]string{"builds", "submit"})
	if err != nil || o.MaxConcurrentBuilds != 2 {
		t.Fatalf("environment: %+v, %v", o, err)
	}
	for _, value := range []string{"0", "-1", "bad", "1.5", "9999999999999999999999"} {
		t.Setenv("GCLOUD_LOCAL_MAX_CONCURRENT_BUILDS", value)
		if _, err := parseCLI([]string{"builds", "submit"}); err == nil {
			t.Fatalf("accepted environment %q", value)
		}
		o, err = parseCLI([]string{"builds", "submit", "--max-concurrent-builds", "3"})
		if err != nil || o.MaxConcurrentBuilds != 3 {
			t.Fatalf("override: %+v, %v", o, err)
		}
		if _, err := parseCLI([]string{"builds", "submit", "--max-concurrent-builds=" + value}); err == nil {
			t.Fatalf("accepted flag %q", value)
		}
	}
}

func TestBuildSlotCapacityAndCancellation(t *testing.T) {
	for _, limit := range []int{1, 2} {
		dir := t.TempDir()
		var releases []func()
		for i := 0; i < limit; i++ {
			release, err := acquireBuildSlot(context.Background(), dir, limit, io.Discard)
			if err != nil {
				t.Fatal(err)
			}
			defer release()
			releases = append(releases, release)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
		var logs strings.Builder
		_, err := acquireBuildSlot(ctx, dir, limit, &logs)
		cancel()
		if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(logs.String(), "waiting for build slot") {
			t.Fatalf("capacity %d: %v, %s", limit, err, &logs)
		}
		releases[0]()
		ctx, cancel = context.WithTimeout(context.Background(), time.Second)
		release, err := acquireBuildSlot(ctx, dir, limit, io.Discard)
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		release()
	}
}

func TestBuildSlotProcessHelper(t *testing.T) {
	dir := os.Getenv("GCLOUD_LOCAL_TEST_LOCK_DIR")
	if dir == "" {
		return
	}
	release, err := acquireBuildSlot(context.Background(), dir, 1, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	os.Stdout.WriteString("locked\n")
	io.Copy(io.Discard, os.Stdin)
}

func TestBuildSlotAcrossProcesses(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "locks")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestBuildSlotProcessHelper$")
	cmd.Env = append(os.Environ(), "GCLOUD_LOCAL_TEST_LOCK_DIR="+dir)
	input, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	output, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { cmd.Process.Kill(); cmd.Wait() }()
	if line, err := bufio.NewReader(output).ReadString('\n'); err != nil || line != "locked\n" {
		t.Fatalf("helper: %q, %v", line, err)
	}
	waitCtx, stop := context.WithTimeout(ctx, 150*time.Millisecond)
	_, err = acquireBuildSlot(waitCtx, dir, 1, io.Discard)
	stop()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("another process bypassed limit: %v", err)
	}
	// Even abrupt termination must release the slot without removing lock files.
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	cmd.Wait()
	release, err := acquireBuildSlot(ctx, dir, 1, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	release()
}

// Verify the runner waits before preparing resources or starting Docker.
func TestRunnerWaitsForBuildSlot(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "docker.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	canonical, err := filepath.EvalSymlinks(socket)
	if err != nil {
		t.Fatal(err)
	}
	release, err := acquireBuildSlot(context.Background(), buildLockDir(canonical), 1, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	opts := testCLI()
	opts.Socket = socket
	opts.Docker = "/does/not/exist"
	var logs strings.Builder
	r := runner{opts: opts, logs: &logs, out: io.Discard}
	err = r.run(context.Background(), Plan{Timeout: 150 * time.Millisecond})
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(logs.String(), "waiting for build slot") || r.temp != "" {
		t.Fatalf("runner did not queue before execution: temp=%q, err=%v, logs=%s", r.temp, err, &logs)
	}
}
