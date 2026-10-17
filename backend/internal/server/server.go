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
//
// The built frontend is a build artifact, not a source file (see .gitignore):
// a fresh clone has no dist until `npm run build` has run. `//go:embed` cannot
// express "maybe there", so the directory always embeds *something*, and this
// is where the "nothing was built" case is turned into an explanation instead
// of a directory listing or a bare 404.
func StaticHandler() http.Handler {
	sub, err := fs.Sub(DistFS, "dist")
	if err != nil {
		return notBuilt()
	}
	if _, err := fs.Stat(sub, "index.html"); err != nil {
		return notBuilt()
	}
	return http.FileServer(http.FS(sub))
}

// notBuilt answers every request with the reason the UI is missing.
//
// It used to be possible to ship a binary like this by accident: the embed
// directory held 1658 stale files from earlier builds, so `index.html` was
// always present and nobody noticed that the copy step had stopped working.
// Now that the directory is regenerated wholesale, an unbundled binary is a
// normal state and has to say so out loud.
func notBuilt() http.Handler {
	const page = `<!DOCTYPE html>
<html lang="zh-CN"><head><meta charset="utf-8"><title>AgentHub 前端未构建</title>
<style>body{font:14px/1.7 system-ui,"Segoe UI",sans-serif;max-width:44rem;margin:12vh auto;padding:0 1.5rem;color:#222}
code{background:#f3f3f3;padding:.15em .4em;border-radius:4px}
h1{font-size:18px}p{color:#444}</style></head><body>
<h1>这个二进制里没有前端产物</h1>
<p>后端已经跑起来了，但 <code>internal/server/dist</code> 是空的 —— 它是构建产物，
不随源码走。编译前先构建一次前端：</p>
<p><code>powershell -File build.ps1</code>（等价于 <code>cd frontend &amp;&amp; npm run build</code>，再 <code>go build</code>）</p>
<p>API 本身不受影响：<code>/api/v1/health</code>、<code>/v1/models</code> 等接口照常可用。</p>
</body></html>`
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(page))
	})
}

// New builds the HTTP server for the given handler and listen address.
func New(h *handler.Handler, addr string) *http.Server {
	return &http.Server{Addr: addr, Handler: h}
}
