package localbuild

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Only errors from a started step may be tolerated by the build config.
type stepExitError struct{ *statusError }

func (e *stepExitError) Unwrap() error { return e.statusError }

type runner struct {
	opts      CLI
	out, logs io.Writer
	id, temp  string
}

func (r *runner) dockerArgs(args []string) []string {
	return append([]string{"--config", dockerConfigDir(r.opts), "--host", "unix://" + r.opts.Socket}, args...)
}

func (r *runner) command(ctx context.Context, input string, args ...string) error {
	cmd := exec.CommandContext(ctx, r.opts.Docker, r.dockerArgs(args)...)
	cmd.Stdout = r.out
	cmd.Stderr = r.logs
	if input != "" {
		cmd.Stdin = strings.NewReader(input)
	}
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("docker %s: %w", args[0], ctx.Err())
		}
		var e *exec.ExitError
		if errors.As(err, &e) {
			return &statusError{e.ExitCode(), fmt.Errorf("docker %s: %w", args[0], err)}
		}
		return err
	}
	return nil
}
func (r *runner) capture(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, r.opts.Docker, r.dockerArgs(args)...)
	cmd.Stderr = r.logs
	b, err := cmd.Output()
	return string(b), err
}
func (r *runner) cleanup(args ...string) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, r.opts.Docker, r.dockerArgs(args)...)
	if data, err := cmd.CombinedOutput(); err != nil {
		fmt.Fprintf(r.logs, "gcloud-local: cleanup %v failed: %v %s\n", args, err, data)
	}
}
func (r *runner) prepare() error {
	var err error
	r.temp, err = os.MkdirTemp("", "gcloud-local-")
	if err != nil {
		return err
	}
	for _, d := range []string{"bin", "docker-config", "home"} {
		if err = os.Mkdir(filepath.Join(r.temp, d), 0700); err != nil {
			return err
		}
	}
	for name, body := range map[string]string{"gcloud": gcloudShim, "docker": dockerShim} {
		if err = os.WriteFile(filepath.Join(r.temp, "bin", name), []byte(body), 0755); err != nil {
			return err
		}
	}
	cfg, err := os.ReadFile(filepath.Join(dockerConfigDir(r.opts), "config.json"))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if len(cfg) > 0 {
		var parsed map[string]any
		if err = json.Unmarshal(cfg, &parsed); err != nil {
			return fmt.Errorf("Docker config: %w", err)
		}
		// Host helpers (desktop, gcloud, pass, ...) generally do not exist in steps.
		if parsed["credsStore"] != "" && parsed["credsStore"] != nil {
			return fmt.Errorf("Docker credsStore is not supported inside builder containers; use --docker-config with a config produced by docker login without credential helpers")
		}
		if helpers, ok := parsed["credHelpers"].(map[string]any); ok && len(helpers) > 0 {
			return fmt.Errorf("Docker credHelpers are not supported inside builder containers; use --docker-config with plain docker login credentials")
		}
		if err = os.WriteFile(filepath.Join(r.temp, "docker-config", "config.json"), cfg, 0600); err != nil {
			return err
		}
	}
	return nil
}
func bind(src, dst string, ro bool) string {
	s := "type=bind,src=" + src + ",dst=" + dst
	if ro {
		s += ",readonly"
	}
	return s
}
func (r *runner) ensureImage(ctx context.Context, image string) error {
	isBuilder := image == builderImage
	var base string
	if isBuilder {
		base, image = localBuilder()
	}
	if _, err := r.capture(ctx, "image", "inspect", image); err == nil {
		return nil
	}
	if isBuilder {
		fmt.Fprintln(r.logs, "gcloud-local: building local Docker/Make builder")
		return r.command(ctx, builderDockerfile, "build", "--build-arg", "DOCKER_CLI_IMAGE="+base, "--tag", image, "-")
	}
	return r.command(ctx, "", "pull", image)
}
func (r *runner) run(ctx context.Context, p Plan) error {
	ctx, cancel := context.WithTimeout(ctx, p.Timeout)
	defer cancel()
	socket, err := filepath.Abs(r.opts.Socket)
	if err != nil {
		return err
	}
	r.opts.Socket = socket
	if st, err := os.Stat(socket); err != nil || st.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("local Docker socket unavailable: %s", socket)
	}
	if strings.Contains(p.Workspace, ",") || strings.Contains(socket, ",") {
		return fmt.Errorf("Docker bind mount paths cannot contain commas")
	}
	if err = r.prepare(); err != nil {
		if r.temp != "" {
			os.RemoveAll(r.temp)
		}
		return err
	}
	defer os.RemoveAll(r.temp)
	volumes := map[string]string{}
	for _, s := range p.Steps {
		for _, v := range s.Step.Volumes {
			if _, ok := volumes[v.Name]; !ok {
				volumes[v.Name] = r.id + "-" + v.Name
			}
		}
	}
	for _, name := range volumes {
		if err = r.command(ctx, "", "volume", "create", name); err != nil {
			return err
		}
		defer r.cleanup("volume", "rm", "--force", name)
	}
	for i, s := range p.Steps {
		fmt.Fprintf(r.logs, "gcloud-local: step %d (%s): %s in %s\n", i, s.Step.ID, s.Image, s.Workdir)
		err = r.step(ctx, i, s, p.Workspace, volumes)
		if err != nil {
			// Cancellation and timeout are build failures, even for allowFailure steps.
			if ctx.Err() != nil || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
				return err
			}
			var stepErr *stepExitError
			if !errors.As(err, &stepErr) {
				return fmt.Errorf("step %d (%s) could not execute: %w", i, s.Step.ID, err)
			}
			code := stepErr.code
			allowed := s.Step.AllowFailure && len(s.Step.AllowExitCodes) == 0
			for _, c := range s.Step.AllowExitCodes {
				if code == c {
					allowed = true
				}
			}
			if !allowed {
				return fmt.Errorf("step %d (%s) failed: %w", i, s.Step.ID, err)
			}
			fmt.Fprintf(r.logs, "gcloud-local: step %d failure allowed: %v\n", i, err)
		}
	}
	if r.opts.NoPush {
		fmt.Fprintln(r.logs, "gcloud-local: --no-push skipped final images push")
	} else {
		for _, image := range p.Images {
			if err = r.command(ctx, "", "push", image); err != nil {
				return err
			}
		}
	}
	fmt.Fprintln(r.logs, "gcloud-local: build succeeded")
	return nil
}
func (r *runner) step(ctx context.Context, index int, s StepPlan, root string, volumes map[string]string) error {
	work := filepath.Join(root, filepath.FromSlash(strings.TrimPrefix(s.Workdir, "/workspace/")))
	if s.Workdir == "/workspace" {
		work = root
	}
	if _, err := resolveWorkdir(root, work); err != nil || !existingDir(work) {
		return fmt.Errorf("step %d: directory %q is unavailable inside workspace", index, s.Workdir)
	}
	ctx, cancel := context.WithTimeout(ctx, s.Timeout)
	defer cancel()
	if err := r.ensureImage(ctx, s.Image); err != nil {
		return err
	}
	if s.Image == builderImage {
		_, s.Image = localBuilder()
	}
	imageEnv, err := r.capture(ctx, "image", "inspect", "--format", "{{range .Config.Env}}{{println .}}{{end}}", s.Image)
	if err != nil {
		return err
	}
	imagePath := "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
	for _, e := range strings.Split(imageEnv, "\n") {
		if strings.HasPrefix(e, "PATH=") {
			imagePath = strings.TrimPrefix(e, "PATH=")
		}
	}
	name := fmt.Sprintf("%s-step-%d", r.id, index)
	args := []string{"run", "--rm", "--user", "0:0", "--name", name, "--network", r.opts.Network,
		"--mount", bind(root, "/workspace", false), "--mount", bind(r.opts.Socket, "/var/run/docker.sock", false),
		"--mount", bind(filepath.Join(r.temp, "bin"), "/gcloud-local/bin", true),
		"--mount", bind(filepath.Join(r.temp, "docker-config"), "/gcloud-local/docker-config", false),
		"--mount", bind(filepath.Join(r.temp, "home"), "/builder/home", false), "--workdir", s.Workdir,
		"--env", "HOME=/builder/home"}
	for _, e := range s.Env {
		key, _, _ := strings.Cut(e, "=")
		switch key {
		case "DOCKER_CONFIG", "DOCKER_HOST", "GCLOUD_LOCAL_NO_PUSH":
			return fmt.Errorf("reserved environment variable %s", key)
		case "PATH":
			imagePath = strings.TrimPrefix(e, "PATH=")
			continue
		}
		args = append(args, "--env", e)
	}
	args = append(args, "--env", "PATH=/gcloud-local/bin:"+imagePath, "--env", "DOCKER_CONFIG=/gcloud-local/docker-config", "--env", "DOCKER_HOST=unix:///var/run/docker.sock")
	if r.opts.NoPush {
		args = append(args, "--env", "GCLOUD_LOCAL_NO_PUSH=1")
	}
	for _, v := range s.Step.Volumes {
		args = append(args, "--mount", "type=volume,src="+volumes[v.Name]+",dst="+v.Path)
	}
	entry := s.Step.Entrypoint
	stepArgs := s.Step.Args
	if s.Step.Script != "" {
		script := s.Step.Script
		if !strings.HasPrefix(script, "#!") {
			script = "#!/bin/sh\nset -e\n" + script
		}
		file := filepath.Join(r.temp, fmt.Sprintf("script-%d", index))
		if err = os.WriteFile(file, []byte(script), 0755); err != nil {
			return err
		}
		args = append(args, "--mount", bind(file, "/gcloud-local/script", true))
		entry = "/gcloud-local/script"
		stepArgs = nil
	}
	if entry != "" {
		args = append(args, "--entrypoint", entry)
	}
	args = append(args, s.Image)
	args = append(args, stepArgs...)
	// Killing the docker client alone does not stop its container.
	// Remove on cancellation before returning and before deleting mounted files.
	err = r.command(ctx, "", args...)
	if ctx.Err() != nil {
		r.cleanup("rm", "--force", name)
		return fmt.Errorf("step %d: %w", index, ctx.Err())
	}
	var exitErr *statusError
	// Docker reserves 125–127 for daemon, invocation and command startup errors.
	if errors.As(err, &exitErr) && exitErr.code != 125 && exitErr.code != 126 && exitErr.code != 127 {
		return &stepExitError{exitErr}
	}
	return err
}
