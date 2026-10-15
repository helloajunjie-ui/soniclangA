// Package server builds the HTTP server from the embedded frontend and the API
// handler (DESIGN §15).
package server

import (
	"io/fs"
	"net/http"

	"github.com/agenthub/agent-proxy/internal/server/handler"
)

// StaticHandler serves the embedded frontend dist at "/".
//
// No SPA rewrite is needed here: the frontend uses hash routing
// (createWebHashHistory), so every route arrives as "/" plus a fragment and the
// server never sees /settings or /chat/{id} at all. A request for a bare path
// without the fragment is a genuine miss and 404 is the honest answer.
func StaticHandler() http.Handler {
	sub, err := fs.Sub(DistFS, "dist")
	if err != nil {
		return http.NotFoundHandler()
	}
	return http.FileServer(http.FS(sub))
}

// New builds the HTTP server for the given handler and listen address.
func New(h *handler.Handler, addr string) *http.Server {
	return &http.Server{Addr: addr, Handler: h}
}
