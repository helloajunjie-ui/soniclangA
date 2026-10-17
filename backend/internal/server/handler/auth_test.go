package handler

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/agenthub/agent-proxy/internal/config"
	"github.com/agenthub/agent-proxy/internal/logx"
)

// These tests cover the middleware chain only, so they build a Handler with just
// the config it needs instead of booting every service. /api/v1/settings is the
// probe route: it reads nothing but Cfg, which keeps the assertions about auth
// from depending on the rest of the app.
func newAuthHandler(token string) *Handler {
	return &Handler{
		Cfg: &config.Config{Token: token, Settings: config.Defaults()},
		log: logx.Default(),
	}
}

func do(h *Handler, method, path string, mutate func(*http.Request)) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "http://127.0.0.1:8787"+path, nil)
	r.Host = "127.0.0.1:8787"
	if mutate != nil {
		mutate(r)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

// With no token configured the API is open — that is the loopback-only default,
// and config.checkExposure is what stops it from being reachable elsewhere.
func TestAuthOpenWhenNoToken(t *testing.T) {
	h := newAuthHandler("")
	if rec := do(h, http.MethodGet, "/api/v1/settings", nil); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (no token configured)", rec.Code)
	}
}

// With a token configured every API call must present it, or the local service
// is only as private as its port.
func TestAuthRejectsMissingAndWrongToken(t *testing.T) {
	h := newAuthHandler("s3cret")

	if rec := do(h, http.MethodGet, "/api/v1/settings", nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("no credentials: status = %d, want 401", rec.Code)
	}
	wrong := func(r *http.Request) { r.Header.Set("Authorization", "Bearer nope") }
	if rec := do(h, http.MethodGet, "/api/v1/settings", wrong); rec.Code != http.StatusUnauthorized {
		t.Errorf("wrong bearer: status = %d, want 401", rec.Code)
	}
	// A prefix that merely looks like a bearer header must not be accepted.
	prefix := func(r *http.Request) { r.Header.Set("Authorization", "Bearer") }
	if rec := do(h, http.MethodGet, "/api/v1/settings", prefix); rec.Code != http.StatusUnauthorized {
		t.Errorf("bare 'Bearer': status = %d, want 401", rec.Code)
	}
}

func TestAuthAcceptsBearerAndQueryToken(t *testing.T) {
	h := newAuthHandler("s3cret")

	bearer := func(r *http.Request) { r.Header.Set("Authorization", "Bearer s3cret") }
	if rec := do(h, http.MethodGet, "/api/v1/settings", bearer); rec.Code != http.StatusOK {
		t.Errorf("bearer: status = %d, want 200", rec.Code)
	}

	// The query form exists for clients that cannot set headers (event-source,
	// plain <img>/downloads), so it has to keep working.
	if rec := do(h, http.MethodGet, "/api/v1/settings?token=s3cret", nil); rec.Code != http.StatusOK {
		t.Errorf("query token: status = %d, want 200", rec.Code)
	}
	if rec := do(h, http.MethodGet, "/api/v1/settings?token=wrong", nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("query token wrong: status = %d, want 401", rec.Code)
	}
}

// The health probe answers before the auth gate on purpose: a supervisor or a
// port-forward check needs a liveness answer without being handed the token. It
// reveals only that the process is up.
func TestHealthProbeBypassesAuth(t *testing.T) {
	h := newAuthHandler("s3cret")
	rec := do(h, http.MethodGet, "/api/health", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (health must not require the token)", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "ok") {
		t.Errorf("health body = %q, want it to report ok", rec.Body.String())
	}
}

// A cross-origin write is refused even when it carries the right token: the
// token may leak through history, a shared clipboard or a malicious page reading
// it out of a URL, and the Origin check is the defence that does not depend on
// the secret staying secret.
func TestCrossOriginWriteRejectedEvenWithToken(t *testing.T) {
	h := newAuthHandler("s3cret")
	evil := func(r *http.Request) {
		r.Header.Set("Origin", "https://evil.example")
		r.Header.Set("Authorization", "Bearer s3cret")
	}
	rec := do(h, http.MethodPost, "/api/v1/modules/rag.knowledge/toggle", evil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-origin write with valid token: status = %d, want 403", rec.Code)
	}
}

// Conversely a same-origin preflight must not be answered with 401: browsers
// never attach credentials to the preflight, so requiring the token there would
// break the legitimate UI outright.
func TestPreflightIsAnsweredWithoutCredentials(t *testing.T) {
	h := newAuthHandler("s3cret")
	sameOrigin := func(r *http.Request) {
		r.Header.Set("Origin", "http://127.0.0.1:8787")
		r.Header.Set("Access-Control-Request-Method", "POST")
	}
	rec := do(h, http.MethodOptions, "/api/v1/settings", sameOrigin)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("preflight: status = %d, want 204", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "http://127.0.0.1:8787" {
		t.Errorf("Allow-Origin = %q, want the requesting origin", got)
	}
}
