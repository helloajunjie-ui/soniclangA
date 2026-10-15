// Package handler — routes for the software-control capability (DESIGN §8
// /api/v1/control/*, §9.1).
//
// The HTTP surface is intentionally thin: it lists windows, inspects a control
// tree and runs one action. Every policy decision lives in the control service,
// so the API cannot become a way around the module switch or the deny list —
// there is no "force" parameter here by design.
package handler

import (
	"errors"
	"net/http"
	"strings"

	"github.com/agenthub/agent-proxy/internal/control"
)

// ---- software control ----

func (h *Handler) control(w http.ResponseWriter, r *http.Request, p string) {
	if h.Control == nil {
		http.Error(w, "software control not available", http.StatusNotFound)
		return
	}
	rest := strings.Trim(strings.TrimPrefix(p, "/api/v1/control"), "/")

	switch {
	case rest == "" || rest == "status":
		h.controlStatus(w, r)
	case rest == "apps":
		h.controlApps(w, r)
	case rest == "run":
		h.controlRun(w, r)
	case rest == "capabilities":
		plat, ok := h.Control.Platform()
		h.json(w, 200, map[string]interface{}{
			"capabilities": h.Control.Capabilities(),
			"platform":     plat,
			"supported":    ok,
			"enabled":      h.Control.IsEnabled(),
		})
	default:
		// /api/v1/control/apps/{id}/inspect
		if strings.HasPrefix(rest, "apps/") && strings.HasSuffix(rest, "/inspect") {
			id := strings.TrimSuffix(strings.TrimPrefix(rest, "apps/"), "/inspect")
			h.controlInspect(w, r, id)
			return
		}
		if rest == "denylist" {
			h.json(w, 200, control.DenyListView())
			return
		}
		http.Error(w, "not found", http.StatusNotFound)
	}
}

func (h *Handler) controlStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	plat, ok := h.Control.Platform()
	h.json(w, 200, map[string]interface{}{
		"enabled":   h.Control.IsEnabled(),
		"platform":  plat,
		"supported": ok,
		// What the capability can do, and which of those actions stay on the
		// console side. Part of the documented API (§8) for any client
		// integrating control; the console itself shows these as buttons rather
		// than as this list.
		"capabilities": h.Control.Capabilities(),
	})
}

func (h *Handler) controlApps(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		apps, err := h.Control.Apps()
		if err != nil {
			h.controlError(w, err)
			return
		}
		h.json(w, 200, apps)
	case http.MethodPost:
		// Explicit refresh is the same read; the POST exists so the UI can
		// trigger a re-scan without the ambiguity of a cache-busting GET.
		apps, err := h.Control.Apps()
		if err != nil {
			h.controlError(w, err)
			return
		}
		h.json(w, 200, apps)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (h *Handler) controlInspect(w http.ResponseWriter, r *http.Request, appID string) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var opts struct {
		MaxDepth         int  `json:"maxDepth"`
		MaxNodes         int  `json:"maxNodes"`
		IncludeOffscreen bool `json:"includeOffscreen"`
	}
	// Optional body: a bare request means "default bounds".
	if _, err := h.decodeOptional(r, &opts); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	snap, err := h.Control.Inspect(r.Context(), control.ActionRequest{
		Cap:              control.CapInspect,
		AppID:            appID,
		MaxDepth:         opts.MaxDepth,
		MaxNodes:         opts.MaxNodes,
		IncludeOffscreen: opts.IncludeOffscreen,
	})
	if err != nil {
		h.controlError(w, err)
		return
	}
	h.json(w, 200, snap)
}

func (h *Handler) controlRun(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req control.ActionRequest
	if err := h.decode(r, &req); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	res, err := h.Control.Run(r.Context(), req)
	if err != nil {
		h.controlError(w, err)
		return
	}
	h.json(w, 200, map[string]interface{}{"result": res})
}

// controlError maps a control error onto an HTTP status.
//
// The distinction that matters to a client is "you asked for something refused
// by policy" (409/403, retrying is pointless) versus "the environment is not
// ready" (503, retrying may work). Returning 400 for everything would teach the
// UI that every refusal is a bug.
func (h *Handler) controlError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, control.ErrDisabled):
		http.Error(w, err.Error(), http.StatusConflict)
	case errors.Is(err, control.ErrBlocked):
		http.Error(w, err.Error(), http.StatusForbidden)
	case errors.Is(err, control.ErrUnsupported):
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
	case errors.Is(err, control.ErrUnknownCapability):
		http.Error(w, err.Error(), http.StatusBadRequest)
	default:
		http.Error(w, err.Error(), http.StatusBadRequest)
	}
}
