// Copyright 2026 The kine-fdb Authors
// SPDX-License-Identifier: Apache-2.0
//
// Build version information, set at link time with -ldflags -X.

// Package version reports the kine-fdb build version.
package version

import (
	"fmt"
	"runtime/debug"
	"strings"
)

var (
	// Version is the release version, for example v0.1.0.
	Version = "dev"
	// Commit is the source revision the binary was built from.
	Commit = ""
)

// String returns the version, the commit, and the kine library version, for
// example "v0.1.0 (0123456789ab, kine v0.17.2)". Missing parts are taken
// from the module build information when available.
func String() string {
	commit := Commit
	kine := ""
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, s := range info.Settings {
			if s.Key == "vcs.revision" && commit == "" && len(s.Value) >= 12 {
				commit = s.Value[:12]
			}
		}
		for _, dep := range info.Deps {
			if dep.Path == "github.com/k3s-io/kine" {
				kine = "kine " + dep.Version
			}
		}
	}
	var details []string
	for _, d := range []string{commit, kine} {
		if d != "" {
			details = append(details, d)
		}
	}
	if len(details) == 0 {
		return Version
	}
	return fmt.Sprintf("%s (%s)", Version, strings.Join(details, ", "))
}
