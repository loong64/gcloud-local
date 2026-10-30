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
	"sync"
	"time"
)

// Only errors from a started step may be tolerated by the build config.
type stepExitError struct{ *statusError }

func (e *stepExitError) Unwrap() error { return e.statusError }

type runner struct {
	opts      CLI
	out, logs io.Writer
	id, temp  string
	prefix    string
}

func (r *runner) dockerArgs(args []string) []string {
	return append([]string{"--config", dockerConfigDir(r.opts), "--host", "unix://" + r.opts.Socket}, args...)
}

func (r *runner) command(ctx context.Context, input string, args ...string) error {
	cmd := exec.CommandContext(ctx, r.opts.Docker, r.dockerArgs(args)...)
	var mu sync.Mutex
	out := &lineWriter{dst: r.out, mu: &mu, prefix: r.prefix}
	logs := &lineWriter{dst: r.logs, mu: &mu, prefix: r.prefix}
	cmd.Stdout = out
	cmd.Stderr = logs
	if input != "" {
		cmd.Stdin = strings.NewReader(input)
	}
	err := cmd.Run()
	outErr, logErr := out.flush(), logs.flush()
	if err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("docker %s: %w", args[0], ctx.Err())
		}
		var e *exec.ExitError
		if errors.As(err, &e) {
			return &statusError{e.ExitCode(), fmt.Errorf("docker %s: %w", args[0], err)}
		}
		return err
	}
	return errors.Join(outErr, logErr)
}
func (r *runner) capture(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, r.opts.Docker, r.dockerArgs(args)...)
	logs := &lineWriter{dst: r.logs, mu: &sync.Mutex{}, prefix: r.prefix}
	cmd.Stderr = logs
	b, err := cmd.Output()
	return string(b), errors.Join(err, logs.flush())
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
	// A cache miss is expected; suppress inspect's diagnostic before pulling.
	cmd := exec.CommandContext(ctx, r.opts.Docker, r.dockerArgs([]string{"image", "inspect", image})...)
	if err := cmd.Run(); err == nil {
		fmt.Fprintf(r.out, "%sAlready have image: %s\n", r.prefix, image)
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	fmt.Fprintf(r.out, "%sPulling image: %s\n", r.prefix, image)
	return r.command(ctx, "", "pull", image)
}
func (r *runner) run(ctx context.Context, p Plan) (runErr error) {
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
	// Resolve aliases so callers using different paths to the same socket share slots.
	canonicalSocket, err := filepath.EvalSymlinks(socket)
	if err != nil {
		return err
	}
	release, err := acquireBuildSlot(ctx, buildLockDir(canonicalSocket), r.opts.MaxConcurrentBuilds, r.logs)
	if err != nil {
		return err
	}
	defer release()
	// Like gcloud builds submit, stream build output to stdout.
	fmt.Fprintf(r.out, "starting build %q\n\n", r.id)
	defer func() {
		if runErr != nil {
			fmt.Fprintln(r.out, "ERROR")
		}
	}()
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
		setup := *r
		setup.out = io.Discard
		if err = setup.command(ctx, "", "volume", "create", name); err != nil {
			return err
		}
		defer r.cleanup("volume", "rm", "--force", name)
	}
	fmt.Fprintln(r.out, "BUILD")
	for i, s := range p.Steps {
		label := stepIdentifier(i, s.Step.ID)
		if len(p.Steps) > 1 {
			fmt.Fprintf(r.out, "Starting %s\n", label)
			r.prefix = label + ": "
		}
		stepRunner := *r
		stepRunner.logs = r.out
		err = stepRunner.step(ctx, i, s, p.Workspace, volumes)
		r.prefix = ""
		if err != nil {
			// Cancellation and timeout are build failures, even for allowFailure steps.
			if ctx.Err() != nil || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
				return err
			}
			var stepErr *stepExitError
			if !errors.As(err, &stepErr) {
				return fmt.Errorf("build step %d %q could not execute: %w", i, s.Image, err)
			}
			code := stepErr.code
			allowed := s.Step.AllowFailure && len(s.Step.AllowExitCodes) == 0
			for _, c := range s.Step.AllowExitCodes {
				if code == c {
					allowed = true
				}
			}
			if !allowed {
				return fmt.Errorf("build step %d %q failed: %w", i, s.Image, err)
			}
			fmt.Fprintf(r.out, "WARNING: %s failed but is allowed to fail: %v\n", label, err)
		}
		if len(p.Steps) > 1 {
			fmt.Fprintf(r.out, "Finished %s\n", label)
		}
	}
	if r.opts.NoPush {
		fmt.Fprintln(r.logs, "gcloud-local: --no-push skipped final images push")
	} else {
		fmt.Fprintln(r.out, "PUSH")
		for _, image := range p.Images {
			fmt.Fprintf(r.out, "Pushing %s\n", image)
			pushRunner := *r
			pushRunner.logs = r.out
			if err = pushRunner.command(ctx, "", "push", image); err != nil {
				return err
			}
		}
	}
	fmt.Fprintln(r.out, "DONE")
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
	imageConfigJSON, err := r.capture(ctx, "image", "inspect", "--format", "{{json .Config}}", s.Image)
	if err != nil {
		return err
	}
	var imageConfig struct {
		Env        []string
		Entrypoint []string
		Cmd        []string
	}
	if strings.TrimSpace(imageConfigJSON) != "" {
		if err := json.Unmarshal([]byte(imageConfigJSON), &imageConfig); err != nil {
			return fmt.Errorf("image %s config: %w", s.Image, err)
		}
	}
	imagePath := "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
	for _, e := range imageConfig.Env {
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
	// Route Docker entrypoints through the no-push wrapper while retaining
	// the exact executable selected by the image or the build configuration.
	if s.Step.Script == "" {
		if entry == "" && len(imageConfig.Entrypoint) > 0 && filepath.Base(imageConfig.Entrypoint[0]) == "docker" {
			entry = imageConfig.Entrypoint[0]
			if len(stepArgs) == 0 {
				stepArgs = imageConfig.Cmd
			}
			stepArgs = append(append([]string{}, imageConfig.Entrypoint[1:]...), stepArgs...)
		}
		if filepath.Base(entry) == "docker" {
			stepArgs = append([]string{"--gcloud-local-executable", entry}, stepArgs...)
			entry = "/gcloud-local/bin/docker"
		}
	}
	if r.opts.BeforeScriptLinux != "" {
		command := stepArgs
		if entry != "" {
			command = append([]string{entry}, command...)
		} else {
			if len(command) == 0 {
				command = imageConfig.Cmd
			}
			command = append(append([]string{}, imageConfig.Entrypoint...), command...)
		}
		if len(command) == 0 {
			return fmt.Errorf("step %d: no command to execute after GCLOUD_LOCAL_BEFOCE_SCRIPT_LINUX", index)
		}
		file := filepath.Join(r.temp, fmt.Sprintf("before-step-%d", index))
		script := "#!/bin/sh\nset -e\ngcloud_local_before() {\n:\n" + r.opts.BeforeScriptLinux + "\n}\ngcloud_local_before\nexec \"$@\"\n"
		if err := os.WriteFile(file, []byte(script), 0755); err != nil {
			return err
		}
		args = append(args, "--mount", bind(file, "/gcloud-local/before-step", true))
		entry, stepArgs = "/gcloud-local/before-step", command
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
