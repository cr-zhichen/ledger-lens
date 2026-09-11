package version

import (
	"regexp"
	"strings"
)

// These values are injected by the build task from the release tag and Git metadata.
var (
	Version   = "dev"
	Commit    = "unknown"
	BuildDate = "unknown"
)

type Info struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	BuildDate string `json:"build_date"`
}

func Current() Info {
	return Info{Version: Version, Commit: Commit, BuildDate: BuildDate}
}

var stablePattern = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)

func IsStable(value string) bool {
	return stablePattern.MatchString(value)
}

func FromTag(tag string) (string, bool) {
	value, found := strings.CutPrefix(tag, "v")
	return value, found && IsStable(value)
}

// Compare compares stable versions numerically, without limiting component precision.
// The boolean is false for development builds or unsupported version formats.
func Compare(a, b string) (int, bool) {
	if !IsStable(a) || !IsStable(b) {
		return 0, false
	}
	aParts, bParts := strings.Split(a, "."), strings.Split(b, ".")
	for i, part := range aParts {
		if len(part) < len(bParts[i]) {
			return -1, true
		}
		if len(part) > len(bParts[i]) {
			return 1, true
		}
		if cmp := strings.Compare(part, bParts[i]); cmp != 0 {
			return cmp, true
		}
	}
	return 0, true
}
