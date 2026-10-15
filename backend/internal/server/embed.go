// Package server wires the HTTP API, SSE chat stream, OpenAI-compatible
// endpoints and the embedded static frontend (M2 placeholder; replaced by the
// Vue build in M5).
package server

import "embed"

// DistFS holds the built frontend (or placeholder) served at "/".
//
//go:embed all:dist
var DistFS embed.FS
