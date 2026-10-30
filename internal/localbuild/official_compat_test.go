package localbuild

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestPlanDoesNotMutateConfig(t *testing.T) {
	o := testCLI()
	o.Source = t.TempDir()
	c := Config{Steps: []Step{{Name: "builder", Args: []string{"$$HOME", "$_VALUE"}}}, Substitutions: map[string]string{"_VALUE": "$_LITERAL"}}
	first, err := makePlan(c, o, "id")
	if err != nil {
		t.Fatal(err)
	}
	second, err := makePlan(c, o, "id")
	if err != nil || !reflect.DeepEqual(first, second) {
		t.Fatalf("repeated planning changed result: %v", err)
	}
	if got := c.Steps[0].Args; !reflect.DeepEqual(got, []string{"$$HOME", "$_VALUE"}) {
		t.Fatalf("config mutated: %v", got)
	}
	o.Substitutions["_VALUE"] = "another"
	third, err := makePlan(c, o, "id")
	if err != nil || third.Steps[0].Step.Args[1] != "another" || first.Steps[0].Step.Args[1] != "$_LITERAL" {
		t.Fatalf("plans share substituted args: %+v %v", third, err)
	}
}

func TestStrictAndLooseSubstitutionMatching(t *testing.T) {
	o := testCLI()
	o.Source = t.TempDir()
	c := Config{Steps: []Step{{Name: "builder"}}, Substitutions: map[string]string{"_UNUSED": "value"}}
	if _, err := makePlan(c, o, "id"); err == nil || !strings.Contains(err.Error(), "_UNUSED") {
		t.Fatalf("unused strict key accepted: %v", err)
	}
	c.Options.SubstitutionOption = "ALLOW_LOOSE"
	c.Steps[0].Args = []string{"before${_MISSING}after", "$$HOME"}
	p, err := makePlan(c, o, "id")
	if err != nil || !reflect.DeepEqual(p.Steps[0].Step.Args, []string{"beforeafter", "$HOME"}) {
		t.Fatalf("loose substitution: %+v %v", p, err)
	}
	c.Steps[0].Args = []string{"$NOT_A_BUILTIN"}
	if _, err := makePlan(c, o, "id"); err == nil {
		t.Fatal("ALLOW_LOOSE accepted an invalid builtin")
	}
	c.Options.SubstitutionOption = ""
	c.Steps[0].Args = nil
	c.Tags = []string{"${_UNUSED}", "value", "$BRANCH_NAME"}
	p, err = makePlan(c, o, "id")
	if err != nil || !reflect.DeepEqual(p.Tags, []string{"value", "value", ""}) {
		t.Fatalf("tag substitutions: %+v %v", p, err)
	}
}

func TestSubstitutionLimits(t *testing.T) {
	o := testCLI()
	for _, data := range []map[string]string{
		{"_X": strings.Repeat("x", 4001)},
		{"_" + strings.Repeat("X", 100): "v"},
	} {
		if _, _, err := substitutionValues(data, o, "id"); err == nil {
			t.Fatal("oversized substitution accepted")
		}
	}
	data := map[string]string{}
	for i := 0; i < 200; i++ {
		data[fmt.Sprintf("_KEY_%d", i)] = "v"
	}
	if _, _, err := substitutionValues(data, o, "id"); err != nil {
		t.Fatal(err)
	}
	o.Substitutions["_EXTRA"] = "v"
	if _, _, err := substitutionValues(data, o, "id"); err == nil {
		t.Fatal("merged substitution count limit not enforced")
	}
}

func TestVolumeMountConflicts(t *testing.T) {
	for _, volumes := range [][]Volume{
		{{Name: "cache", Path: "/builder"}},
		{{Name: "cache", Path: "/builder/home/cache"}},
		{{Name: "cache", Path: "/var"}},
		{{Name: "cache", Path: "/cache,readonly"}},
		{{Name: "cache", Path: "/cache"}, {Name: "cache", Path: "/other"}},
		{{Name: "cache", Path: "/cache"}, {Name: "other", Path: "/cache/nested"}},
	} {
		if err := validateVolumes(volumes); err == nil {
			t.Fatalf("conflicting volumes accepted: %+v", volumes)
		}
	}
	if err := validateVolumes([]Volume{{Name: "cache", Path: "/cache"}, {Name: "other", Path: "/cache-other"}}); err != nil {
		t.Fatal(err)
	}
}

