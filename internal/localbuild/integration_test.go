package localbuild

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDockerIntegration(t *testing.T) {
	if os.Getenv("GCLOUD_LOCAL_INTEGRATION") != "1" {
		t.Skip("set GCLOUD_LOCAL_INTEGRATION=1 (requires Docker and network on first run)")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Fatal(err)
	}
	docker := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("docker", args...)
		b, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("docker %v: %s: %v", args, b, err)
		}
		return string(b)
	}
	docker("info", "--format", "{{.ServerVersion}}")
	newID := func() string { return fmt.Sprintf("gcloud-local-test-%d", time.Now().UnixNano()) }
	run := func(t *testing.T, c Config, o CLI) (string, error, string) {
		t.Helper()
		var logs bytes.Buffer
		id := newID()
		p, err := makePlan(c, o, id)
		if err != nil {
			t.Fatal(err)
		}
		r := runner{opts: o, out: &logs, logs: &logs, id: id}
		err = r.run(context.Background(), p)
		t.Log(logs.String())
		return logs.String(), err, id
	}
	t.Run("build-and-inspect", func(t *testing.T) {
		c, err := load(filepath.Join("..", "..", "examples", "smoke.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		o := testCLI()
		o.Source = t.TempDir()
		o.NoPush = true
		o.Timeout = "15m"
		image := newID() + ":test"
		o.Substitutions["_IMAGE"] = image
		_, err, _ = run(t, c, o)
		if err != nil {
			t.Fatal(err)
		}
		defer docker("image", "rm", image)
		container := strings.TrimSpace(docker("create", image, "/unused"))
		defer docker("rm", container)
		file := filepath.Join(t.TempDir(), "message.txt")
		docker("cp", container+":/message.txt", file)
		b, err := os.ReadFile(file)
		if err != nil || string(b) != "local-build-ok\n" {
			t.Fatalf("artifact %q: %v", b, err)
		}
	})
	t.Run("no-push-and-auth", func(t *testing.T) {
		o := testCLI()
		o.Source = t.TempDir()
		o.NoPush = true
		c := Config{Steps: []Step{
			{Name: "ghcr.io/loong64/cloud-builders/docker", Entrypoint: "bash", Args: []string{"-euc", `gcloud --quiet auth configure-docker; docker push invalid.local/never:push; docker image push invalid.local/never:push; docker manifest create invalid.local/never:push; docker manifest annotate invalid.local/never:push x; docker manifest push invalid.local/never:push; docker --config /gcloud-local/docker-config push invalid.local/never:push; docker --log-level=error image push invalid.local/never:push`}},
			{Name: "ghcr.io/loong64/cloud-builders/docker", Args: []string{"push", "invalid.local/never:push"}},
			{Name: "ghcr.io/loong64/cloud-builders/docker", Entrypoint: "/usr/bin/docker", Args: []string{"push", "invalid.local/never:push"}},
		}, Images: []string{"ghcr.io/loong64/cloud-builders/docker"}}
		logs, err, _ := run(t, c, o)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Count(logs, "--no-push skipped docker") != 9 {
			t.Fatal("push wrapper bypassed", logs)
		}
	})
	t.Run("tag-submission", func(t *testing.T) {
		root := t.TempDir()
		writeTestFile(t, filepath.Join(root, "Dockerfile"), "FROM scratch\nCOPY message /message\n", 0644)
		writeTestFile(t, filepath.Join(root, "message"), "tag-build-ok\n", 0644)
		image := newID() + ":tag"
		defer docker("image", "rm", "--force", image)
		var output bytes.Buffer
		if err := Main(context.Background(), []string{"builds", "submit", root, "--tag", image, "--no-push"}, &output, &output); err != nil {
			t.Fatalf("tag submission: %v\n%s", err, &output)
		}
		docker("image", "inspect", image)
	})
	t.Run("missing-output-no-push", func(t *testing.T) {
		o := testCLI()
		o.Source, o.NoPush = t.TempDir(), true
		image := newID() + ":missing"
		c := Config{Steps: []Step{{Name: "ghcr.io/loong64/cloud-builders/docker", Script: "echo no-image-created"}}, Images: []string{image}}
		logs, err, _ := run(t, c, o)
		if err == nil || !strings.Contains(err.Error(), image) || strings.Contains(logs, "DONE\n") {
			t.Fatalf("missing image accepted: %v\n%s", err, logs)
		}
	})
	t.Run("no-source-isolated-workspace", func(t *testing.T) {
		root := t.TempDir()
		file := filepath.Join(root, "cloudbuild.yaml")
		writeTestFile(t, file, `steps:
- name: ghcr.io/loong64/cloud-builders/docker
  script: |
    test ! -e /workspace/README.md
    test -z "$(ls -A /workspace)"
    mkdir generated
    echo isolated > generated/value
- name: ghcr.io/loong64/cloud-builders/docker
  dir: generated
  script: test "$(cat value)" = isolated
`, 0600)
		var output bytes.Buffer
		if err := Main(context.Background(), []string{"builds", "submit", "--no-source", "--no-push", "--config", file}, &output, &output); err != nil {
			t.Fatalf("source-free submission: %v\n%s", err, &output)
		}
		if _, err := os.Stat(filepath.Join(root, "generated")); !os.IsNotExist(err) {
			t.Fatalf("source-free build wrote into config directory: %v", err)
		}
		_, after, ok := strings.Cut(output.String(), "starting build \"")
		if !ok {
			t.Fatalf("build ID missing: %s", &output)
		}
		id, _, _ := strings.Cut(after, "\"")
		if got := strings.TrimSpace(docker("volume", "ls", "--filter", "name="+id, "--format", "{{.Name}}")); got != "" {
			t.Fatalf("source-free workspace leaked: %s", got)
		}
	})
	t.Run("reject-buildx-registry-output", func(t *testing.T) {
		o := testCLI()
		o.Source = t.TempDir()
		o.NoPush = true
		c := Config{Steps: []Step{{Name: "ghcr.io/loong64/cloud-builders/docker", Args: []string{"buildx", "build", "--push", "."}}}}
		logs, err, _ := run(t, c, o)
		if ExitCode(err) != 2 || !strings.Contains(logs, "refuses a build") {
			t.Fatal(logs, err)
		}
	})
	t.Run("shared-volumes-home-and-env", func(t *testing.T) {
		o := testCLI()
		o.Source = t.TempDir()
		o.NoPush = true
		volume := []Volume{{Name: "cache", Path: "/cache"}}
		c := Config{Steps: []Step{
			{Name: "ghcr.io/loong64/cloud-builders/docker", Volumes: volume, Script: "echo volume > /cache/value\necho home > $HOME/value\necho workspace > value\n"},
			{Name: "ghcr.io/loong64/cloud-builders/docker", Volumes: volume, Script: "test \"$(cat /cache/value)\" = volume\ntest \"$(cat $HOME/value)\" = home\ntest \"$(cat value)\" = workspace\n"},
		}}
		_, err, id := run(t, c, o)
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.TrimSpace(docker("volume", "ls", "--filter", "name="+id, "--format", "{{.Name}}")); got != "" {
			t.Fatal("volume leaked", got)
		}
	})
	t.Run("failure-and-allowed-exit", func(t *testing.T) {
		o := testCLI()
		o.Source = t.TempDir()
		o.NoPush = true
		c := Config{Steps: []Step{{Name: "ghcr.io/loong64/cloud-builders/docker", Entrypoint: "sh", Args: []string{"-c", "exit 7"}}}}
		_, err, _ := run(t, c, o)
		if err == nil || ExitCode(err) != 7 {
			t.Fatal("exit code not propagated", err)
		}
		file := filepath.Join(o.Source, "cloudbuild.yaml")
		writeTestFile(t, file, `steps:
- name: ghcr.io/loong64/cloud-builders/docker
  entrypoint: sh
  args: ['-c', 'exit 7']
  allow_exit_codes: ['7']
- name: ghcr.io/loong64/cloud-builders/docker
  script: echo continued
`, 0600)
		c, err = load(file)
		if err != nil {
			t.Fatal(err)
		}
		logs, err, _ := run(t, c, o)
		if err != nil || !strings.Contains(logs, "continued") {
			t.Fatal(logs, err)
		}
	})
	t.Run("timeout-cleans-container", func(t *testing.T) {
		o := testCLI()
		o.Source = t.TempDir()
		o.NoPush = true
		c := Config{Steps: []Step{{Name: "ghcr.io/loong64/cloud-builders/docker", Script: "sleep 60", Timeout: "2s", AllowFailure: true}}}
		_, err, id := run(t, c, o)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal("timeout did not fail build", err)
		}
		if got := strings.TrimSpace(docker("ps", "-a", "--filter", "name="+id, "--format", "{{.Names}}")); got != "" {
			t.Fatal("container leaked", got)
		}
	})
}
