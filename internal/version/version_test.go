package version

import (
	"strings"
	"testing"
)

func TestString_UsesLinkTimeValues(t *testing.T) {
	defer func(v, c, d string) { Version, Commit, Date = v, c, d }(Version, Commit, Date)
	Version, Commit, Date = "v1.2.3", "abc1234", "2026-10-07T10:00:00Z"

	got := Get().String()
	for _, want := range []string{"costblame v1.2.3", "(abc1234)", "built 2026-10-07T10:00:00Z", "go1", "/"} {
		if !strings.Contains(got, want) {
			t.Errorf("%q is missing %q", got, want)
		}
	}
}

func TestGet_DefaultsToDevAndNeverEmpty(t *testing.T) {
	defer func(v, c, d string) { Version, Commit, Date = v, c, d }(Version, Commit, Date)
	Version, Commit, Date = "dev", "", ""

	info := Get()
	if info.Version != "dev" || info.GoVersion == "" || info.Platform == "" {
		t.Errorf("unexpected info %+v", info)
	}
	if !strings.HasPrefix(info.String(), "costblame dev") {
		t.Errorf("String() = %q", info.String())
	}
}