func TestPostSubstitutionValidation(t *testing.T) {
	o := testCLI()
	o.Source = t.TempDir()
	for _, c := range []Config{
		{Steps: []Step{{Name: "${_MISSING}"}}, Options: Options{SubstitutionOption: "ALLOW_LOOSE"}},
		{Steps: []Step{{Name: "Upper/Case"}}},
		{Steps: []Step{{Name: "builder"}}, Images: []string{"--help"}},
		{Steps: []Step{{Name: "builder", Timeout: "2m"}}, Timeout: "1m"},
	} {
		if _, err := makePlan(c, o, "id"); err == nil {
			t.Fatalf("invalid plan accepted: %+v", c)
		}
	}
	if d, err := duration("120", time.Minute); err != nil || d != 2*time.Minute {
		t.Fatalf("unitless SDK timeout: %s %v", d, err)
	}
}

func TestTimeoutEnvironmentPrecedence(t *testing.T) {
	t.Setenv("CLOUDSDK_BUILDS_TIMEOUT", "120")
	for _, tc := range []struct {
		flags []string
		want  time.Duration
	}{
		{nil, 2 * time.Minute},
		{[]string{"--timeout=3m"}, 3 * time.Minute},
	} {
		o, err := parseCLI(append([]string{"builds", "submit"}, tc.flags...))
		if err != nil {
			t.Fatal(err)
		}
		o.Source = t.TempDir()
		p, err := makePlan(Config{Steps: []Step{{Name: "builder", Timeout: "30s"}}, Timeout: "1m"}, o, "id")
		if err != nil || p.Timeout != tc.want || p.Steps[0].Timeout != 30*time.Second {
			t.Fatalf("timeout precedence: %s, %v", p.Timeout, err)
		}
	}
}

func TestTagSubmissionDryRun(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "Dockerfile"), "FROM scratch\n", 0644)
	for _, flag := range []string{"--tag", "-t"} {
		var out strings.Builder
		err := Main(context.Background(), []string{"builds", "submit", root, flag, "local/${_NAME}:test", "--substitutions=_NAME=app", "--dry-run", "--docker-bin=/missing"}, &out, io.Discard)
		if err != nil {
			t.Fatal(err)
		}
		var p Plan
		if err := json.Unmarshal([]byte(out.String()), &p); err != nil {
			t.Fatal(err)
		}
		if len(p.Steps) != 1 || p.Images[0] != "local/app:test" || p.Steps[0].Step.Args[3] != p.Images[0] || p.Steps[0].Step.Args[1] != "--no-cache" {
			t.Fatalf("unexpected Docker build plan: %+v", p)
		}
	}
	if _, err := parseCLI([]string{"builds", "submit", "--tag=app", "--config=custom.yaml"}); err == nil {
		t.Fatal("tag/config conflict accepted")
	}
	o := testCLI()
	o.Tag, o.Source = "app", t.TempDir()
	if _, err := submissionConfig(o); err == nil {
		t.Fatal("tag submission without Dockerfile accepted")
	}
}

func TestMissingOutputsFailBeforeAnyPush(t *testing.T) {
	for _, noPush := range []bool{false, true} {
		root := t.TempDir()
		socket := filepath.Join(root, "docker.sock")
		listener, err := net.Listen("unix", socket)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { listener.Close() })
		docker := filepath.Join(root, "docker")
		writeTestFile(t, docker, `#!/bin/sh
shift 4
case "$1 $2" in
 'image inspect') [ "$3" != missing:tag ] || exit 1 ;;
 push*) echo PUSH_ATTEMPT ;;
esac
`, 0755)
		o := testCLI()
		o.Source, o.Socket, o.Docker, o.DockerConfig, o.NoPush = root, socket, docker, root, noPush
		p, err := makePlan(Config{Steps: []Step{{Name: "builder"}}, Images: []string{"existing:tag", "missing:tag"}}, o, "test")
		if err != nil {
			t.Fatal(err)
		}
		var output strings.Builder
		r := runner{opts: o, out: &output, logs: &output, id: "test"}
		err = r.run(context.Background(), p)
		if err == nil || !strings.Contains(err.Error(), "missing:tag") || strings.Contains(output.String(), "PUSH_ATTEMPT") || strings.Contains(output.String(), "DONE\n") {
			t.Fatalf("noPush=%v: %v\n%s", noPush, err, &output)
		}
	}
}
