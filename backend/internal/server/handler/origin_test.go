package handler

import (
	"net/http/httptest"
	"testing"
)

// The API listens on loopback, but "local" is not a security boundary: the
// browser of the user at the machine is local too. Before this check, the
// handler answered Access-Control-Allow-Origin: * and accepted any Origin, so
// any page the user had open could read the agent's history and drive it —
// including switching on the desktop-control module and then operating the
// desktop. These cases pin the boundary.
func TestOriginAllowed(t *testing.T) {
	cases := []struct {
		name    string
		origin  string
		host    string
		allowed bool
	}{
		{"no origin (curl, native client)", "", "127.0.0.1:8787", true},
		{"same origin", "http://127.0.0.1:8787", "127.0.0.1:8787", true},
		{"same origin over a LAN address", "http://192.168.1.9:8787", "192.168.1.9:8787", true},
		{"dev server on its own port", "http://localhost:5273", "127.0.0.1:8787", true},
		{"dev server via loopback ip", "http://127.0.0.1:5273", "127.0.0.1:8787", true},
		{"ipv6 loopback dev server", "http://[::1]:5273", "127.0.0.1:8787", true},

		{"an arbitrary website", "https://evil.example", "127.0.0.1:8787", false},
		{"a website that mimics loopback in its path", "https://evil.example/127.0.0.1:8787", "127.0.0.1:8787", false},
		{"a host that merely starts with localhost", "http://localhost.evil.example", "127.0.0.1:8787", false},
		{"a lookalike loopback ip", "http://127.0.0.2.evil.example", "127.0.0.1:8787", false},
		{"a private address that is not the host", "http://10.0.0.5:8787", "127.0.0.1:8787", false},
		{"a sandboxed document", "null", "127.0.0.1:8787", false},
		{"a malformed origin", "://bad", "127.0.0.1:8787", false},
		// A public IP is not loopback even though it parses.
		{"a public ip", "http://93.184.216.34:8787", "127.0.0.1:8787", false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", "http://"+c.host+"/api/v1/modules/x/toggle", nil)
			r.Host = c.host
			if c.origin != "" {
				r.Header.Set("Origin", c.origin)
			}
			if got := originAllowed(r); got != c.allowed {
				t.Errorf("originAllowed(origin=%q, host=%q) = %v, want %v",
					c.origin, c.host, got, c.allowed)
			}
		})
	}
}

// The check must cover every surface that can change state or leak data, and
// must not be limited to the control endpoints.
func TestIsAPIPathCoversTheWriteSurfaces(t *testing.T) {
	yes := []string{
		"/api/v1/control/run", "/api/v1/modules/x/toggle", "/api/v1/conversations",
		"/api/v1/memories", "/api/v1/providers", "/api/v1/scripts/x",
		"/v1/chat/completions", "/v1/models",
	}
	for _, p := range yes {
		if !isAPIPath(p) {
			t.Errorf("%s is not treated as an API path", p)
		}
	}
	// Static assets are served from the embedded UI and are not state.
	if isAPIPath("/index.html") || isAPIPath("/assets/app.js") {
		t.Error("a static path was treated as an API path")
	}
}
