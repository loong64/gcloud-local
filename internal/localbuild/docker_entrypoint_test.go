package localbuild

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDockerEntrypointIntegration(t *testing.T) {
	if os.Getenv("GCLOUD_LOCAL_INTEGRATION") != "1" {
		t.Skip("set GCLOUD_LOCAL_INTEGRATION=1 (requires Docker)")
	}
	image := fmt.Sprintf("gcloud-local-entrypoint-%d:test", time.Now().UnixNano())
	build := exec.Command("docker", "build", "--tag", image, "-")
	build.Stdin = strings.NewReader("FROM ghcr.io/loong64/cloud-builders/docker\nENTRYPOINT [\"/workspace/docker\", \"inherited\"]\nCMD [\"default arg\"]\n")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v: %s", err, output)
	}
	t.Cleanup(func() {
		if output, err := exec.Command("docker", "image", "rm", image).CombinedOutput(); err != nil {
			t.Errorf("image cleanup: %v: %s", err, output)
		}
	})
	for _, explicit := range []bool{false, true} {
		for _, noPush := range []bool{false, true} {
			t.Run(fmt.Sprintf("explicit=%v/noPush=%v", explicit, noPush), func(t *testing.T) {
				root := t.TempDir()
				writeTestFile(t, filepath.Join(root, "docker"), "#!/bin/sh\nprintf 'custom-executable:'\nprintf '<%s>' \"$@\"\n", 0755)
				c := Config{Steps: []Step{{Name: image}}}
				want := "custom-executable:<inherited><default arg>"
				if explicit {
					c.Steps[0].Entrypoint = "/workspace/docker"
					c.Steps[0].Args = []string{"--version", "argument with spaces"}
					want = "custom-executable:<--version><argument with spaces>"
				}
				hook := ""
				if noPush {
					hook = "set -- /bin/echo replaced-args"
				}
				t.Setenv("GCLOUD_LOCAL_BEFOCE_SCRIPT_LINUX", hook)
				o := testCLI()
				o.Source, o.DockerConfig, o.NoPush = root, root, noPush
				p, err := makePlan(c, o, "test")
				if err != nil {
					t.Fatal(err)
				}
				var output strings.Builder
				r := runner{opts: o, out: &output, logs: &output, id: fmt.Sprintf("entrypoint-%d", time.Now().UnixNano())}
				if err := r.run(context.Background(), p); err != nil {
					t.Fatalf("%v: %s", err, output.String())
				}
				if !strings.Contains(output.String(), want) {
					t.Fatalf("got %q, want %q", output.String(), want)
				}
			})
		}
	}
}
