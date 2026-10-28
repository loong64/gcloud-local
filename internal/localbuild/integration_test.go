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
			{Name: "gcr.io/cloud-builders/docker", Entrypoint: "bash", Args: []string{"-euc", `gcloud --quiet auth configure-docker; docker push invalid.local/never:push; docker image push invalid.local/never:push; docker manifest create invalid.local/never:push; docker manifest annotate invalid.local/never:push x; docker manifest push invalid.local/never:push; docker --config /gcloud-local/docker-config push invalid.local/never:push; docker --log-level=error image push invalid.local/never:push`}},
			{Name: "gcr.io/cloud-builders/docker", Args: []string{"push", "invalid.local/never:push"}},
		}, Images: []string{"invalid.local/never:push"}}
		logs, err, _ := run(t, c, o)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Count(logs, "--no-push skipped docker") != 8 {
			t.Fatal("push wrapper bypassed", logs)
		}
	})
	t.Run("reject-buildx-registry-output", func(t *testing.T) {
		o := testCLI()
		o.Source = t.TempDir()
		o.NoPush = true
		c := Config{Steps: []Step{{Name: "gcr.io/cloud-builders/docker", Args: []string{"buildx", "build", "--push", "."}}}}
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
			{Name: "gcr.io/cloud-builders/docker", Volumes: volume, Script: "echo volume > /cache/value\necho home > $HOME/value\necho workspace > value\n"},
			{Name: "gcr.io/cloud-builders/docker", Volumes: volume, Script: "test \"$(cat /cache/value)\" = volume\ntest \"$(cat $HOME/value)\" = home\ntest \"$(cat value)\" = workspace\n"},
		}}
		_, err, id := run(t, c, o)
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.TrimSpace(docker("volume", "ls", "--filter", "name="+id, "--format", "{{.Name}}")); got != "" {
			t.Fatal("volume leaked", got)
		}
	})
	t.Run("generated-workdir", func(t *testing.T) {
		o := testCLI()
		o.Source = t.TempDir()
		o.NoPush = true
		// Steps run as root; let the host test user remove generated directories.
		c := Config{Steps: []Step{
			{Name: "gcr.io/cloud-builders/docker", Script: "umask 000\nmkdir -p generated/nested\necho ready > generated/nested/value"},
			{Name: "gcr.io/cloud-builders/docker", Dir: "generated/nested", Script: "test \"$(cat value)\" = ready"},
		}}
		if _, err, _ := run(t, c, o); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("failure-and-allowed-exit", func(t *testing.T) {
		o := testCLI()
		o.Source = t.TempDir()
		o.NoPush = true
		c := Config{Steps: []Step{{Name: "gcr.io/cloud-builders/docker", Entrypoint: "sh", Args: []string{"-c", "exit 7"}}}}
		_, err, _ := run(t, c, o)
		if err == nil || ExitCode(err) != 7 {
			t.Fatal("exit code not propagated", err)
		}
		c.Steps[0].AllowExitCodes = []int{7}
		c.Steps = append(c.Steps, Step{Name: "gcr.io/cloud-builders/docker", Script: "echo continued"})
		logs, err, _ := run(t, c, o)
		if err != nil || !strings.Contains(logs, "continued") {
			t.Fatal(logs, err)
		}
	})
	t.Run("timeout-cleans-container", func(t *testing.T) {
		o := testCLI()
		o.Source = t.TempDir()
		o.NoPush = true
		c := Config{Steps: []Step{{Name: "gcr.io/cloud-builders/docker", Script: "sleep 60", Timeout: "2s", AllowFailure: true}}}
		_, err, id := run(t, c, o)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal("timeout did not fail build", err)
		}
		if got := strings.TrimSpace(docker("ps", "-a", "--filter", "name="+id, "--format", "{{.Names}}")); got != "" {
			t.Fatal("container leaked", got)
		}
	})
}
