package localbuild

import (
	"fmt"
	"regexp"
	"strings"
)

// gcloud submits substitution data unchanged; expansion is a local equivalent
// of the Cloud Build service, not an implementation supplied by the gcloud CLI.
var userKey = regexp.MustCompile(`^_[A-Z0-9_]+$`)
var templateToken = regexp.MustCompile(`\$\$|\$\{([A-Z_][A-Z0-9_]*)\}|\$([A-Z_][A-Z0-9_]*)`)

func templateKey(t string) string {
	return strings.TrimSuffix(strings.TrimPrefix(t[1:], "{"), "}")
}

func substitute(s string, values map[string]string, loose bool) (string, error) {
	var err error
	out := templateToken.ReplaceAllStringFunc(s, func(t string) string {
		if t == "$$" {
			return "$"
		}
		key := templateKey(t)
		if value, ok := values[key]; ok {
			return value
		}
		if loose && userKey.MatchString(key) {
			return ""
		}
		if err == nil {
			err = fmt.Errorf("unknown substitution %s; shell variables must use $$", t)
		}
		return t
	})
	return out, err
}

func substitutionValues(config map[string]string, opts CLI, buildID string) (map[string]string, map[string]string, error) {
	values := map[string]string{"PROJECT_ID": opts.Project, "PROJECT_NUMBER": "", "BUILD_ID": buildID, "LOCATION": opts.Region}
	for _, k := range []string{"TRIGGER_NAME", "COMMIT_SHA", "REVISION_ID", "SHORT_SHA", "REPO_NAME", "REPO_FULL_NAME", "BRANCH_NAME", "TAG_NAME", "REF_NAME", "TRIGGER_BUILD_CONFIG_PATH", "SERVICE_ACCOUNT_EMAIL", "SERVICE_ACCOUNT", "_HEAD_BRANCH", "_BASE_BRANCH", "_HEAD_REPO_URL", "_PR_NUMBER"} {
		values[k] = ""
	}
	merged, err := mergeSubstitutions(config, opts.Substitutions)
	if err != nil {
		return nil, nil, err
	}
	custom := map[string]string{}
	for k, v := range merged {
		if !userKey.MatchString(k) {
			if _, ok := values[k]; !ok || k == "PROJECT_ID" || k == "BUILD_ID" {
				return nil, nil, fmt.Errorf("invalid substitution override %q", k)
			}
		} else {
			custom[k] = v
		}
		values[k] = v
	}
	if len(custom) > 200 {
		return nil, nil, fmt.Errorf("too many custom substitutions (max 200)")
	}
	for k, v := range custom {
		values[k] = v
	}
	for k, v := range values {
		if len(k) > 100 || len(v) > 4000 {
			return nil, nil, fmt.Errorf("substitution %q exceeds key/value limits (100/4000 bytes)", k)
		}
	}
	return values, custom, nil
}
