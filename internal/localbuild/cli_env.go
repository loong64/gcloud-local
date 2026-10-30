package localbuild

import (
	"fmt"
	"os"
	"strconv"
)

// Use Cloud SDK property names where equivalents exist. Local-only options
// have their own namespace; none of these settings require a shell wrapper.
func applyEnvironment(o *CLI, explicit map[string]bool) error {
	for _, setting := range []struct {
		flag, env string
		target    *string
	}{
		{"--project", "CLOUDSDK_CORE_PROJECT", &o.Project},
		{"--region", "CLOUDSDK_BUILDS_REGION", &o.Region},
	} {
		if value := os.Getenv(setting.env); value != "" && !explicit[setting.flag] {
			*setting.target = value
		}
	}
	for _, setting := range []struct {
		flag, env string
		target    *bool
	}{
		{"--no-push", "GCLOUD_LOCAL_NO_PUSH", &o.NoPush},
		{"--dry-run", "GCLOUD_LOCAL_DRY_RUN", &o.DryRun},
	} {
		if value := os.Getenv(setting.env); value != "" && !explicit[setting.flag] {
			parsed, err := strconv.ParseBool(value)
			if err != nil {
				return fmt.Errorf("%s: expected a boolean: %w", setting.env, err)
			}
			*setting.target = parsed
		}
	}
	if value := os.Getenv("GCLOUD_LOCAL_MAX_CONCURRENT_BUILDS"); value != "" && !explicit["--max-concurrent-builds"] {
		n, err := positiveConcurrency(value)
		if err != nil {
			return fmt.Errorf("GCLOUD_LOCAL_MAX_CONCURRENT_BUILDS: %w", err)
		}
		o.MaxConcurrentBuilds = n
	}
	if value := os.Getenv("GCLOUD_LOCAL_BUILDER_IMAGES"); value != "" {
		images, err := parseAssignments(value)
		if err != nil {
			return fmt.Errorf("GCLOUD_LOCAL_BUILDER_IMAGES: %w", err)
		}
		for old, replacement := range images {
			if replacement == "" {
				return fmt.Errorf("GCLOUD_LOCAL_BUILDER_IMAGES: builder-image expects OLD=NEW with a nonempty replacement")
			}
			if _, overridden := o.BuilderImages[old]; !overridden {
				o.BuilderImages[old] = replacement
			}
		}
	}
	return nil
}

func positiveConcurrency(value string) (int, error) {
	n, err := strconv.Atoi(value)
	if err != nil || n < 1 {
		return 0, fmt.Errorf("expected a positive integer, got %q", value)
	}
	return n, nil
}
