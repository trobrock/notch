package upgrade

import (
	"fmt"
	"strconv"
	"strings"
)

type semVersion struct {
	major, minor, patch uint64
	prerelease          []string
}

func parseSemVersion(value string) (semVersion, error) {
	original := value
	value = strings.TrimSpace(value)
	value = strings.TrimPrefix(value, "v")
	if build := strings.IndexByte(value, '+'); build >= 0 {
		metadata := strings.Split(value[build+1:], ".")
		if len(metadata) == 0 || metadata[0] == "" {
			return semVersion{}, fmt.Errorf("invalid semantic version %q", original)
		}
		for _, identifier := range metadata {
			if !validPrereleaseIdentifier(identifier) {
				return semVersion{}, fmt.Errorf("invalid semantic version %q", original)
			}
		}
		value = value[:build]
	}
	core := value
	var prerelease []string
	if dash := strings.IndexByte(value, '-'); dash >= 0 {
		core, prerelease = value[:dash], strings.Split(value[dash+1:], ".")
		if len(prerelease) == 0 || prerelease[0] == "" {
			return semVersion{}, fmt.Errorf("invalid semantic version %q", original)
		}
	}
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return semVersion{}, fmt.Errorf("invalid semantic version %q", original)
	}
	numbers := make([]uint64, 3)
	for i, part := range parts {
		if !validNumericIdentifier(part) {
			return semVersion{}, fmt.Errorf("invalid semantic version %q", original)
		}
		n, err := strconv.ParseUint(part, 10, 64)
		if err != nil {
			return semVersion{}, fmt.Errorf("invalid semantic version %q", original)
		}
		numbers[i] = n
	}
	for _, identifier := range prerelease {
		if !validPrereleaseIdentifier(identifier) || (isNumeric(identifier) && len(identifier) > 1 && identifier[0] == '0') {
			return semVersion{}, fmt.Errorf("invalid semantic version %q", original)
		}
	}
	return semVersion{major: numbers[0], minor: numbers[1], patch: numbers[2], prerelease: prerelease}, nil
}

func validNumericIdentifier(value string) bool {
	return value != "" && isNumeric(value) && (len(value) == 1 || value[0] != '0')
}

func isNumeric(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func validPrereleaseIdentifier(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if (r < '0' || r > '9') && (r < 'A' || r > 'Z') && (r < 'a' || r > 'z') && r != '-' {
			return false
		}
	}
	return true
}

func compareSemVersion(left, right semVersion) int {
	for _, pair := range [][2]uint64{{left.major, right.major}, {left.minor, right.minor}, {left.patch, right.patch}} {
		if pair[0] < pair[1] {
			return -1
		}
		if pair[0] > pair[1] {
			return 1
		}
	}
	if len(left.prerelease) == 0 && len(right.prerelease) == 0 {
		return 0
	}
	if len(left.prerelease) == 0 {
		return 1
	}
	if len(right.prerelease) == 0 {
		return -1
	}
	for i := 0; i < len(left.prerelease) && i < len(right.prerelease); i++ {
		leftID, rightID := left.prerelease[i], right.prerelease[i]
		if leftID == rightID {
			continue
		}
		leftNumeric, rightNumeric := isNumeric(leftID), isNumeric(rightID)
		switch {
		case leftNumeric && rightNumeric:
			leftNumber, _ := strconv.ParseUint(leftID, 10, 64)
			rightNumber, _ := strconv.ParseUint(rightID, 10, 64)
			if leftNumber < rightNumber {
				return -1
			}
			return 1
		case leftNumeric:
			return -1
		case rightNumeric:
			return 1
		case leftID < rightID:
			return -1
		default:
			return 1
		}
	}
	if len(left.prerelease) < len(right.prerelease) {
		return -1
	}
	if len(left.prerelease) > len(right.prerelease) {
		return 1
	}
	return 0
}
