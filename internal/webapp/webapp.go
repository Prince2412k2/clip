// Package webapp serves the static phone-facing UI (drop text/files/images to a
// chosen target). The page's JS talks to the daemon's /v1 API.
package webapp

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed assets
var assets embed.FS

// Handler serves the embedded static webapp at "/".
func Handler() http.Handler {
	sub, err := fs.Sub(assets, "assets")
	if err != nil {
		panic(err) // embed guarantees assets exists at build time
	}
	return http.FileServer(http.FS(sub))
}
