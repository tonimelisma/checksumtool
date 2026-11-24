package main

import "runtime/debug"

// version is stamped by the release workflow with
// -ldflags "-X main.version=vX.Y.Z". When it is empty, the module version Go
// records in the build info is used instead: a `go build` of a clean checkout
// at a tag, or `go install ...@vX.Y.Z`, reports that tag.
var version string

func versionString() string {
	if version != "" {
		return version
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" {
		return info.Main.Version
	}
	return "(devel)"
}
