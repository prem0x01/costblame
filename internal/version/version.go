// Package version holds the build's identity. The values are set at link time
// by the release workflow and the Dockerfile:
//
//	go build -ldflags "-X github.com/prem0x01/costblame/internal/version.Version=v1.2.3 ..."
//
// A plain `go build` leaves the defaults, and Get then falls back to the VCS
// information Go embeds in the binary, so a local build still says which commit
// it came from.
package version

import (
	"fmt"
	"runtime"
	"runtime/debug"
)

// Set at link time.
var (
	Version = "dev"
	Commit  = ""
	Date    = ""
)

// Info describes a build.
type Info struct {
	Version   string
	Commit    string
	Date      string
	GoVersion string
	Platform  string
}

// Get returns the build's identity, filling any gaps from the VCS stamp that
// `go build` embeds (commit and a "modified" marker for a dirty tree).
func Get() Info {
	info := Info{Version: Version, Commit: Commit, Date: Date, GoVersion: runtime.Version(), Platform: runtime.GOOS + "/" + runtime.GOARCH}
	if bi, ok := debug.ReadBuildInfo(); ok {
		dirty := false
		for _, s := range bi.Settings {
			switch s.Key {
			case "vcs.revision":
				if info.Commit == "" {
					info.Commit = s.Value
				}
			case "vcs.time":
				if info.Date == "" {
					info.Date = s.Value
				}
			case "vcs.modified":
				dirty = s.Value == "true"
			}
		}
		if dirty && info.Commit != "" && Commit == "" {
			info.Commit += "-dirty"
		}
	}
	if len(info.Commit) > 12 && Commit == "" {
		info.Commit = info.Commit[:12] + suffix(info.Commit)
	}
	return info
}

func suffix(commit string) string {
	if len(commit) > 6 && commit[len(commit)-6:] == "-dirty" {
		return "-dirty"
	}
	return ""
}

// String is the one-line form printed by `costblame version`.
func (i Info) String() string {
	s := i.Version
	if i.Commit != "" {
		s += " (" + i.Commit + ")"
	}
	if i.Date != "" {
		s += " built " + i.Date
	}
	return fmt.Sprintf("costblame %s %s %s", s, i.GoVersion, i.Platform)
}
