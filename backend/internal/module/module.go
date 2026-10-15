// Package module defines the Module lifecycle interface and the Registry that
// lets every feature (provider/tool/storage/rag/...) plug in with an
// independent enabled switch and health reporting (DESIGN §5 模块化机制).
package module

import (
	"fmt"
	"sort"
	"sync"
)

// Module is the lifecycle contract every plugin implements.
type Module interface {
	ID() string
	Init() error
	Start() error
	Stop() error
}

// Kind groups modules by the plugin slot they occupy.
const (
	KindProvider = "provider"
	KindTool     = "tool"
	KindStorage  = "storage"
	KindRAG      = "rag"
	KindMemory   = "memory"
	KindRuntime  = "runtime"
)

// Meta is the descriptive part of a registration; Info adds live state.
type Meta struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Kind string `json:"kind"`
	// Description is shown to the user verbatim, so it must be plain language:
	// no module ids, no internal component names, no implementation jargon.
	Description string `json:"description,omitempty"`
	Builtin     bool   `json:"builtin"`
	// Hidden keeps a module out of the listing handed to the UI. Internal
	// plumbing (the orchestrator core, the storage backend, the per-provider
	// and per-builtin-tool entries) is still registered — so its lifecycle and
	// its disabled-set entry keep working — but it is not a knob a user should
	// ever see. Exposing it just invites a click that cannot do anything
	// meaningful.
	Hidden bool `json:"-"`
	// ReadOnly renders the entry as information rather than a switch. It is
	// implied for `tool.*` and `provider.*` entries that are surfaced for
	// audit but must not be flipped from the capability page.
	ReadOnly bool `json:"-"`
}

// Info is the public view of a registered module.
type Info struct {
	Meta
	Enabled bool `json:"enabled"`
	// Switchable is false for modules that must always run (the runtime core),
	// so the UI renders them as read-only.
	Switchable bool `json:"switchable"`
}

// Registry holds all registered modules plus their per-module enable switches.
//
// The disabled set is the single source of truth consulted by the orchestrator
// when it assembles tools, so flipping a switch in the UI takes effect on the
// next turn without restarting the process.
type Registry struct {
	mu       sync.RWMutex
	modules  map[string]Module
	meta     map[string]Meta
	disabled map[string]bool
	// listeners are notified after a switch is committed so subsystems whose
	// state is mirrored from the registry stay in sync.
	listeners []func(id string, enabled bool)
}

// NewRegistry constructs an empty registry.
func NewRegistry() *Registry {
	return &Registry{
		modules:  map[string]Module{},
		meta:     map[string]Meta{},
		disabled: map[string]bool{},
	}
}

// Register adds a module with default metadata (id as name, runtime kind).
func (r *Registry) Register(m Module) {
	r.RegisterWithMeta(m, Meta{ID: m.ID(), Name: m.ID(), Kind: KindRuntime})
}

// RegisterWithMeta adds a module with descriptive metadata.
func (r *Registry) RegisterWithMeta(m Module, meta Meta) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if meta.ID == "" {
		meta.ID = m.ID()
	}
	if meta.Name == "" {
		meta.Name = meta.ID
	}
	if meta.Kind == "" {
		meta.Kind = KindRuntime
	}
	r.modules[meta.ID] = m
	r.meta[meta.ID] = meta
}

// Get returns a registered module by id.
func (r *Registry) Get(id string) (Module, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	m, ok := r.modules[id]
	return m, ok
}

// List returns the public info of all modules the UI is allowed to show,
// sorted by kind then name so the UI order is stable across restarts.
//
// Modules marked Hidden are skipped: they are internal plumbing with no
// user-meaningful switch, and listing them would only produce controls that
// appear to work while changing nothing observable.
func (r *Registry) List() []Info {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Info, 0, len(r.modules))
	for id, meta := range r.meta {
		if meta.Hidden {
			continue
		}
		out = append(out, Info{
			Meta:       meta,
			Enabled:    !r.disabled[id],
			Switchable: !meta.ReadOnly,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// IsEnabled reports whether a module is on. Unknown ids are enabled by default
// so a stale switch cannot silently disable a newly registered module.
func (r *Registry) IsEnabled(id string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return !r.disabled[id]
}

// SetEnabled flips a module's switch.
//
// Listeners run after the switch is committed so an observer always sees the
// new state. They are called synchronously and their errors are ignored on
// purpose: a listener is a state mirror (e.g. pushing the switch into the
// control service), and failing the toggle because a mirror hiccupped would
// leave the UI showing one state and the runtime holding another.
func (r *Registry) SetEnabled(id string, enabled bool) error {
	r.mu.RLock()
	_, ok := r.modules[id]
	r.mu.RUnlock()
	if !ok {
		return fmt.Errorf("module %s not found", id)
	}
	r.mu.Lock()
	if enabled {
		delete(r.disabled, id)
	} else {
		r.disabled[id] = true
	}
	listeners := append([]func(string, bool){}, r.listeners...)
	r.mu.Unlock()
	for _, fn := range listeners {
		if fn != nil {
			fn(id, enabled)
		}
	}
	return nil
}

// OnChange registers a listener invoked after every successful SetEnabled.
func (r *Registry) OnChange(fn func(id string, enabled bool)) {
	if fn == nil {
		return
	}
	r.mu.Lock()
	r.listeners = append(r.listeners, fn)
	r.mu.Unlock()
}

// DisabledIDs returns the ids currently switched off (for persistence).
func (r *Registry) DisabledIDs() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.disabled))
	for id := range r.disabled {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// RestoreDisabled re-applies a persisted set of disabled ids, ignoring ids that
// are no longer registered.
func (r *Registry) RestoreDisabled(ids []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, id := range ids {
		if _, ok := r.modules[id]; ok {
			r.disabled[id] = true
		}
	}
}

// EnabledFilter returns a predicate for ids, useful for filtering tool lists.
func (r *Registry) EnabledFilter(ids []string) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if r.IsEnabled(id) {
			out = append(out, id)
		}
	}
	return out
}

// StartAll starts every enabled module in registration order.
func (r *Registry) StartAll() error {
	r.mu.RLock()
	ids := make([]string, 0, len(r.modules))
	for id := range r.modules {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	snapshot := make(map[string]Module, len(r.modules))
	for id, m := range r.modules {
		snapshot[id] = m
	}
	disabled := make(map[string]bool, len(r.disabled))
	for k, v := range r.disabled {
		disabled[k] = v
	}
	r.mu.RUnlock()
	for _, id := range ids {
		if disabled[id] {
			continue
		}
		if err := snapshot[id].Start(); err != nil {
			return err
		}
	}
	return nil
}

// StopAll stops every module (best effort).
func (r *Registry) StopAll() {
	r.mu.RLock()
	mods := make([]Module, 0, len(r.modules))
	for _, m := range r.modules {
		mods = append(mods, m)
	}
	r.mu.RUnlock()
	for _, m := range mods {
		_ = m.Stop()
	}
}

// StartAll invokes Init then Start on every enabled module.
func (r *Registry) InitAll() error {
	r.mu.RLock()
	ids := make([]string, 0, len(r.modules))
	for id := range r.modules {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	snapshot := make(map[string]Module, len(r.modules))
	for id, m := range r.modules {
		snapshot[id] = m
	}
	disabled := make(map[string]bool, len(r.disabled))
	for k, v := range r.disabled {
		disabled[k] = v
	}
	r.mu.RUnlock()
	for _, id := range ids {
		if disabled[id] {
			continue
		}
		if err := snapshot[id].Init(); err != nil {
			return err
		}
	}
	return nil
}
