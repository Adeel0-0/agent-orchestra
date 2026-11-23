// Package webui embeds the built React UI (web/ builds into dist/ here, see
// web/vite.config.ts) so the Go binary serves the workspace with no files on disk.
package webui

import (
	"embed"
	"io/fs"
)

//go:embed dist
var dist embed.FS

// FS returns the built UI rooted at dist/ (index.html, assets/...).
func FS() fs.FS {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		panic(err) // the embed pattern above guarantees dist exists
	}
	return sub
}
