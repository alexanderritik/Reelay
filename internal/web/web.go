// Package web serves the dashboard and the embeddable player. Both are
// static pages built into the binary that talk to the /videos API, so a
// self-hosted install needs nothing from the internet.
package web

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed static
var files embed.FS

func Routes(mux *http.ServeMux) {
	static, err := fs.Sub(files, "static")
	if err != nil {
		panic(err) // the embed pattern above guarantees the directory
	}
	assets := http.StripPrefix("/static/", http.FileServerFS(static))
	mux.Handle("GET /static/", noCache(assets))
	mux.Handle("GET /{$}", page(static, "index.html"))
	mux.Handle("GET /embed/{id}", page(static, "embed.html"))
}

// page serves one HTML file whatever the path, for routes that carry their
// state in the URL (the player reads its video id from it).
func page(static fs.FS, name string) http.Handler {
	return noCache(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeFileFS(w, r, static, name)
	}))
}

// Embedded files carry no modification time, so revalidate on every load:
// an upgraded binary must never be paired with a cached old script.
func noCache(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		h.ServeHTTP(w, r)
	})
}
