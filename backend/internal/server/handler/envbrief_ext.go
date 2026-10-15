// Package handler — routes for the two pieces of the "one sentence and we
// start" flow that are not the desktop console itself (DESIGN §9.3):
//
//   - /api/v1/env/probe   what this machine can do the requested job with;
//   - /api/v1/briefs      the notes the user and the agent write while waiting.
//
// Both are thin. The probe is a pure read of the local machine and the brief
// store owns its own files, so there is no policy to enforce here.
package handler

import (
	"errors"
	"net/http"
	"strings"

	"github.com/agenthub/agent-proxy/internal/brief"
	"github.com/agenthub/agent-proxy/internal/envprobe"
)

// ---- environment probe ----

func (h *Handler) env(w http.ResponseWriter, r *http.Request, p string) {
	rest := strings.Trim(strings.TrimPrefix(p, "/api/v1/env"), "/")
	switch {
	case rest == "probe":
		if r.Method != http.MethodGet && r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		q := r.URL.Query().Get("q")
		if q == "" {
			var body struct {
				Query string `json:"query"`
			}
			_, _ = h.decodeOptional(r, &body)
			q = body.Query
		}
		if strings.TrimSpace(q) == "" {
			http.Error(w, "缺少 q：要探测什么需求", http.StatusBadRequest)
			return
		}
		h.json(w, 200, envprobe.Probe(q))
	default:
		http.Error(w, "not found", http.StatusNotFound)
	}
}

// ---- briefs ----

func (h *Handler) briefs(w http.ResponseWriter, r *http.Request, p string) {
	if h.Briefs == nil || !h.Briefs.Enabled() {
		http.Error(w, "简报存储未启用", http.StatusNotFound)
		return
	}
	rest := strings.Trim(strings.TrimPrefix(p, "/api/v1/briefs"), "/")
	if rest == "" {
		h.briefCollection(w, r)
		return
	}
	h.briefItem(w, r, rest)
}

func (h *Handler) briefCollection(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		list, err := h.Briefs.List()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		h.json(w, 200, list)
	case http.MethodPost:
		var body struct {
			Title string `json:"title"`
		}
		if err := h.decode(r, &body); err != nil {
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
		b, err := h.Briefs.Create(body.Title)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		h.json(w, 200, b)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (h *Handler) briefItem(w http.ResponseWriter, r *http.Request, id string) {
	switch r.Method {
	case http.MethodGet:
		b, err := h.Briefs.Get(id)
		if h.briefErr(w, err) {
			return
		}
		h.json(w, 200, b)
	case http.MethodPut:
		// Replace: used when the note is rewritten wholesale from the console.
		var body struct {
			Content string `json:"content"`
		}
		if err := h.decode(r, &body); err != nil {
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
		b, err := h.Briefs.Write(id, body.Content)
		if h.briefErr(w, err) {
			return
		}
		h.json(w, 200, b)
	case http.MethodPost:
		// Append: the normal path while ideas are still coming in. Keeping the
		// two apart means a caller cannot lose earlier notes by sending only
		// the newest thought.
		var body struct {
			Text string `json:"text"`
		}
		if err := h.decode(r, &body); err != nil {
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
		b, err := h.Briefs.Append(id, body.Text)
		if h.briefErr(w, err) {
			return
		}
		h.json(w, 200, b)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (h *Handler) briefErr(w http.ResponseWriter, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, brief.ErrNotFound):
		http.Error(w, err.Error(), http.StatusNotFound)
	case errors.Is(err, brief.ErrBadID):
		http.Error(w, err.Error(), http.StatusBadRequest)
	default:
		http.Error(w, err.Error(), http.StatusBadRequest)
	}
	return true
}
