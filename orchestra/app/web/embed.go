// Package webassets embeds the built React UI (web/ builds into dist/ here, see
// web/vite.config.ts) so the Go binary serves the window with no files on disk.
//
// It lives beside the Python package's copy of the build so the UI build and the
// Python launcher stay unchanged during the migration. When the Python backend is
// removed, move dist/ (and vite's outDir) next to the Go code instead.
package webassets

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
