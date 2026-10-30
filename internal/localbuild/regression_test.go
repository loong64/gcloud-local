package localbuild

import (
	"context"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func writeTestFile(t *testing.T, name, body string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(name, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
}

func TestBuilderPullAndCache(t *testing.T) {
	root := t.TempDir()
	docker := filepath.Join(root, "docker")
	o := testCLI()
	o.Docker = docker
	var output strings.Builder
	r := runner{opts: o, out: &output, logs: io.Discard}
	for _, cached := range []bool{false, true} {
		inspectCode := "1"
		if cached {
			inspectCode = "0"
		}
		writeTestFile(t, docker, `#!/bin/sh
shift 4
case "$1 $2" in
  'image inspect') exit `+inspectCode+` ;;
  pull*) printf '%s\n' "$@" ;;
  *) exit 99 ;;
esac
`, 0755)
		output.Reset()
		if err := r.ensureImage(context.Background(), "ghcr.io/loong64/cloud-builders/docker"); err != nil {
			t.Fatal(err)
		}
		want := "pull\nghcr.io/loong64/cloud-builders/docker\n"
		if cached {
			want = ""
		}
		if output.String() != want {
			t.Fatalf("cached=%v: got %q, want %q", cached, output.String(), want)
		}
	}
	writeTestFile(t, docker, "#!/bin/sh\nexit 1\n", 0755)
	if err := r.ensureImage(context.Background(), "ghcr.io/loong64/cloud-builders/docker"); err == nil {
		t.Fatal("failed pull must fail without falling back to a local build")
	}
}

func TestDockerShimGlobalOptions(t *testing.T) {
	root := t.TempDir()
	shim := filepath.Join(root, "shim")
	writeTestFile(t, shim, dockerShim, 0755)
	writeTestFile(t, filepath.Join(root, "docker"), "#!/bin/sh\nprintf 'delegated:'\nprintf '<%s>' \"$@\"\n", 0755)
	for _, tc := range []struct {
		name   string
		args   []string
		noPush bool
		want   string
		code   int
	}{
		{"config-push", []string{"--config", "/tmp/config", "push", "example/image"}, true, "skipped docker", 0},
		{"host-image-push", []string{"-Hunix:///tmp/docker.sock", "image", "push", "example/image"}, true, "skipped docker", 0},
		{"interspersed-options", []string{"image", "--config", "/tmp/config", "push", "example/image"}, true, "skipped docker", 0},
		{"context-manifest", []string{"--context=local", "--debug", "manifest", "push", "example/image"}, true, "skipped docker", 0},
		{"imagetools", []string{"--log-level", "error", "buildx", "imagetools", "create"}, true, "refuses buildx imagetools", 2},
		{"registry-output", []string{"--config=/tmp/config", "buildx", "build", "--push", "."}, true, "refuses a build", 2},
		{"unknown-option", []string{"--future-option", "push", "example/image"}, true, "refuses unknown Docker option", 2},
		{"local-build", []string{"--config", "/tmp/config", "build", "."}, true, "delegated:<--config></tmp/config><build><.>", 0},
		{"push-enabled", []string{"--config", "/tmp/config", "push", "example/image"}, false, "delegated:<--config></tmp/config><push><example/image>", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command("/bin/sh", append([]string{shim}, tc.args...)...)
			cmd.Env = []string{"PATH=" + root + ":/usr/bin:/bin"}
			if tc.noPush {
				cmd.Env = append(cmd.Env, "GCLOUD_LOCAL_NO_PUSH=1")
			}
			out, _ := cmd.CombinedOutput()
			if cmd.ProcessState.ExitCode() != tc.code || !strings.Contains(string(out), tc.want) {
				t.Fatalf("exit %d, output %s", cmd.ProcessState.ExitCode(), out)
			}
		})
	}
}

func TestGeneratedWorkdir(t *testing.T) {
	root := t.TempDir()
	o := testCLI()
	o.Source = root
	c := Config{Steps: []Step{{Name: "image", Script: "mkdir -p generated/nested"}, {Name: "image", Dir: "generated/nested", Script: "echo ok"}}}
	p, err := makePlan(c, o, "id")
	if err != nil {
		t.Fatal(err)
	}
	if p.Steps[1].Workdir != "/workspace/generated/nested" {
		t.Fatal(p.Steps[1].Workdir)
	}
	// A symlink introduced after planning must still be checked before execution.
	if err := os.Symlink(t.TempDir(), filepath.Join(root, "generated")); err != nil {
		t.Fatal(err)
	}
	r := runner{opts: o}
	if err := r.step(context.Background(), 1, p.Steps[1], root, nil); err == nil || !strings.Contains(err.Error(), "directory") {
		t.Fatalf("workspace escape accepted: %v", err)
	}
}

func TestWorkdirRejectsInvalidAncestors(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "file"), "", 0600)
	for _, tc := range []struct{ name, target string }{{"outside", t.TempDir()}, {"dangling", filepath.Join(root, "missing")}} {
		if err := os.Symlink(tc.target, filepath.Join(root, tc.name)); err != nil {
			t.Fatal(err)
		}
	}
	for _, dir := range []string{"file/child", "outside/child", "dangling/child", "../escape"} {
		if _, err := resolveWorkdir(root, filepath.Join(root, dir)); err == nil {
			t.Fatalf("invalid directory accepted: %s", dir)
		}
	}
}

func TestInfrastructureFailureCannotBeAllowed(t *testing.T) {
	root := t.TempDir()
	socket := filepath.Join(root, "docker.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	docker := filepath.Join(root, "docker")
	for _, tc := range []struct {
		name, command string
		code          int
		allowCodes    []int
		wantSuccess   bool
	}{
		{"pull", "pull", 1, []int{1}, false},
		{"inspect", "inspect", 1, []int{1}, false},
		{"daemon", "run", 125, []int{125}, false},
		{"entrypoint", "run", 126, nil, false},
		{"missing-command", "run", 127, nil, false},
		{"step-failure", "run", 7, []int{7}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			script := "#!/bin/sh\nshift 4\ncase \"$1 $2\" in\n"
			switch tc.command {
			case "pull":
				script += "'image inspect') exit 1 ;;\npull*) exit 1 ;;\n"
			case "inspect":
				script += "'image inspect') [ \"$3\" != --format ] || exit 1 ;;\n"
			case "run":
				script += "run*) exit " + strconv.Itoa(tc.code) + " ;;\n"
			}
			script += "esac\nexit 0\n"
			writeTestFile(t, docker, script, 0755)
			o := testCLI()
			o.Source = root
			o.Socket = socket
			o.Docker = docker
			o.DockerConfig = filepath.Join(root, "config")
			o.NoPush = true
			c := Config{Steps: []Step{{Name: "image", Script: "echo ok", AllowFailure: true, AllowExitCodes: tc.allowCodes}}}
			p, err := makePlan(c, o, "test")
			if err != nil {
				t.Fatal(err)
			}
			var logs strings.Builder
			r := runner{opts: o, out: io.Discard, logs: &logs, id: "test"}
			err = r.run(context.Background(), p)
			if (err == nil) != tc.wantSuccess {
				t.Fatalf("error %v, logs %s", err, logs.String())
			}
		})
	}
}
