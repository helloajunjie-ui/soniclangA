package provider

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// With a single key, the attempt that fails benches that key, so the retry loop's
// next selectKey() reports "all keys in cooldown". That message used to be what
// the user saw for every failure — a bad baseUrl, a revoked key, a 500 from the
// vendor all arrived as "keys are cooling down, try again in 60s", which points
// at the wrong thing entirely. The first real error must win.
func TestStreamFailureReportsTheUpstreamCause(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"error":{"message":"upstream is having a bad day"}}`)
	}))
	defer upstream.Close()

	s := newTestService(t)
	if err := s.Create(&Provider{
		ID: "p-stream", Name: "P", Type: "openai", BaseURL: upstream.URL + "/v1",
	}, []string{"sk-key-eeeeeeee"}); err != nil {
		t.Fatal(err)
	}
	rt, err := s.GetRuntime("p-stream")
	if err != nil {
		t.Fatal(err)
	}

	ch, err := rt.ChatStream(context.Background(),
		[]Message{{Role: "user", Content: "hi"}},
		CallOptions{Model: "m"})
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}

	var final StreamChunk
	for chunk := range ch {
		if chunk.Done || chunk.Err != nil {
			final = chunk
		}
	}
	if final.Err == nil {
		t.Fatal("a failing upstream produced no error at all")
	}
	msg := final.Err.Error()
	if strings.Contains(msg, "cooldown") {
		t.Errorf("the cooldown bookkeeping message masked the real cause: %q", msg)
	}
	if !strings.Contains(msg, "500") {
		t.Errorf("error does not mention the upstream status: %q", msg)
	}
}

// The non-streaming path must not regress: a healthy stream still comes through.
func TestStreamStillWorksAfterTheChange(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"model\":\"m\",\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()

	s := newTestService(t)
	if err := s.Create(&Provider{
		ID: "p-ok", Name: "P", Type: "openai", BaseURL: upstream.URL + "/v1",
	}, []string{"sk-key-ffffffff"}); err != nil {
		t.Fatal(err)
	}
	rt, err := s.GetRuntime("p-ok")
	if err != nil {
		t.Fatal(err)
	}
	ch, err := rt.ChatStream(context.Background(),
		[]Message{{Role: "user", Content: "hi"}}, CallOptions{Model: "m"})
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}

	var text string
	var failed error
	for chunk := range ch {
		text += chunk.Delta
		if chunk.Err != nil {
			failed = chunk.Err
		}
	}
	if failed != nil {
		t.Fatalf("healthy stream reported an error: %v", failed)
	}
	if text != "ok" {
		t.Errorf("streamed text = %q, want ok", text)
	}
}
