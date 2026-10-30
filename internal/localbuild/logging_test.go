package localbuild

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestLineWriter(t *testing.T) {
	for _, prefix := range []string{"", "Step #0 - \"build\": "} {
		var output strings.Builder
		w := lineWriter{dst: &output, mu: &sync.Mutex{}, prefix: prefix}
		long := strings.Repeat("x", 256*1024)
		for _, part := range []string{"hel", "lo\n\nprogress\r\n", long, "\nlast"} {
			if n, err := w.Write([]byte(part)); err != nil || n != len(part) {
				t.Fatalf("write: %d, %v", n, err)
			}
		}
		if err := w.flush(); err != nil {
			t.Fatal(err)
		}
		if err := w.flush(); err != nil {
			t.Fatal(err)
		}
		want := prefix + "hello\n" + prefix + "\n" + prefix + "progress\r\n" + prefix + long + "\n" + prefix + "last\n"
		if output.String() != want {
			t.Fatalf("incorrect line framing (prefix %q)", prefix)
		}
	}
}

func TestBuildLogLifecycle(t *testing.T) {
	for _, tc := range []struct {
		name            string
		count, code     int
		allowed, noPush bool
	}{
		{name: "single", count: 1},
		{name: "multiple", count: 2},
		{name: "failed", count: 2, code: 7},
		{name: "allowed", count: 2, code: 7, allowed: true},
		{name: "no-push", count: 2, noPush: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			socket := filepath.Join(root, "docker.sock")
			listener, err := net.Listen("unix", socket)
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			docker := filepath.Join(root, "docker")
			writeTestFile(t, docker, fmt.Sprintf(`#!/bin/sh
shift 4
case "$1 $2" in
 'image inspect') echo '{}' ;;
 run*) printf 'hello\nlast'; printf 'diagnostic\n' >&2; exit %d ;;
 push*) echo pushed ;;
esac
`, tc.code), 0755)
			o := testCLI()
			o.Source, o.Socket, o.Docker, o.DockerConfig, o.NoPush = root, socket, docker, root, tc.noPush
			c := Config{Images: []string{"result:test"}}
			for i := 0; i < tc.count; i++ {
				id := ""
				if i == 0 {
					id = "build"
				}
				c.Steps = append(c.Steps, Step{Name: "image", ID: id, AllowFailure: tc.allowed})
			}
			p, err := makePlan(c, o, "test")
			if err != nil {
				t.Fatal(err)
			}
			var out, logs strings.Builder
			r := runner{opts: o, out: &out, logs: &logs, id: "test"}
			err = r.run(context.Background(), p)
			failed := tc.code != 0 && !tc.allowed
			if (err != nil) != failed {
				t.Fatalf("error %v; logs %s", err, &logs)
			}
			if failed && ExitCode(err) != tc.code {
				t.Fatalf("exit code lost: %v", err)
			}
			if strings.Contains(logs.String(), "diagnostic") || strings.Contains(logs.String(), "BUILD\n") {
				t.Fatalf("build output leaked to stderr: %s", &logs)
			}
			got := out.String()
			for _, want := range []string{" LOCAL BUILD OUTPUT: test ", "starting build \"test\"\n\nBUILD\n", "Already have image: image\n", "hello\n", "last\n", "diagnostic\n"} {
				if !strings.Contains(got, want) {
					t.Fatalf("missing %q in %s", want, got)
				}
			}
			for _, want := range []string{"Created [local/test].\n", "Waiting for build to complete.", "Streaming local Docker logs for build [test]."} {
				if !strings.Contains(logs.String(), want) {
					t.Fatalf("missing status %q in %s", want, &logs)
				}
			}
			if tc.count > 1 {
				for _, want := range []string{"Starting Step #0 - \"build\"\n", "Step #0 - \"build\": hello\n", "Step #0 - \"build\": last\n", "Step #0 - \"build\": diagnostic\n"} {
					if !strings.Contains(got, want) {
						t.Fatalf("missing %q in %s", want, got)
					}
				}
				if !failed && !strings.Contains(got, "Finished Step #1\n") {
					t.Fatal(got)
				}
			} else if strings.Contains(got, "Step #") {
				t.Fatal(got)
			}
			if failed {
				if !strings.Contains(got, "ERROR\n"+buildLogBoundary("")) || strings.Contains(got, "DONE\n") || strings.Contains(got, "Starting Step #1") || strings.Contains(got, "CREATE_TIME") {
					t.Fatal(got)
				}
				if !strings.Contains(logs.String(), "BUILD FAILURE: build [test]:") || !strings.Contains(err.Error(), `completed with status "FAILURE"`) {
					t.Fatalf("missing failure detail: %v, %s", err, &logs)
				}
			} else {
				if !strings.Contains(got, "DONE\n"+buildLogBoundary("")) || !strings.HasSuffix(got, "SUCCESS\n") {
					t.Fatal(got)
				}
				lines := strings.Split(strings.TrimSpace(got), "\n")
				fields := strings.Fields(lines[len(lines)-1])
				if len(fields) != 6 || fields[0] != "test" || fields[3] != root || fields[4] != "result:test" || fields[5] != "SUCCESS" {
					t.Fatalf("bad summary row: %v", fields)
				}
			}
			if tc.allowed && !strings.Contains(got, "WARNING: Step #0") {
				t.Fatal(got)
			}
			if !failed && !tc.noPush && !strings.Contains(got, "PUSH\nPushing result:test\npushed\nDONE\n") {
				t.Fatal(got)
			}
			if tc.noPush && strings.Contains(got, "PUSH\n") {
				t.Fatal(got)
			}
			if tc.noPush && !strings.Contains(logs.String(), "Build [test]: --no-push skipped final images push") {
				t.Fatal(logs.String())
			}
		})
	}
}

