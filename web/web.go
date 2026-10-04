// Package web holds the page and its static files, built into the binary.
package web

import (
	"embed"
	"html/template"
	"io/fs"
)

//go:embed index.html static
var files embed.FS

// Page parses the page template.
func Page() (*template.Template, error) {
	return template.ParseFS(files, "index.html")
}

// Static is the files served under /static/.
func Static() fs.FS {
	static, err := fs.Sub(files, "static")
	if err != nil {
		// The directory is embedded above: a missing one is a broken build, not a runtime state.
		panic(err)
	}
	return static
}
