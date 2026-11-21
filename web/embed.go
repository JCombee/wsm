// Package web embeds the built dashboard frontend (web/dist, made by
// "make web"). Without a build, dist holds only .gitkeep and the dashboard
// answers with a hint to run make web.
package web

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var dist embed.FS

// Dist returns the built frontend.
func Dist() fs.FS {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		panic(err)
	}
	return sub
}
