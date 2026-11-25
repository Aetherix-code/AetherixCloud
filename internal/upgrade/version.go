package upgrade

import (
	"strings"
)

// Normalize returns a tag with a leading "v" when the rest looks like
// MAJOR.MINOR…; otherwise returns the trimmed raw string (e.g. "dev").
func Normalize(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}
	v = strings.TrimPrefix(v, "V")
	if !strings.HasPrefix(v, "v") {
		if looksSemver(v) {
			return "v" + v
		}
		return v
	}
	return v
}

func looksSemver(v string) bool {
	// crude: starts with a digit and contains a dot
	if v == "" || v[0] < '0' || v[0] > '9' {
		return false
	}
	return strings.Contains(v, ".")
}

// Equal reports whether two version strings refer to the same release.
func Equal(a, b string) bool {
	return Normalize(a) == Normalize(b)
}

// ArchiveName returns the goreleaser asset name for linux/<arch>.
func ArchiveName(version, arch string) string {
	v := strings.TrimPrefix(Normalize(version), "v")
	return "aetherixcloud_" + v + "_linux_" + arch + ".tar.gz"
}
