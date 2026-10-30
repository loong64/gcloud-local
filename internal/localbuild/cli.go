package localbuild

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

const usage = `gcloud-local builds submit [SOURCE] --config FILE [OPTIONS]

Execute Cloud Build image steps against a local Docker daemon.

  --config FILE              Cloud Build YAML/JSON (default cloudbuild.yaml)
  --substitutions K=V,...     Overrides; repeatable, supports gcloud ^DELIM^ syntax
  --project ID               PROJECT_ID (default local)
  --region REGION            LOCATION (default local)
  --workspace-root DIR       Repository root for archives without .git
  --no-push                  Skip final and nested docker/manifest pushes
  --dry-run                  Print JSON build plan without running Docker
  --max-concurrent-builds N   Concurrent builds per user/daemon (default 1)
  --timeout DURATION         Total build timeout, e.g. 3600s or 2h
  --docker-config DIR        Docker login config (default $DOCKER_CONFIG or ~/.docker)
  --docker-socket PATH       Local daemon socket (default /var/run/docker.sock)
  --docker-bin PATH          Docker CLI executable (default docker)
  --network NAME             Step container network (default bridge)
  --quiet, -q                Accepted for gcloud compatibility
  --verbosity LEVEL          Accepted for gcloud compatibility; logging is unchanged
  --version                  Show build version
  --help, -h                 Show help

Only local synchronous builds are supported. Unsupported config fields fail explicitly.
Steps run sequentially; waitFor dependencies are validated. Concurrent submissions
share build slots across processes; queue time counts toward the build timeout.
Registry login must be performed using docker login before submission.

Environment defaults (explicit flags take precedence):
  CLOUDSDK_CORE_PROJECT      Default PROJECT_ID
  CLOUDSDK_BUILDS_REGION     Default LOCATION
  GCLOUD_LOCAL_MAX_CONCURRENT_BUILDS  Default for --max-concurrent-builds
  GCLOUD_LOCAL_NO_PUSH       Boolean default for --no-push
  GCLOUD_LOCAL_DRY_RUN       Boolean default for --dry-run
  GCLOUD_LOCAL_BEFOCE_SCRIPT_LINUX  Commands inside each step container before args/script
`

// Version can be set at build time using -ldflags "-X gcloud-local/internal/localbuild.Version=...".
var Version = "dev"

type CLI struct {
	MaxConcurrentBuilds                                 int
	Source, Config, Project, Region, Workspace, Timeout string
	BeforeScriptLinux                                   string
	DockerConfig, Socket, Docker, Network               string
	Substitutions                                       map[string]string
	NoPush, DryRun, Help                                bool
}

func parseAssignments(s string) (map[string]string, error) {
	sep := ","
	if strings.HasPrefix(s, "^") {
		end := strings.Index(s[1:], "^")
		if end < 1 {
			return nil, fmt.Errorf("invalid custom delimiter")
		}
		sep = s[1 : 1+end]
		s = s[2+end:]
	}
	result := map[string]string{}
	for _, pair := range strings.Split(s, sep) {
		k, v, ok := strings.Cut(pair, "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("expected KEY=VALUE in %q", pair)
		}
		result[k] = v
	}
	return result, nil
}

func validateVerbosity(value string) error {
	switch value {
	case "debug", "info", "warning", "error", "critical", "none":
		return nil
	default:
		return fmt.Errorf("invalid verbosity %q; expected debug, info, warning, error, critical, or none", value)
	}
}

