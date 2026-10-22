package version

import (
	"runtime/debug"
	"strings"
	"testing"
)

func info(settings map[string]string, deps ...*debug.Module) *debug.BuildInfo {
	bi := &debug.BuildInfo{Deps: deps}
	for k, v := range settings {
		bi.Settings = append(bi.Settings, debug.BuildSetting{Key: k, Value: v})
	}
	return bi
}

func TestTheFactoryVersionNamesTheCommitsItWasBuiltFrom(t *testing.T) {
	bi := info(map[string]string{"vcs.revision": "b8c9716fa8180123456789abcdef0123456789ab", "vcs.modified": "false"},
		&debug.Module{Path: "github.com/ouroboric-dev/gleipnir", Version: "v0.0.0-20261022013122-6b6f83eec777"},
		&debug.Module{Path: "github.com/ouroboric-dev/gungnir", Version: "v0.0.0-20261021195316-32cc95d9d0ff"},
		&debug.Module{Path: "github.com/BurntSushi/toml", Version: "v1.6.0"})
	got := Describe(bi)
	want := "odin b8c9716fa818 gungnir 32cc95d9d0ff gleipnir 6b6f83eec777"
	if got != want {
		t.Errorf("Describe() = %q, want %q", got, want)
	}
}

func TestADirtyOrLocalBuildSaysSoBecauseItCannotBeRepeated(t *testing.T) {
	bi := info(map[string]string{"vcs.revision": "b8c9716fa8180123456789abcdef0123456789ab", "vcs.modified": "true"},
		&debug.Module{Path: "github.com/ouroboric-dev/gungnir", Version: "(devel)"},
		&debug.Module{Path: "github.com/ouroboric-dev/gleipnir", Version: "v0.0.0-20261022013122-6b6f83eec777",
			Replace: &debug.Module{Path: "../gleipnir"}})
	got := Describe(bi)
	for _, want := range []string{"odin b8c9716fa818+dirty", "gungnir local", "gleipnir local"} {
		if !strings.Contains(got, want) {
			t.Errorf("Describe() = %q, want it to contain %q", got, want)
		}
	}
}

func TestABuildWithNoRecordedCommitStillHasAVersionOfBoundedLength(t *testing.T) {
	for name, bi := range map[string]*debug.BuildInfo{"no settings": info(nil), "nil": nil} {
		got := Describe(bi)
		if got != "odin unknown" {
			t.Errorf("%s: Describe() = %q, want %q", name, got, "odin unknown")
		}
	}
	long := info(map[string]string{"vcs.revision": strings.Repeat("a", 500)})
	if got := Describe(long); len(got) > 200 {
		t.Errorf("Describe() is %d bytes, want at most 200 (the ledger schema's bound)", len(got))
	}
}
