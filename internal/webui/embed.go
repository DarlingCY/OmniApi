package webui

import (
	"embed"
	"io/fs"
)

// dist is populated by `npm run web:build`, which writes the compiled Vue
// console into this directory so it ships inside the binary.
//
//go:embed all:dist
var dist embed.FS

// Assets returns the embedded console, or nil when the console has not been
// built yet. A nil result makes the server fall back to its health payload.
func Assets() fs.FS {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		return nil
	}
	if _, err := fs.Stat(sub, "index.html"); err != nil {
		return nil
	}
	return sub
}