func TestSubmissionWorkspaceLogging(t *testing.T) {
	root := t.TempDir()
	socket := filepath.Join(root, "docker.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	docker := filepath.Join(root, "docker")
	config := filepath.Join(root, "cloudbuild.yaml")
	writeTestFile(t, config, "steps: [{name: image}]", 0600)
	writeTestFile(t, docker, "#!/bin/sh\nshift 4\n[ \"$1 $2\" != 'image inspect' ] || echo '{}'\nexit 0\n", 0755)
	for _, verbosity := range []string{"", "info", "debug"} {
		t.Run(verbosity, func(t *testing.T) {
			args := []string{"builds", "submit", root, "--config", config, "--no-push", "--docker-bin", docker, "--docker-socket", socket, "--docker-config", root}
			if verbosity != "" {
				args = append([]string{"--verbosity=" + verbosity}, args...)
			}
			var out, logs strings.Builder
			if err := Main(context.Background(), args, &out, &logs); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(logs.String(), "source "+root) != (verbosity == "debug") {
				t.Fatalf("verbosity %q: %s", verbosity, &logs)
			}
			if !strings.Contains(logs.String(), "Created [local/local-") || !strings.Contains(out.String(), "SUCCESS\n") {
				t.Fatalf("submission logs missing: %s\n%s", &logs, &out)
			}
		})
	}
}

func TestBuildSummary(t *testing.T) {
	created := time.Date(2026, 10, 30, 12, 0, 0, 0, time.FixedZone("CST", 8*3600))
	for _, tc := range []struct {
		plan     Plan
		duration time.Duration
		want     []string
	}{
		{Plan{}, 0, []string{"2026-10-30T04:00:00+00:00", "0S", "-", "-"}},
		{Plan{Source: "/tmp/source", Images: []string{"a:test", "b:test"}}, 65 * time.Second, []string{"2026-10-30T04:00:00+00:00", "1M5S", "/tmp/source", "a:test (+1 more)"}},
		{Plan{Source: "source\nwith\tcontrols"}, time.Hour + 2*time.Minute + 3*time.Second, []string{"2026-10-30T04:00:00+00:00", "1H2M3S", `source\nwith\tcontrols`, "-"}},
	} {
		var out strings.Builder
		printBuildSummary(&out, tc.plan, "test", created, tc.duration)
		lines := strings.Split(strings.TrimSpace(out.String()), "\n")
		if len(lines) != 2 || strings.Join(strings.Fields(lines[0]), " ") != "ID CREATE_TIME DURATION SOURCE IMAGES STATUS" {
			t.Fatalf("bad summary: %s", &out)
		}
		for _, want := range tc.want {
			if !strings.Contains(lines[1], want) {
				t.Fatalf("missing %q: %s", want, &out)
			}
		}
	}
}

func TestBuildLogDeadlineAndCancellation(t *testing.T) {
	for _, cause := range []error{context.DeadlineExceeded, context.Canceled} {
		for _, running := range []bool{false, true} {
			var out, logs strings.Builder
			created := time.Now()
			var started time.Time
			if running {
				started = created.Add(time.Second)
			}
			err := finishBuildLog(&out, &logs, Plan{}, "build-id", created, started, created.Add(2*time.Second), cause)
			status := "CANCELLED"
			if cause == context.DeadlineExceeded {
				status = "TIMEOUT"
				if !strings.Contains(logs.String(), "--timeout=DURATION") {
					t.Fatal(logs.String())
				}
			}
			if !errors.Is(err, cause) || !strings.Contains(err.Error(), `status "`+status+`"`) || !strings.Contains(logs.String(), "build-id") {
				t.Fatalf("missing build status/cause: %v, %s", err, &logs)
			}
			if strings.Contains(out.String(), "SUCCESS") || (!running && out.Len() > 0) {
				t.Fatalf("queued or failed build emitted successful output: %s", &out)
			}
		}
	}
	// Formatting a failed build must not replace the step's exit code/error chain.
	cause := &stepExitError{&statusError{7, errors.New("step failed")}}
	err := finishBuildLog(io.Discard, io.Discard, Plan{}, "test", time.Now(), time.Now(), time.Now(), cause)
	if ExitCode(err) != 7 {
		t.Fatalf("exit code lost in build status: %v", err)
	}
}
