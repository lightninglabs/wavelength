package build

import (
	"fmt"
	"runtime"
	"runtime/debug"
	"strings"
)

const (
	// AppMajor defines the major version of this binary.
	AppMajor uint = 0

	// AppMinor defines the minor version of this binary.
	AppMinor uint = 1

	// AppPatch defines the application patch for this binary.
	AppPatch uint = 0

	// AppPreRelease defines the pre-release version suffix for this
	// binary.
	AppPreRelease = "rc1"
)

var (
	// Commit stores the git commit hash of this build. This should be
	// set using the -X linker flag, e.g.,
	// -ldflags "-X
	// github.com/lightninglabs/darepo-client/build.Commit=<hash>".
	Commit string

	// CommitHash stores the short git commit hash from the build
	// metadata. This is automatically populated by the runtime.
	CommitHash string

	// RawTags stores the raw build tags string from the build metadata.
	// This is automatically populated by the runtime.
	RawTags string

	// GoVersion stores the Go version used to compile the binary. This is
	// automatically populated by the runtime.
	GoVersion string
)

func init() {
	// Populate build info from runtime metadata.
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return
	}

	GoVersion = runtime.Version()

	for _, setting := range info.Settings {
		switch setting.Key {
		case "vcs.revision":
			// Use short commit hash (first 7 chars) like lnd.
			if len(setting.Value) >= 7 {
				CommitHash = setting.Value[:7]
			} else {
				CommitHash = setting.Value
			}

		case "-tags":
			RawTags = setting.Value
		}
	}
}

// Version returns the application version as a properly formed string per the
// semantic versioning 2.0.0 spec (http://semver.org/).
func Version() string {
	// Start with the major, minor, and patch versions.
	version := fmt.Sprintf("%d.%d.%d", AppMajor, AppMinor, AppPatch)

	// Append pre-release version if there is one.
	if AppPreRelease != "" {
		version = fmt.Sprintf("%s-%s", version, AppPreRelease)
	}

	return version
}

// Tags returns the list of build tags that were compiled into the binary.
func Tags() []string {
	if len(RawTags) == 0 {
		return nil
	}

	return strings.Split(RawTags, ",")
}
