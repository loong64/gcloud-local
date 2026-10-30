package localbuild

import (
	"fmt"
	"path"
	"regexp"
	"strings"

	"github.com/distribution/reference"
)

var volumeName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)

// Local Docker constraints. gcloud's config.FinalizeCloudbuildConfig does not
// perform image/volume checks; Cloud Build validates these on the service.
func validateImage(image string) error {
	if _, err := reference.ParseNormalizedNamed(image); err != nil {
		return fmt.Errorf("invalid Docker image reference %q: %w", image, err)
	}
	return nil
}

func pathsOverlap(a, b string) bool {
	return a == b || a == "/" || b == "/" || strings.HasPrefix(a, b+"/") || strings.HasPrefix(b, a+"/")
}

func validateVolumes(volumes []Volume) error {
	names := map[string]bool{}
	for i, v := range volumes {
		if !volumeName.MatchString(v.Name) || !path.IsAbs(v.Path) || path.Clean(v.Path) != v.Path || strings.ContainsAny(v.Path, ",\x00") {
			return fmt.Errorf("invalid volume %+v", v)
		}
		if names[v.Name] {
			return fmt.Errorf("duplicate volume name %q", v.Name)
		}
		names[v.Name] = true
		// Ancestors must also be rejected: /builder masks /builder/home, for example.
		for _, reserved := range []string{"/workspace", "/gcloud-local", "/builder/home", "/root", "/var/run/docker.sock"} {
			if pathsOverlap(v.Path, reserved) {
				return fmt.Errorf("conflicting volume path %s (reserved %s)", v.Path, reserved)
			}
		}
		for _, previous := range volumes[:i] {
			if pathsOverlap(v.Path, previous.Path) {
				return fmt.Errorf("conflicting volume paths %s and %s", previous.Path, v.Path)
			}
		}
	}
	return nil
}
