//go:build !webui

// Package web holds the web UI's production build, embedded in the binary
// when it is built with -tags webui (scripts/build.sh does that after
// building web/dist).
package web

import "io/fs"

// Files returns nil: this binary was built without the UI.
func Files() fs.FS { return nil }
