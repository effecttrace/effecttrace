// Package version carries build information set with -ldflags.
package version

import "runtime/debug"

// Set at build time:
//
//	-X github.com/effecttrace/effecttrace/internal/version.Version=v0.1.0
//	-X github.com/effecttrace/effecttrace/internal/version.Commit=<sha>
var (
	Version = "dev"
	Commit  = ""
)

// String returns "version (commit)".
func String() string {
	c := Commit
	if c == "" {
		if bi, ok := debug.ReadBuildInfo(); ok {
			for _, s := range bi.Settings {
				if s.Key == "vcs.revision" {
					c = s.Value
				}
			}
		}
	}
	if len(c) > 12 {
		c = c[:12]
	}
	if c == "" {
		return Version
	}
	return Version + " (" + c + ")"
}
