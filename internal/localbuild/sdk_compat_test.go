package localbuild

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// These checked-in fixtures were captured by executing Google Cloud CLI 591.0.0
// config.LoadCloudbuildConfigFromStream and submit_util._GetBuildTimeout offline.
func TestSDKFixtures(t *testing.T) {
	data, err := os.ReadFile("testdata/gcloud-sdk.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixtures struct {
		Assignments []struct {
			Input  string
			Error  bool
			Result map[string]string
		}
		Configs []struct {
			Name, YAML string
			Params     map[string]string
			Error      bool
			Result     json.RawMessage
		}
		Timeouts []struct {
			Input   string
			Error   bool
			Seconds int64
		}
		TagArgs []string
	}
	if err := json.Unmarshal(data, &fixtures); err != nil {
		t.Fatal(err)
	}
	for _, tc := range fixtures.Assignments {
		t.Run("assignments/"+tc.Input, func(t *testing.T) {
			got, err := parseAssignments(tc.Input)
			if (err != nil) != tc.Error || (!tc.Error && !reflect.DeepEqual(got, tc.Result)) {
				t.Fatalf("got %v, %v; SDK expects %v, error=%v", got, err, tc.Result, tc.Error)
			}
		})
	}
	for _, tc := range fixtures.Configs {
		t.Run("config/"+tc.Name, func(t *testing.T) {
			file := filepath.Join(t.TempDir(), "cloudbuild.yaml")
			writeTestFile(t, file, tc.YAML, 0600)
			config, err := load(file)
			if err == nil {
				config.Substitutions, err = mergeSubstitutions(config.Substitutions, tc.Params)
			}
			if (err != nil) != tc.Error {
				t.Fatalf("got error %v; SDK error=%v", err, tc.Error)
			}
			if tc.Error {
				return
			}
			steps := []map[string]any{}
			for _, s := range config.Steps {
				steps = append(steps, map[string]any{
					"name": s.Name, "args": append([]string{}, s.Args...), "env": append([]string{}, s.Env...),
					"waitFor": append([]string{}, s.WaitFor...), "allowFailure": s.AllowFailure,
					"allowExitCodes": append([]int{}, s.AllowExitCodes...), "automap": s.Automap,
					"script": s.Script, "dir": s.Dir,
				})
			}
			gotBytes, err := json.Marshal(map[string]any{
				"steps": steps, "substitutions": config.Substitutions,
				"env": append([]string{}, config.Options.Env...), "automap": config.Options.Automap,
				"timeout": config.Timeout, "tags": append([]string{}, config.Tags...),
			})
			if err != nil {
				t.Fatal(err)
			}
			var got, want any
			if err := json.Unmarshal(gotBytes, &got); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(tc.Result, &want); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("local: %s\nSDK: %s", gotBytes, tc.Result)
			}
		})
	}
	for _, tc := range fixtures.Timeouts {
		t.Run("timeout/"+tc.Input, func(t *testing.T) {
			got, err := sdkTimeout(tc.Input)
			if (err != nil) != tc.Error || (!tc.Error && got != time.Duration(tc.Seconds)*time.Second) {
				t.Fatalf("got %s, %v; SDK expects %ds, error=%v", got, err, tc.Seconds, tc.Error)
			}
		})
	}
	t.Run("tag", func(t *testing.T) {
		root := t.TempDir()
		writeTestFile(t, filepath.Join(root, "Dockerfile"), "FROM scratch\n", 0600)
		o := testCLI()
		o.Source, o.Tag = root, "gcr.io/example/app:local"
		config, err := submissionConfig(o)
		if err != nil {
			t.Fatal(err)
		}
		// Adapt only the internal builder's docker prefix and cloud-only network.
		want := []string{}
		for i := 1; i < len(fixtures.TagArgs); i++ {
			if fixtures.TagArgs[i] == "--network" {
				i++
				continue
			}
			want = append(want, fixtures.TagArgs[i])
		}
		if !reflect.DeepEqual(config.Steps[0].Args, want) {
			t.Fatalf("local: %v; adapted SDK: %v", config.Steps[0].Args, want)
		}
	})
}

func TestNoSourcePlan(t *testing.T) {
	o, err := parseCLI([]string{"builds", "submit", "--no-source", "--dry-run"})
	if err != nil {
		t.Fatal(err)
	}
	p, err := makePlan(Config{Steps: []Step{{Name: "builder"}}}, o, "id")
	if err != nil || !p.NoSource || p.Source != "" || p.Workspace != "" || p.Steps[0].Workdir != "/workspace" || p.Timeout != time.Hour {
		t.Fatalf("unexpected source-free plan: %+v, %v", p, err)
	}
	for _, args := range [][]string{
		{"--no-source", "."}, {"--no-source", "--workspace-root=/tmp"},
		{"--no-source", "--tag=app"}, {"--no-cache"},
	} {
		if _, err := parseCLI(append([]string{"builds", "submit"}, args...)); err == nil {
			t.Fatalf("invalid local submission accepted: %v", args)
		}
	}
}

func TestLocalDockerRules(t *testing.T) {
	if err := validateImage("image@sha256:blah"); err == nil {
		t.Fatal("malformed digest accepted")
	}
	if err := validateVolumes([]Volume{{Name: "v", Path: "/cache"}}); err != nil {
		t.Fatalf("valid single-character Docker volume name rejected: %v", err)
	}
}

func TestSDKLocalNumericBounds(t *testing.T) {
	for _, value := range []string{"0.5s", "1000000000000000000s"} {
		if _, err := sdkTimeout(value); err == nil {
			t.Fatalf("unrepresentable local timeout accepted: %s", value)
		}
	}
	file := filepath.Join(t.TempDir(), "cloudbuild.yaml")
	writeTestFile(t, file, `steps: [{name: builder, allowExitCodes: ['9223372036854775807', '-9223372036854775808']}]
options: {diskSizeGb: '7'}
`, 0600)
	c, err := load(file)
	if err != nil || c.Options.DiskSizeGb != "7" || c.Steps[0].AllowExitCodes[0] != 9223372036854775807 || c.Steps[0].AllowExitCodes[1] != -9223372036854775808 {
		t.Fatalf("int64 limits did not decode: %+v, %v", c, err)
	}
	writeTestFile(t, file, "steps: [{name: builder, allowExitCodes: ['9223372036854775808']}]", 0600)
	if _, err := load(file); err == nil {
		t.Fatal("out-of-range local int64 accepted")
	}
}
