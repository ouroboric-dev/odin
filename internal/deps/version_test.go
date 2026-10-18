package deps

import "testing"

func TestVersionOrder(t *testing.T) {
	order := []string{"0.9.9", "1.0.0-alpha", "1.0.0-alpha.1", "1.0.0-alpha.beta", "1.0.0-beta.2", "1.0.0-beta.11", "1.0.0", "1.0.1", "1.2.0", "2.0.0"}
	for i, a := range order {
		for j, b := range order {
			va, err := ParseVersion(a)
			vb, err2 := ParseVersion(b)
			if err != nil || err2 != nil {
				t.Fatal(err, err2)
			}
			want := cmpInt(i, j)
			if got := va.Compare(vb); got != want {
				t.Errorf("Compare(%s, %s) = %d, want %d", a, b, got, want)
			}
		}
	}
}

func TestParseVersionReadsGoAndNpmForms(t *testing.T) {
	for in, want := range map[string]string{"v1.2.3": "1.2.3", "1.2.3": "1.2.3", "v0.42.0": "0.42.0", "1.2.3-rc.1+build.5": "1.2.3-rc.1"} {
		if v, err := ParseVersion(in); err != nil || v.String() != want {
			t.Errorf("ParseVersion(%q) = %v, %v; want %s", in, v, err, want)
		}
	}
	for _, bad := range []string{"", "1.2", "latest", "^1.2.3", "v01.2.3", "1.2.3.4", "1.2.3-", "1.2.x"} {
		if _, err := ParseVersion(bad); err == nil {
			t.Errorf("ParseVersion(%q) passed", bad)
		}
	}
}

func TestValidPackageRefusesAnOptionAndAPathEscape(t *testing.T) {
	for _, ok := range []string{"golang.org/x/text", "vite", "@tanstack/react-query", "github.com/Foo/bar"} {
		if !ValidPackage(ok) {
			t.Errorf("%q refused", ok)
		}
	}
	for _, bad := range []string{"", "-g", "--registry=http://x", "a/../b", "a b", "a;b", "$(x)", "@/x"} {
		if ValidPackage(bad) {
			t.Errorf("%q passed", bad)
		}
	}
}
