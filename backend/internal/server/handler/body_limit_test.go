package handler

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/agenthub/agent-proxy/internal/logx"
)

// zeroReader yields an endless stream of 'x' so a test can hand the handler a
// body far larger than the cap without allocating it up front.
type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'x'
	}
	return len(p), nil
}

// decode used to io.ReadAll the body with no limit, so a client that kept writing
// grew the process until it died. The cap turns that into a plain "bad body".
func TestDecodeRejectsOversizedBody(t *testing.T) {
	h := &Handler{Cfg: newAuthHandler("").Cfg, log: logx.Default()}
	req := httptest.NewRequest(http.MethodPost, "/", io.LimitReader(zeroReader{}, maxJSONBody*2))
	req.Header.Set("Content-Type", "application/json")

	var v map[string]interface{}
	if err := h.decode(req, &v); err == nil {
		t.Fatal("a body twice the cap was accepted; the read is unbounded again")
	}
}

// The cap must not reject ordinary payloads.
func TestDecodeAcceptsNormalBody(t *testing.T) {
	h := &Handler{Cfg: newAuthHandler("").Cfg, log: logx.Default()}
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"message":"你好"}`))

	var v map[string]string
	if err := h.decode(req, &v); err != nil {
		t.Fatalf("normal body rejected: %v", err)
	}
	if v["message"] != "你好" {
		t.Errorf("decoded message = %q", v["message"])
	}
}

// decodeOptional must keep its "no body is fine" contract under the cap.
func TestDecodeOptionalStillAllowsEmptyBody(t *testing.T) {
	h := &Handler{Cfg: newAuthHandler("").Cfg, log: logx.Default()}
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(""))
	var v map[string]string
	sent, err := h.decodeOptional(req, &v)
	if err != nil || sent {
		t.Fatalf("empty body: sent=%v err=%v, want false/nil", sent, err)
	}
}
