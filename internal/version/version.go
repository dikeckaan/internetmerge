// Package version exposes the application version. The value is injected at
// build time via -ldflags "-X .../internal/version.Version=v1.2.3"; in dev
// builds it stays "dev".
package version

// Version is the running build's version (e.g. "v0.4.0"). Overridden by ldflags.
var Version = "dev"

// Number returns the version without a leading "v", or "" for dev builds.
func Number() string {
	if Version == "" || Version == "dev" {
		return ""
	}
	if Version[0] == 'v' || Version[0] == 'V' {
		return Version[1:]
	}
	return Version
}
