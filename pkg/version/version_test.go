// Copyright 2026 The kine-fdb Authors
// SPDX-License-Identifier: Apache-2.0
//
// Tests for the version string.

package version

import (
	"strings"
	"testing"
)

func TestStringShortensTheCommit(t *testing.T) {
	defer func(v, c string) { Version, Commit = v, c }(Version, Commit)
	Version, Commit = "v1.2.3", "0123456789abcdef0123456789abcdef01234567"

	got := String()
	if !strings.HasPrefix(got, "v1.2.3 (0123456789ab") || strings.Contains(got, "0123456789abc") {
		t.Fatalf("String() = %q, want the version and a 12-character commit", got)
	}
}
