package release

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Version is a parsed release version: major.minor.patch plus an optional
// prerelease (the part after '-').
type Version struct {
	Major, Minor, Patch uint64
	Pre                 []string
}

// ParseTag parses a release tag as the release workflow's tag check accepts
// it: vX.Y.Z or vX.Y.Z-pre, where pre is dot separated, non-empty
// identifiers of letters and digits; numbers and numeric identifiers have no
// leading zeros. ParseTag also refuses numbers that overflow 64 bits.
func ParseTag(tag string) (Version, error) {
	if !strings.HasPrefix(tag, "v") {
		return Version{}, fmt.Errorf("release tag %q does not start with v", tag)
	}
	return parse(tag[1:], false)
}

// ParseVersion parses a build's own version: the release form with or
// without the leading v ("0.2.34", "v0.2.35-snapshot"). Semantic version
// prerelease identifiers may hold '-' (Go pseudo versions), and a "+build"
// suffix is ignored. Anything else, such as "dev", is an error.
func ParseVersion(s string) (Version, error) {
	s = strings.TrimPrefix(s, "v")
	if i := strings.IndexByte(s, '+'); i >= 0 {
		build := s[i+1:]
		if build == "" || !validIdents(build, true) {
			return Version{}, fmt.Errorf("version %q: malformed build suffix", s)
		}
		s = s[:i]
	}
	return parse(s, true)
}

func parse(s string, hyphenInPre bool) (Version, error) {
	core, pre, hasPre := strings.Cut(s, "-")
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return Version{}, fmt.Errorf("version %q is not X.Y.Z", s)
	}
	var nums [3]uint64
	for i, p := range parts {
		n, err := parseNum(p)
		if err != nil {
			return Version{}, fmt.Errorf("version %q: %w", s, err)
		}
		nums[i] = n
	}
	v := Version{Major: nums[0], Minor: nums[1], Patch: nums[2]}
	if hasPre {
		if pre == "" || !validIdents(pre, hyphenInPre) {
			return Version{}, fmt.Errorf("version %q: malformed prerelease", s)
		}
		v.Pre = strings.Split(pre, ".")
		for _, id := range v.Pre {
			if isNumeric(id) && len(id) > 1 && id[0] == '0' {
				return Version{}, fmt.Errorf("version %q: prerelease number with a leading zero", s)
			}
		}
	}
	return v, nil
}

func parseNum(p string) (uint64, error) {
	if p == "" || !isNumeric(p) {
		return 0, errors.New("not a number")
	}
	if len(p) > 1 && p[0] == '0' {
		return 0, errors.New("number with a leading zero")
	}
	return strconv.ParseUint(p, 10, 64)
}

func isNumeric(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// validIdents reports whether s is non-empty dot separated identifiers of
// ASCII letters and digits (and '-' when hyphen is true).
func validIdents(s string, hyphen bool) bool {
	for _, id := range strings.Split(s, ".") {
		if id == "" {
			return false
		}
		for _, c := range id {
			switch {
			case c >= '0' && c <= '9', c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z':
			case c == '-' && hyphen:
			default:
				return false
			}
		}
	}
	return true
}

// Compare returns -1, 0 or 1 as v sorts before, equal to or after w, by
// semantic version precedence: a prerelease sorts before its release.
func (v Version) Compare(w Version) int {
	for _, d := range [3][2]uint64{{v.Major, w.Major}, {v.Minor, w.Minor}, {v.Patch, w.Patch}} {
		if d[0] != d[1] {
			if d[0] < d[1] {
				return -1
			}
			return 1
		}
	}
	switch {
	case len(v.Pre) == 0 && len(w.Pre) == 0:
		return 0
	case len(v.Pre) == 0:
		return 1
	case len(w.Pre) == 0:
		return -1
	}
	for i := 0; i < len(v.Pre) && i < len(w.Pre); i++ {
		if c := compareIdent(v.Pre[i], w.Pre[i]); c != 0 {
			return c
		}
	}
	switch {
	case len(v.Pre) < len(w.Pre):
		return -1
	case len(v.Pre) > len(w.Pre):
		return 1
	}
	return 0
}

func compareIdent(a, b string) int {
	an, bn := isNumeric(a), isNumeric(b)
	switch {
	case an && bn:
		// Equal length digit strings without leading zeros compare as text.
		if len(a) != len(b) {
			if len(a) < len(b) {
				return -1
			}
			return 1
		}
		return strings.Compare(a, b)
	case an:
		return -1
	case bn:
		return 1
	}
	return strings.Compare(a, b)
}

// String formats v without the leading v.
func (v Version) String() string {
	s := fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch)
	if len(v.Pre) > 0 {
		s += "-" + strings.Join(v.Pre, ".")
	}
	return s
}
