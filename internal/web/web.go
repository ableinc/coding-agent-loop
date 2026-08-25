// Package web is the browser console for the control API, compiled into the
// binary so an installed daemon — which is only the executable, see
// internal/install — still serves it with no files on disk.
//
// The embed directive cannot reach outside the directory of the file that
// declares it, which is why the assets live under this package rather than
// being embedded from internal/server.
package web

import (
	"embed"
	"io/fs"
)

//go:embed assets
var FS embed.FS

// Assets is FS rooted at the asset directory, so callers see "index.html",
// "app.css", "app.js" directly rather than "assets/index.html".
var Assets = must(fs.Sub(FS, "assets"))

func must(f fs.FS, err error) fs.FS {
	if err != nil {
		panic(err)
	}
	return f
}
