// Package taskblaster embeds the TaskBlaster demo page so that the server
// can serve it itself, from the same origin as the JMAP endpoints, with no
// proxy in between.
package taskblaster

import (
	_ "embed"
	"net/http"
)

//go:embed index.html
var page []byte

// Handler serves the page, whatever path it is mounted at.
func Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(page)
	})
}
