package localbuild

import (
	"context"
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"testing"
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
			for _, want := range []string{"starting build \"test\"\n\nBUILD\n", "Already have image: image\n", "hello\n", "last\n", "diagnostic\n"} {
				if !strings.Contains(got, want) {
					t.Fatalf("missing %q in %s", want, got)
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
				if !strings.HasSuffix(got, "ERROR\n") || strings.Contains(got, "DONE\n") || strings.Contains(got, "Starting Step #1") {
					t.Fatal(got)
				}
			} else if !strings.HasSuffix(got, "DONE\n") {
				t.Fatal(got)
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
		})
	}
}
