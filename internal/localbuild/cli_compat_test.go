package localbuild

import (
	"context"
	"encoding/json"
	"io"
	"path/filepath"
	"strings"
	"testing"
)

func TestGlobalProjectPositions(t *testing.T) {
	t.Setenv("CLOUDSDK_CORE_PROJECT", "env-project")
	for _, args := range [][]string{
		{"--project=sample", "builds", "submit"},
		{"--project", "sample", "--quiet", "builds", "submit"},
		{"builds", "--project=sample", "submit"},
		{"builds", "submit", "--project=sample"},
		{"--verbosity=info", "--project=old", "builds", "submit", "--project=sample"},
	} {
		o, err := parseCLI(args)
		if err != nil || o.Project != "sample" {
			t.Fatalf("%v: got %q, %v", args, o.Project, err)
		}
	}
	for _, args := range [][]string{
		{"--project=", "builds", "submit"},
		{"builds", "--project"},
		{"--project", "--quiet", "builds", "submit"},
		{"--project", "-q", "builds", "submit"},
	} {
		if _, err := parseCLI(args); err == nil {
			t.Fatalf("missing project accepted: %v", args)
		}
	}
	file := filepath.Join(t.TempDir(), "cloudbuild.yaml")
	writeTestFile(t, file, "steps: [{name: builder, args: ['$PROJECT_ID']}]", 0600)
	var out strings.Builder
	err := Main(context.Background(), []string{"--project=sample", "builds", "submit", "--no-source", "--config", file, "--dry-run"}, &out, io.Discard)
	var p Plan
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(out.String()), &p); err != nil || p.Steps[0].Step.Args[0] != "sample" {
		t.Fatalf("project not passed to build: %s, %v", &out, err)
	}
}

func TestNoCacheRequiresUnsupportedKaniko(t *testing.T) {
	for _, flags := range [][]string{
		{"--no-cache"}, {"--tag=app", "--no-cache"}, {"--tag=app", "--no-cache=true"},
	} {
		_, err := parseCLI(append([]string{"builds", "submit"}, flags...))
		if err == nil || !strings.Contains(err.Error(), "Kaniko") {
			t.Fatalf("%v: expected Kaniko error, got %v", flags, err)
		}
	}
	if _, err := parseCLI([]string{"builds", "submit", "--tag=app", "--no-cache=false"}); err != nil {
		t.Fatal(err)
	}
}

func TestVerbosityCompatibility(t *testing.T) {
	for _, args := range [][]string{
		{"--verbosity", "debug", "--quiet", "builds", "submit", "source"},
		{"--quiet", "--verbosity=info", "builds", "submit", "source"},
		{"builds", "submit", "--verbosity", "warning", "source"},
		{"builds", "submit", "source", "--verbosity=error"},
		{"builds", "submit", "--verbosity=critical", "source"},
		{"builds", "submit", "--verbosity=none", "source"},
	} {
		o, err := parseCLI(args)
		if err != nil || o.Source != "source" {
			t.Fatalf("%v: source %q, error %v", args, o.Source, err)
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
