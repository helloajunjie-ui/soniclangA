package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The embedded frontend is a build artifact now (see .gitignore), so a binary
// compiled from a fresh clone has no index.html to serve. That state has to
// explain itself: a bare 404 for "/" reads like a broken server, and before the
// dist directory was regenerated wholesale nobody could even tell the
// difference — stale build output kept index.html present for years.
func TestNotBuiltExplainsMissingFrontend(t *testing.T) {
	rec := httptest.NewRecorder()
	notBuilt().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html", ct)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store (a stale explanation is worse than none)", cc)
	}

	body := rec.Body.String()
	// The two commands that actually fix it, and the reassurance that the API
	// is unaffected — otherwise the reader assumes the whole binary is broken.
	for _, want := range []string{"build.ps1", "npm run build", "/api/v1/health"} {
		if !strings.Contains(body, want) {
			t.Errorf("body does not mention %q", want)
		}
	}
}
