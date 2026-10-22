// Package version names the build of ygg that wrote a run, so a number quoted later can be traced to the exact
// factory that produced it (spec §10.2).
package version

import (
	"fmt"
	"runtime/debug"
	"strings"
)

// siblings are the modules whose commits belong to the factory's identity, in the order they are printed.
var siblings = []string{"gungnir", "gleipnir", "jormungandr", "havamal"}

const maxLen = 200 // the ledger schema's bound on ygg.factory.version

// Factory is the identity of this build of ygg.
func Factory() string {
	bi, _ := debug.ReadBuildInfo()
	return Describe(bi)
}

// Describe is Factory for a given build info: "odin <12 characters of the commit>", then each sibling module
// with the commit its pseudo-version names. A build with uncommitted changes says +dirty; a sibling that was
// replaced or built from a workspace says local, because that build cannot be repeated from the pins.
func Describe(bi *debug.BuildInfo) string {
	parts := []string{"odin " + commit(bi)}
	if bi != nil {
		for _, name := range siblings {
			for _, d := range bi.Deps {
				if d.Path != "github.com/ouroboric-dev/"+name {
					continue
				}
				parts = append(parts, name+" "+moduleCommit(d))
			}
		}
	}
	out := strings.Join(parts, " ")
	if len(out) > maxLen {
		out = out[:maxLen]
	}
	return out
}

func commit(bi *debug.BuildInfo) string {
	if bi == nil {
		return "unknown"
	}
	var rev, dirty string
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			if s.Value == "true" {
				dirty = "+dirty"
			}
		}
	}
	if rev == "" {
		return "unknown"
	}
	return rev[:min(12, len(rev))] + dirty
}

func moduleCommit(d *debug.Module) string {
	if d.Replace != nil || d.Version == "" || d.Version == "(devel)" {
		return "local"
	}
	// A pseudo-version ends in the commit: v0.0.0-20261021195316-32cc95d9d0ff.
	if i := strings.LastIndex(d.Version, "-"); i >= 0 && len(d.Version)-i-1 == 12 {
		return d.Version[i+1:]
	}
	return fmt.Sprint(d.Version)
}
