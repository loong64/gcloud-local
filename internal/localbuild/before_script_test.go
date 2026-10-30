package localbuild

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestBeforeScriptContainerIntegration(t *testing.T) {
	if os.Getenv("GCLOUD_LOCAL_INTEGRATION") != "1" {
		t.Skip("set GCLOUD_LOCAL_INTEGRATION=1 (requires Docker)")
	}
	for _, tc := range []struct {
		name, hook, config, wantOrder string
		code                          int
	}{
		{
			name: "args-and-script",
			hook: "test \"$PWD\" = /workspace; test -n \"$STEP_ENV\"; printf 'hook\\n' >> order; export PREPARED='two words'; touch /tmp/gcloud-local-prepared",
			config: `steps:
  - name: ghcr.io/loong64/cloud-builders/docker
    env: ['STEP_ENV=present']
    entrypoint: sh
    args: ['-ec', 'test "$$PREPARED" = "two words"; test -f /tmp/gcloud-local-prepared; test "$$1" = "argument with spaces"; printf "args\n" >> order', 'step', 'argument with spaces']
  - name: ghcr.io/loong64/cloud-builders/docker
    env: ['STEP_ENV=present']
    script: |
      test "$PREPARED" = "two words"
      test -f /tmp/gcloud-local-prepared
      printf 'script\n' >> order
`,
			wantOrder: "hook\nargs\nhook\nscript\n",
		},
		{
			name: "positional-parameters",
			hook: "set -- /bin/echo replaced-args; shift; export PREPARED='two words'",
			config: `steps:
  - name: ghcr.io/loong64/cloud-builders/docker
    entrypoint: sh
    args: ['-ec', 'test "$$PREPARED" = "two words"; test "$$1" = "argument with spaces"; test "$$2" = ""; test "$$3" = "a; echo unsafe"; printf "args\n" >> order', 'step', 'argument with spaces', '', 'a; echo unsafe']
`,
			wantOrder: "args\n",
		},
		{
			name: "default-docker-entrypoint",
			hook: "printf 'hook\\n' >> order; export PREPARED=ready",
			config: `steps:
  - name: ghcr.io/loong64/cloud-builders/docker
    args: ['--version']
`,
			wantOrder: "hook\n",
		},
		{
			name: "failure-skips-args",
			hook: "false; printf 'unexpected\\n' >> order",
			config: `steps:
  - name: ghcr.io/loong64/cloud-builders/docker
    entrypoint: sh
    args: ['-c', 'printf "args\n" >> order']
`,
			code: 1,
		},
		{
			name: "timeout",
			hook: "sleep 30",
			config: `steps:
  - name: ghcr.io/loong64/cloud-builders/docker
    timeout: 2s
    args: ['--version']
`,
			code: 1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			config := filepath.Join(root, "cloudbuild.yaml")
			writeTestFile(t, config, tc.config, 0600)
			t.Setenv("GCLOUD_LOCAL_BEFOCE_SCRIPT_LINUX", tc.hook)
			var output strings.Builder
			err := Main(context.Background(), []string{"builds", "submit", root, "--config", config, "--docker-config", root, "--no-push"}, &output, &output)
			code := 0
			if err != nil {
				code = ExitCode(err)
			}
			if code != tc.code {
				t.Fatalf("error %v; output %s", err, output.String())
			}
			if tc.name == "timeout" && !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("got %v", err)
			}
			if tc.name == "default-docker-entrypoint" && !strings.Contains(output.String(), "Docker version") {
				t.Fatal(output.String())
			}
			order, readErr := os.ReadFile(filepath.Join(root, "order"))
			if readErr != nil && !os.IsNotExist(readErr) {
				t.Fatal(readErr)
			}
			if string(order) != tc.wantOrder {
				t.Fatalf("order=%q, want %q", order, tc.wantOrder)
			}
			if tc.name == "timeout" {
				b, err := exec.Command("docker", "ps", "-aq", "--filter", "volume="+root).CombinedOutput()
				if err != nil || len(strings.TrimSpace(string(b))) != 0 {
					t.Fatalf("containers remain: %s, %v", b, err)
				}
			}
		})
	}
}

func TestBeforeScriptSkippedForDryRun(t *testing.T) {
	root := t.TempDir()
	config := filepath.Join(root, "cloudbuild.yaml")
	writeTestFile(t, config, "steps:\n  - name: image\n", 0600)
	t.Setenv("GCLOUD_LOCAL_BEFOCE_SCRIPT_LINUX", "touch should-not-exist; exit 9")
	if err := Main(context.Background(), []string{"builds", "submit", root, "--config", config, "--dry-run"}, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "should-not-exist")); !os.IsNotExist(err) {
		t.Fatal("dry run executed hook")
	}
}
