package main

import (
	"runtime"
	"runtime/debug"
	"strings"
)

// version is the release this binary claims to be.
//
// A var rather than a const so a release build can stamp it:
//
//	go build -trimpath -ldflags="-s -w -X main.version=$(git describe --tags)"
//
// The default is the version under development, so a plain `go build` is
// honest about what it is rather than reporting whatever the last release was.
var version = "0.1.0"

// versionString reports the release, and where it came from.
//
// The commit is read from the build information Go embeds automatically, so a
// binary built without any ldflags still says exactly which tree produced it.
// That matters more than the tag: "0.1.0" is a claim, the revision is a fact.
func versionString() string {
	parts := []string{version}

	if info, ok := debug.ReadBuildInfo(); ok {
		var revision, modified string
		for _, s := range info.Settings {
			switch s.Key {
			case "vcs.revision":
				revision = s.Value
			case "vcs.modified":
				modified = s.Value
			}
		}
		if revision != "" {
			short := revision
			if len(short) > 7 {
				short = short[:7]
			}
			// A build from a dirty tree is not the commit it names, and saying so
			// is the difference between a useful bug report and a confusing one.
			if modified == "true" {
				short += "-dirty"
			}
			parts = append(parts, short)
		}
	}

	parts = append(parts, runtime.Version(), runtime.GOOS+"/"+runtime.GOARCH)
	return "shcr " + parts[0] + " (" + strings.Join(parts[1:], ", ") + ")"
}
