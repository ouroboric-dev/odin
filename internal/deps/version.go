// Package deps bumps one dependency across the products of a project (spec §9.6): the canary product
// first, then the rest, one at a time. A bump that fails a product's ladder checks is undone in that
// product, byte for byte, and goes no further.
package deps

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

var versionRE = regexp.MustCompile(`^v?(0|[1-9][0-9]{0,8})\.(0|[1-9][0-9]{0,8})\.(0|[1-9][0-9]{0,8})(?:-([0-9A-Za-z][0-9A-Za-z.-]*))?(?:\+[0-9A-Za-z.-]+)?$`)

// Version is a semantic version, such as 1.4.2 or 1.4.2-rc.1. Build metadata is dropped.
type Version struct {
	Major, Minor, Patch int
	Pre                 string
}

// ParseVersion reads x.y.z, with an optional leading v, a pre-release and build metadata.
func ParseVersion(s string) (Version, error) {
	m := versionRE.FindStringSubmatch(s)
	if m == nil {
		return Version{}, fmt.Errorf("%q is not a version of the form x.y.z", s)
	}
	n := func(i int) int { v, _ := strconv.Atoi(m[i]); return v }
	return Version{Major: n(1), Minor: n(2), Patch: n(3), Pre: m[4]}, nil
}

func (v Version) String() string {
	s := fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch)
	if v.Pre != "" {
		s += "-" + v.Pre
	}
	return s
}

// Compare returns -1, 0 or 1 as v is before, equal to or after o. A pre-release is before its release.
func (v Version) Compare(o Version) int {
	for _, p := range [][2]int{{v.Major, o.Major}, {v.Minor, o.Minor}, {v.Patch, o.Patch}} {
		if c := cmpInt(p[0], p[1]); c != 0 {
			return c
		}
	}
	switch {
	case v.Pre == o.Pre:
		return 0
	case v.Pre == "":
		return 1
	case o.Pre == "":
		return -1
	}
	a, b := strings.Split(v.Pre, "."), strings.Split(o.Pre, ".")
	for i := 0; i < len(a) && i < len(b); i++ {
		if c := cmpIdent(a[i], b[i]); c != 0 {
			return c
		}
	}
	return cmpInt(len(a), len(b))
}

// cmpIdent orders two pre-release identifiers: numbers by value and before words, words as text.
func cmpIdent(a, b string) int {
	x, ea := strconv.Atoi(a)
	y, eb := strconv.Atoi(b)
	switch {
	case ea == nil && eb == nil:
		return cmpInt(x, y)
	case ea == nil:
		return -1
	case eb == nil:
		return 1
	}
	return strings.Compare(a, b)
}

func cmpInt(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

var packageRE = regexp.MustCompile(`^(@[a-z0-9][a-z0-9._~-]*/)?[A-Za-z0-9][A-Za-z0-9._~/-]*$`)

// ValidPackage reports whether name can be a Go module path or an npm package name. It refuses a name
// that starts with "-" (an option for the tool), and "..".
func ValidPackage(name string) bool {
	return packageRE.MatchString(name) && !strings.Contains(name, "..") && len(name) <= 214
}
