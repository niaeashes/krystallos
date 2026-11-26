//go:build webui

// Package web holds the web UI's production build, embedded in the binary
// when it is built with -tags webui (scripts/build.sh does that after
// building web/dist).
package web

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var dist embed.FS

// Files returns the built UI, with index.html at its root.
func Files() fs.FS {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		panic(err)
	}
	return sub
}
