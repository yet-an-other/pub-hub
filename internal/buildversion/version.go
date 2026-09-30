// Package buildversion reports the release tag or the Go module build identity.
package buildversion

import (
	"runtime/debug"
	"strings"
)

// Release is set by the release workflow for binaries built directly from a tag.
var Release string

func Current() string {
	if Release != "" {
		return Release
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "dev"
	}
	if info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	var revision, modified string
	for _, setting := range info.Settings {
		switch setting.Key {
		case "vcs.revision":
			revision = setting.Value
		case "vcs.modified":
			modified = setting.Value
		}
	}
	if revision == "" {
		return "dev"
	}
	if len(revision) > 7 {
		revision = revision[:7]
	}
	version := "dev+" + revision
	if strings.EqualFold(modified, "true") {
		version += "-dirty"
	}
	return version
}
