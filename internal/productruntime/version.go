package productruntime

import (
	"regexp"
	"strconv"
	"strings"
)

const (
	numericVersionID    = `(?:0|[1-9][0-9]*)`
	nonnumericVersionID = `(?:[0-9]*[A-Za-z-][0-9A-Za-z-]*)`
	prereleaseVersion   = `(?:` + numericVersionID + `|` + nonnumericVersionID + `)(?:\.(?:` + numericVersionID + `|` + nonnumericVersionID + `))*`
	buildVersion        = `[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*`
)

var versionPattern = regexp.MustCompile(`^v?(` + numericVersionID + `)\.(` + numericVersionID + `)\.(` + numericVersionID + `)(?:-` + prereleaseVersion + `)?(?:\+` + buildVersion + `)?$`)

// VersionAtLeast compares numeric semantic-version cores; valid suffixes never upgrade the core.
func VersionAtLeast(value, floor string) bool {
	got, gotOK := parseVersionCore(value)
	want, wantOK := parseVersionCore(floor)
	if !gotOK || !wantOK {
		return false
	}
	for index := range got {
		if got[index] != want[index] {
			return got[index] > want[index]
		}
	}
	return true
}

func parseVersionCore(value string) ([3]uint32, bool) {
	var result [3]uint32
	parts := versionPattern.FindStringSubmatch(strings.TrimSpace(value))
	if len(parts) != len(result)+1 {
		return result, false
	}
	for index, part := range parts[1:] {
		number, err := strconv.ParseUint(part, 10, 32)
		if err != nil {
			return result, false
		}
		result[index] = uint32(number)
	}
	return result, true
}
