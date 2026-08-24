package extpkg

import (
	"strconv"
	"strings"
)

type packageVersion struct {
	major, minor, patch int
	prerelease          []string
}

func compareVersions(left, right string) int {
	l, lok := parsePackageVersion(left)
	r, rok := parsePackageVersion(right)
	if !lok || !rok {
		return strings.Compare(left, right)
	}
	for _, pair := range [][2]int{{l.major, r.major}, {l.minor, r.minor}, {l.patch, r.patch}} {
		if pair[0] < pair[1] {
			return -1
		}
		if pair[0] > pair[1] {
			return 1
		}
	}
	if len(l.prerelease) == 0 && len(r.prerelease) != 0 {
		return 1
	}
	if len(l.prerelease) != 0 && len(r.prerelease) == 0 {
		return -1
	}
	for i := 0; i < len(l.prerelease) && i < len(r.prerelease); i++ {
		ln, le := strconv.Atoi(l.prerelease[i])
		rn, re := strconv.Atoi(r.prerelease[i])
		switch {
		case le == nil && re == nil && ln != rn:
			if ln < rn {
				return -1
			}
			return 1
		case le == nil && re != nil:
			return -1
		case le != nil && re == nil:
			return 1
		case l.prerelease[i] != r.prerelease[i]:
			return strings.Compare(l.prerelease[i], r.prerelease[i])
		}
	}
	if len(l.prerelease) < len(r.prerelease) {
		return -1
	}
	if len(l.prerelease) > len(r.prerelease) {
		return 1
	}
	return 0
}

func parsePackageVersion(value string) (packageVersion, bool) {
	value = strings.TrimPrefix(value, "v")
	buildParts := strings.SplitN(value, "+", 2)
	if len(buildParts) == 2 {
		for _, identifier := range strings.Split(buildParts[1], ".") {
			if identifier == "" {
				return packageVersion{}, false
			}
		}
	}
	value = buildParts[0]
	parts := strings.SplitN(value, "-", 2)
	core := strings.Split(parts[0], ".")
	if len(core) != 3 {
		return packageVersion{}, false
	}
	for _, value := range core {
		if value == "" || (len(value) > 1 && value[0] == '0') {
			return packageVersion{}, false
		}
	}
	major, e1 := strconv.Atoi(core[0])
	minor, e2 := strconv.Atoi(core[1])
	patch, e3 := strconv.Atoi(core[2])
	if e1 != nil || e2 != nil || e3 != nil {
		return packageVersion{}, false
	}
	result := packageVersion{major: major, minor: minor, patch: patch}
	if len(parts) == 2 {
		result.prerelease = strings.Split(parts[1], ".")
		for _, identifier := range result.prerelease {
			if identifier == "" {
				return packageVersion{}, false
			}
			if _, err := strconv.Atoi(identifier); err == nil && len(identifier) > 1 && identifier[0] == '0' {
				return packageVersion{}, false
			}
		}
	}
	return result, true
}
