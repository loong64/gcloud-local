package localbuild

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testCLI() CLI { o, _ := parseCLI([]string{"builds", "submit"}); return o }
func TestSubstitutions(t *testing.T) {
	values := map[string]string{"_TAG": "v1", "PROJECT_ID": "local", "_INDIRECT": "$_TAG"}
	for _, tc := range []struct{ in, want string }{
		{"${_TAG}-$_TAG/$PROJECT_ID", "v1-v1/local"},
		{"$$HOME $${_TAG} $$$_TAG", "$HOME ${_TAG} $v1"},
		{"${_TAG}suffix", "v1suffix"},
		{"$_INDIRECT", "$_TAG"},
	} {
		got, err := substitute(tc.in, values, false)
		if err != nil || got != tc.want {
			t.Errorf("%q: %q, %v", tc.in, got, err)
		}
	}
	if _, err := substitute("$HOME", values, false); err == nil {
		t.Fatal("unescaped shell variable accepted")
	}
	if got, err := substitute("$_MISSING", values, true); err != nil || got != "" {
		t.Fatal(got, err)
	}
	if _, err := substitute("$_MISSING", values, false); err == nil {
		t.Fatal("missing strict substitution accepted")
	}
}
func TestCLI(t *testing.T) {
	o, err := parseCLI([]string{"--quiet", "builds", "submit", "--config=x.yaml", "source", "--substitutions", "^;^_A=a,b;_B=x=y", "--substitutions=_C=3", "--no-push", "--dry-run=false"})
	if err != nil || o.Source != "source" || o.Config != "x.yaml" || o.Substitutions["_A"] != "a,b" || o.Substitutions["_B"] != "x=y" || !o.NoPush || o.DryRun {
		t.Fatal(o, err)
	}
	for _, args := range [][]string{{"builds", "submit", "--async"}, {"builds", "submit", "--config"}, {"builds", "submit", "a", "b"}} {
		if _, err := parseCLI(args); err == nil {
			t.Fatal("invalid args accepted", args)
		}
	}
}
func fixture(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	source := filepath.Join(root, "services", "app")
	for _, p := range []string{source, filepath.Join(root, ".git"), filepath.Join(root, "output")} {
		if err := os.MkdirAll(p, 0755); err != nil {
			t.Fatal(err)
		}
	}
	return root, source
}
func TestPlanPathsSubstitutionsAndEnv(t *testing.T) {
	root, source := fixture(t)
	o := testCLI()
	o.Source = source
	o.Substitutions["_REGISTRY"] = "ghcr.io/example/registry"
	c := Config{Steps: []Step{{Name: "gcr.io/cloud-builders/docker", Dir: "output", Env: []string{"X=step", "TAG=$_TAG"}, Args: []string{"build", "."}}}, Substitutions: map[string]string{"_TAG": "v1"}, Options: Options{Env: []string{"X=global"}}, Images: []string{"${_REGISTRY}/test:$_TAG", "gcr.io/$PROJECT_ID/test:$_TAG"}}
	p, err := makePlan(c, o, "test")
	if err != nil {
		t.Fatal(err)
	}
	if p.Workspace != root || p.Steps[0].Workdir != "/workspace/services/app/output" || p.Images[0] != "ghcr.io/example/registry/test:v1" || p.Images[1] != "gcr.io/local/test:v1" || p.Steps[0].Image != "gcr.io/cloud-builders/docker" {
		t.Fatalf("%+v", p)
	}
	if strings.Join(p.Steps[0].Env, ",") != "TAG=v1,X=step" {
		t.Fatal(p.Steps[0].Env)
	}
	c.Steps[0].Dir = ""
	p, err = makePlan(c, o, "test")
	if err != nil || p.Steps[0].Workdir != "/workspace/services/app" {
		t.Fatal(p, err)
	}
	c.Steps[0].Dir = "../../../../"
	if _, err = makePlan(c, o, "test"); err == nil {
		t.Fatal("workspace escape accepted")
	}
}
func TestValidation(t *testing.T) {
	_, source := fixture(t)
	o := testCLI()
	o.Source = source
	cases := []Config{
		{Steps: []Step{{Name: "image", WaitFor: []string{"future"}}}},
		{Steps: []Step{{Name: "image", ID: "same"}, {Name: "image", ID: "same"}}},
		{Steps: []Step{{Name: "image", Script: "echo hi", Args: []string{"hi"}}}},
		{Steps: []Step{{Name: "image", Volumes: []Volume{{Name: "data", Path: "/workspace"}}}}},
		{Steps: []Step{{Name: "image"}}, Timeout: "0s"},
		{Steps: []Step{{Name: "image", Dir: "/etc"}}},
	}
	for _, c := range cases {
		if _, err := makePlan(c, o, "test"); err == nil {
			t.Fatalf("invalid config accepted: %+v", c)
		}
	}
	c := Config{Steps: []Step{{Name: "image", ID: "one"}, {Name: "image", WaitFor: []string{"one"}}}}
	if _, err := makePlan(c, o, "test"); err != nil {
		t.Fatal(err)
	}
}
func TestRejectUnsupportedFields(t *testing.T) {
	for _, body := range []string{"steps: [{name: alpine, secretEnv: [TOKEN]}]", "steps: [{name: alpine}]\navailableSecrets: {}", "steps: [{name: alpine}]\noptions: {dynamicSubstitutions: true}", "steps: [{name: alpine}]\n---\nsteps: []"} {
		file := filepath.Join(t.TempDir(), "cloudbuild.yaml")
		os.WriteFile(file, []byte(body), 0600)
		if _, err := load(file); err == nil {
			t.Fatal("unsupported config accepted", body)
		}
	}
}
func TestAutomapAndScript(t *testing.T) {
	o := testCLI()
	o.Source = t.TempDir()
	c := Config{Substitutions: map[string]string{"_X": "hi"}, Options: Options{Automap: true}, Steps: []Step{{Name: "image", Script: "echo $_X", Env: []string{"_X=override"}}}}
	p, err := makePlan(c, o, "id")
	if err != nil {
		t.Fatal(err)
	}
	if p.Steps[0].Step.Script != "echo $_X" || !strings.Contains(strings.Join(p.Steps[0].Env, ","), "_X=override") {
		t.Fatal(p)
	}
}
func TestDryRunNoDocker(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "cloudbuild.yaml")
	os.WriteFile(file, []byte("steps: [{name: image}]"), 0600)
	var out, logs strings.Builder
	err := Main(context.Background(), []string{"builds", "submit", root, "--config", file, "--dry-run", "--docker-bin=/does/not/exist"}, &out, &logs)
	if err != nil {
		t.Fatal(err)
	}
	var p Plan
	if err = json.Unmarshal([]byte(out.String()), &p); err != nil || len(p.Steps) != 1 {
		t.Fatal(out.String(), err)
	}
}
