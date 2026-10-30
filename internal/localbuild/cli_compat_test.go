package localbuild

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestVerbosityCompatibility(t *testing.T) {
	for _, level := range []string{"debug", "info", "warning", "error", "critical", "none"} {
		for _, args := range [][]string{
			{"--verbosity", level, "--quiet", "builds", "submit", "source"},
			{"--quiet", "--verbosity=" + level, "builds", "submit", "source"},
			{"builds", "submit", "--verbosity", level, "source"},
			{"builds", "submit", "source", "--verbosity=" + level},
		} {
			o, err := parseCLI(args)
			if err != nil || o.Source != "source" {
				t.Fatalf("%v: source %q, error %v", args, o.Source, err)
			}
		}
	}
	for _, prefix := range [][]string{nil, {"builds", "submit"}} {
		for _, flags := range [][]string{{"--verbosity"}, {"--verbosity="}, {"--verbosity", "verbose"}, {"--verbosity", "--quiet"}} {
			args := append(append([]string{}, prefix...), flags...)
			if _, err := parseCLI(args); err == nil {
				t.Fatalf("invalid verbosity accepted: %v", args)
			}
		}
	}
}

// Exercise a build submission from an external tool with local behavior
// configured through the environment. No Docker or GCP is needed.
func TestExternalToolSubmission(t *testing.T) {
	t.Setenv("GCLOUD_LOCAL_NO_PUSH", "true")
	t.Setenv("GCLOUD_LOCAL_DRY_RUN", "true")
	root := t.TempDir()
	source := filepath.Join(root, "project")
	configDir := filepath.Join(source, "services", "app")
	if err := os.MkdirAll(configDir, 0755); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(configDir, "cloudbuild.yaml")
	if err := os.WriteFile(config, []byte(`options:
  machineType: N1_HIGHCPU_8
steps:
- name: example/builder:local
  dir: ./services/app
  entrypoint: bash
  args: [-c, 'make build']
  env:
  - CONFIG=$_CONFIG
  - IMAGE_VERSION=$_IMAGE_VERSION
  - TAG=$_TAG
  - REGISTRY=$_REGISTRY
  - OPTIONAL=$_OPTIONAL
substitutions:
  _CONFIG: default
  _IMAGE_VERSION: v0.0.0
  _TAG: placeholder
  _REGISTRY: placeholder
  _OPTIONAL: default
images:
- gcr.io/$PROJECT_ID/app:$_IMAGE_VERSION
tags: [app, '${_TAG}', '${_CONFIG}']
`), 0600); err != nil {
		t.Fatal(err)
	}
	args := []string{
		"builds", "submit", "--verbosity", "info", "--region", "us-central1",
		"--config", config,
		"--substitutions", "_CONFIG=production,_IMAGE_VERSION=v1.0.0,_REGISTRY=localhost:5000/test,_OPTIONAL=,_TAG=v1-local",
		"--project", "local-project", source,
		"--docker-bin=/does/not/exist",
	}
	var out, logs strings.Builder
	if err := Main(context.Background(), args, &out, &logs); err != nil {
		t.Fatal(err)
	}
	var plan Plan
	if err := json.Unmarshal([]byte(out.String()), &plan); err != nil {
		t.Fatal(err)
	}
	if plan.Source != source || len(plan.Steps) != 1 || len(plan.Images) != 1 {
		t.Fatalf("unexpected plan: %+v", plan)
	}
	step := plan.Steps[0]
	if step.Workdir != "/workspace/services/app" || step.Image != "example/builder:local" {
		t.Fatalf("unexpected step: %+v", step)
	}
	wantEnv := "CONFIG=production,IMAGE_VERSION=v1.0.0,OPTIONAL=,REGISTRY=localhost:5000/test,TAG=v1-local"
	if strings.Join(step.Env, ",") != wantEnv || plan.Images[0] != "gcr.io/local-project/app:v1.0.0" {
		t.Fatalf("substitutions lost: %+v", plan)
	}
	if logs.Len() != 0 {
		t.Fatalf("unexpected dry-run logs: %s", logs.String())
	}
}

func TestEnvironmentDefaultsAndOverrides(t *testing.T) {
	t.Setenv("CLOUDSDK_CORE_PROJECT", "env-project")
	t.Setenv("CLOUDSDK_BUILDS_REGION", "env-region")
	t.Setenv("GCLOUD_LOCAL_NO_PUSH", "true")
	t.Setenv("GCLOUD_LOCAL_DRY_RUN", "1")
	o, err := parseCLI([]string{"builds", "submit"})
	if err != nil || o.Project != "env-project" || o.Region != "env-region" || !o.NoPush || !o.DryRun {
		t.Fatalf("environment defaults: %+v, %v", o, err)
	}
	o, err = parseCLI([]string{"builds", "submit", "--project=flag-project", "--region", "flag-region", "--no-push=false", "--dry-run=false"})
	if err != nil || o.Project != "flag-project" || o.Region != "flag-region" || o.NoPush || o.DryRun {
		t.Fatalf("flag overrides: %+v, %v", o, err)
	}
}

func TestInvalidEnvironment(t *testing.T) {
	for _, name := range []string{"GCLOUD_LOCAL_NO_PUSH", "GCLOUD_LOCAL_DRY_RUN"} {
		t.Setenv(name, "")
	}
	for _, tc := range []struct{ name, value string }{
		{"GCLOUD_LOCAL_NO_PUSH", "maybe"},
		{"GCLOUD_LOCAL_DRY_RUN", "maybe"},
	} {
		t.Run(tc.name+"/"+tc.value, func(t *testing.T) {
			t.Setenv(tc.name, tc.value)
			if _, err := parseCLI([]string{"builds", "submit"}); err == nil || !strings.Contains(err.Error(), tc.name) {
				t.Fatalf("invalid environment was not identified: %v", err)
			}
			if o, err := parseCLI([]string{"builds", "submit", "--help"}); err != nil || !o.Help {
				t.Fatalf("invalid environment blocked help: %+v, %v", o, err)
			}
		})
	}
	// Unused invalid boolean defaults must not block explicit flags.
	t.Setenv("GCLOUD_LOCAL_NO_PUSH", "invalid")
	t.Setenv("GCLOUD_LOCAL_DRY_RUN", "invalid")
	if _, err := parseCLI([]string{"builds", "submit", "--no-push=false", "--dry-run"}); err != nil {
		t.Fatal(err)
	}
}
