// Package version reports the cproxy build version.
package version

import (
	"fmt"
	"runtime"
	"runtime/debug"
	"strings"
)

// Version is overridden at build time via -ldflags.
var Version = "dev"

// String returns the version, preferring Go build metadata when unset.
func String() string {
	v := Version
	if v == "dev" {
		if bi, ok := debug.ReadBuildInfo(); ok {
			for _, s := range bi.Settings {
				if s.Key == "vcs.revision" && len(s.Value) >= 7 {
					return "dev+" + s.Value[:7]
				}
			}
			if bi.Main.Version != "" && bi.Main.Version != "(devel)" {
				return bi.Main.Version
			}
		}
	}
	return v
}

// Full returns a one-line version banner.
func Full() string {
	return fmt.Sprintf("cproxy %s (%s/%s)", strings.TrimSpace(String()), runtime.GOOS, runtime.GOARCH)
}