func parseCLI(args []string) (CLI, error) {
	o := CLI{MaxConcurrentBuilds: 1, Source: ".", Config: "cloudbuild.yaml", Project: "local", Region: "local", Socket: "/var/run/docker.sock", Docker: "docker", Network: "bridge", Substitutions: map[string]string{}}
	if len(args) == 1 && (args[0] == "--help" || args[0] == "-h") {
		o.Help = true
		return o, nil
	}
	// gcloud accepts compatibility flags before or after the command.
globalFlags:
	for len(args) > 0 {
		key, value, hasValue := strings.Cut(args[0], "=")
		switch key {
		case "--quiet", "-q":
			args = args[1:]
		case "--verbosity":
			if !hasValue {
				if len(args) < 2 {
					return o, fmt.Errorf("missing value for %s", key)
				}
				value = args[1]
				args = args[1:]
			}
			if err := validateVerbosity(value); err != nil {
				return o, err
			}
			args = args[1:]
		default:
			break globalFlags
		}
	}
	if len(args) < 2 || args[0] != "builds" || args[1] != "submit" {
		return o, fmt.Errorf("expected builds submit; run gcloud-local --help")
	}
	args = args[2:]
	sourceSet := false
	explicit := map[string]bool{}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if !strings.HasPrefix(arg, "-") {
			if sourceSet {
				return o, fmt.Errorf("only one source directory is allowed")
			}
			o.Source = arg
			sourceSet = true
			continue
		}
		key, v, has := strings.Cut(arg, "=")
		explicit[key] = true
		switch key {
		case "--help", "-h":
			o.Help = true
			continue
		case "--quiet", "-q":
			continue
		case "--no-push", "--dry-run":
			b := true
			if has {
				var err error
				b, err = strconv.ParseBool(v)
				if err != nil {
					return o, err
				}
			}
			if key == "--no-push" {
				o.NoPush = b
			} else {
				o.DryRun = b
			}
			continue
		}
		switch key {
		case "--max-concurrent-builds", "--config", "--project", "--region", "--workspace-root", "--timeout", "--docker-config", "--docker-socket", "--docker-bin", "--network", "--substitutions", "--verbosity":
		default:
			return o, fmt.Errorf("unsupported flag %s", key)
		}
		if !has {
			i++
			if i >= len(args) {
				return o, fmt.Errorf("missing value for %s", key)
			}
			v = args[i]
		}
		if v == "" {
			return o, fmt.Errorf("empty value for %s", key)
		}
		switch key {
		case "--verbosity":
			if err := validateVerbosity(v); err != nil {
				return o, err
			}
		case "--max-concurrent-builds":
			n, err := positiveConcurrency(v)
			if err != nil {
				return o, fmt.Errorf("%s: %w", key, err)
			}
			o.MaxConcurrentBuilds = n
		case "--config":
			o.Config = v
		case "--project":
			o.Project = v
		case "--region":
			o.Region = v
		case "--workspace-root":
			o.Workspace = v
		case "--timeout":
			o.Timeout = v
		case "--docker-config":
			o.DockerConfig = v
		case "--docker-socket":
			o.Socket = v
		case "--docker-bin":
			o.Docker = v
		case "--network":
			o.Network = v
		case "--substitutions":
			m, e := parseAssignments(v)
			if e != nil {
				return o, e
			}
			for k, v := range m {
				o.Substitutions[k] = v
			}
		}
	}
	if !o.Help {
		if err := applyEnvironment(&o, explicit); err != nil {
			return o, err
		}
	}
	return o, nil
}

type statusError struct {
	code int
	err  error
}

func (e *statusError) Error() string { return e.err.Error() }
func (e *statusError) Unwrap() error { return e.err }
func ExitCode(err error) int {
	var e *statusError
	if errors.As(err, &e) && e.code > 0 && e.code <= 255 {
		return e.code
	}
	return 1
}

func Main(ctx context.Context, args []string, out, logs io.Writer) error {
	if len(args) == 1 && args[0] == "--version" {
		fmt.Fprintln(out, "gcloud-local", Version)
		return nil
	}
	opts, err := parseCLI(args)
	if err != nil {
		return err
	}
	if opts.Help {
		fmt.Fprint(out, usage)
		return nil
	}
	c, err := load(opts.Config)
	if err != nil {
		return err
	}
	var random [8]byte
	if _, err = rand.Read(random[:]); err != nil {
		return err
	}
	id := "local-" + hex.EncodeToString(random[:])
	if opts.DryRun {
		id = "local-dry-run"
	}
	p, err := makePlan(c, opts, id)
	if err != nil {
		return err
	}
	if opts.DryRun {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(p)
	}
	if c.Options.MachineType != "" || c.Options.DiskSizeGb != "" || c.Options.Logging != "" || c.Options.LogStreamingOption != "" {
		fmt.Fprintln(logs, "gcloud-local: cloud machine/disk/logging options are ignored; using local resources")
	}
	fmt.Fprintf(logs, "gcloud-local: source %s; workspace %s\n", p.Source, p.Workspace)
	r := runner{opts: opts, out: out, logs: logs, id: id}
	return r.run(ctx, p)
}

func dockerConfigDir(o CLI) string {
	if o.DockerConfig != "" {
		return o.DockerConfig
	}
	if d := os.Getenv("DOCKER_CONFIG"); d != "" {
		return d
	}
	home, _ := os.UserHomeDir()
	return home + "/.docker"
}
