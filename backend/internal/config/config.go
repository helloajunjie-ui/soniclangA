// Package config loads runtime configuration (data directory, listen address,
// auth token, dev mode) and the global settings.json document, and exposes
// typed accessors plus a reachability audit hook.
package config

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"sync"

	"github.com/agenthub/agent-proxy/internal/storage"
)

// FailoverSettings controls disaster-recovery behaviour (see DESIGN §6.6/§14).
type FailoverSettings struct {
	Enabled                bool `json:"enabled"`
	CooldownSeconds        int  `json:"cooldownSeconds"`
	AllowCrossProviderAuto bool `json:"allowCrossProviderAuto"`
}

// AutoRouteSettings controls intent routing in mode=auto (see DESIGN §6.8/§14).
type AutoRouteSettings struct {
	Enabled bool `json:"enabled"`
	// Strategy selects how a message is classified: "rule" (keyword screen,
	// free) or "model" (ask the LLM, costs one call). Any other value is
	// normalised to "rule" by mergeDefaults so a stale or hand-edited
	// settings.json cannot leave the UI showing a blank picker.
	Strategy  string  `json:"strategy"`
	Threshold float64 `json:"threshold"`
}

// Settings is the global settings.json document (DESIGN §7).
type Settings struct {
	Failover  FailoverSettings  `json:"failover"`
	AutoRoute AutoRouteSettings `json:"autoRoute"`
	// MemoryAutoExtract gates the background extraction that turns finished
	// turns into long-term memories. It is deliberately *not* offered as a
	// switch in the UI (DESIGN §17 原则五): the honest answer to "记不记" is
	// always yes, so asking would only invite the user to switch off the
	// product's core promise. The field stays readable/writable for
	// hand-edited settings.json, tests and future privacy controls.
	MemoryAutoExtract bool   `json:"memoryAutoExtract"`
	DefaultRoleId     string `json:"defaultRoleId"`
	DefaultProviderId string `json:"defaultProviderId"`
	// DefaultModel is the model used when neither the role nor the request
	// specifies one. Keeps "service provider configured = usable" true for a
	// zero-config user: every role falls back to the default provider's model.
	DefaultModel string `json:"defaultModel"`
	Theme        string `json:"theme"`
	// StorageDriver selects the persistence backend: "sqlite" (default,
	// modernc.org/sqlite, no CGO) or "json" (flat files). See DESIGN §7.1.
	StorageDriver string `json:"storageDriver"`
	// DisabledModules lists module ids switched off in the UI (DESIGN §5
	// 独立开关). Empty means everything is on.
	DisabledModules []string `json:"disabledModules,omitempty"`
}

// Config is the root runtime configuration object.
type Config struct {
	DataDir  string
	Addr     string
	Token    string
	Dev      bool
	Settings Settings

	mu           sync.RWMutex
	settingsPath string
	// settingsStore persists the settings document inside the same database as
	// every other namespace. settings.json is only the one-time migration source
	// for a pre-database install; after the first successful save it is renamed
	// to settings.json.bak so there is never a second source of truth.
	settingsStore storage.Store
	// overrides records flags the user passed explicitly. loadSettings() must
	// re-apply them: a missing settings.json is seeded from Defaults(), which
	// would otherwise silently discard the CLI value (a "dead" flag).
	overrides map[string]string
}

// Defaults returns the default settings (DESIGN §14 待确认 defaults).
func Defaults() Settings {
	return Settings{
		// Failover ships on: retrying on the same provider's next key is a
		// basic gateway duty rather than a preference, and the user has no
		// control left to switch it on with. The cross-provider half stays off
		// — that one sends the user's content to a different vendor, is not
		// implemented yet, and must never default to on (DESIGN §6.6).
		Failover:          FailoverSettings{Enabled: true, CooldownSeconds: 60, AllowCrossProviderAuto: false},
		AutoRoute:         AutoRouteSettings{Enabled: true, Strategy: "rule", Threshold: 0.6},
		MemoryAutoExtract: true,
		DefaultRoleId:     "general",
		DefaultProviderId: "",
		DefaultModel:      "",
		Theme:             "system",
		StorageDriver:     storage.DriverSQLite,
	}
}

// Parse builds a Config from CLI flags and environment overrides.
func Parse() (*Config, error) {
	c := &Config{Settings: Defaults()}
	fs := flag.NewFlagSet("agenthub", flag.ContinueOnError)
	fs.StringVar(&c.DataDir, "data", "./data", "data directory (json store + vault)")
	fs.StringVar(&c.Addr, "addr", "127.0.0.1:8787", "listen address")
	fs.StringVar(&c.Token, "token", "", "auth token for non-localhost / shared access (optional)")
	fs.BoolVar(&c.Dev, "dev", false, "serve frontend from disk instead of embedded dist")
	fs.StringVar(&c.Settings.StorageDriver, "storage", c.Settings.StorageDriver,
		"persistence backend: sqlite (default, no CGO) or json (flat files)")
	if err := fs.Parse(os.Args[1:]); err != nil {
		return nil, err
	}
	// remember explicitly-set flags so settings loading cannot discard them
	c.overrides = map[string]string{}
	fs.Visit(func(f *flag.Flag) { c.overrides[f.Name] = f.Value.String() })
	if v := os.Getenv("AGENTHUB_DATA"); v != "" {
		c.DataDir = v
	}
	if v := os.Getenv("AGENTHUB_ADDR"); v != "" {
		c.Addr = v
	}
	if v := os.Getenv("AGENTHUB_TOKEN"); v != "" {
		c.Token = v
	}
	return c, nil
}

// Init creates the data directory and loads the settings document. Settings are
// persisted through the same Store (sqlite by default) as every other module —
// a single database, no stray settings.json to chase. The json file survives
// only as the one-time migration source for an older install.
func (c *Config) Init() error {
	if err := os.MkdirAll(c.DataDir, 0o755); err != nil {
		return err
	}
	c.settingsPath = filepath.Join(c.DataDir, "settings.json")
	// Open the settings namespace with the same driver as the rest of the app
	// (flag or default sqlite). A fallback to the json driver inside Open is fine
	// here; only a hard failure is fatal.
	st, _ := storage.Open(c.DataDir, "settings", c.SubDir("settings"), c.Settings.StorageDriver)
	c.settingsStore = st
	return c.loadSettings()
}

func (c *Config) loadSettings() error {
	// Primary source: the settings document inside the store.
	var s Settings
	if err := c.settingsStore.Load("settings", &s); err == nil {
		c.Settings = mergeDefaults(s)
		c.applyOverrides()
		return nil
	}
	// Legacy source: settings.json from a pre-database install. Import it once,
	// persist it into the store, then decommission the file so the database
	// becomes the single source of truth.
	if data, err := os.ReadFile(c.settingsPath); err == nil && len(data) > 0 {
		if err := json.Unmarshal(data, &s); err == nil {
			c.Settings = mergeDefaults(s)
			c.applyOverrides()
			if err := c.SaveSettings(); err == nil {
				_ = os.Rename(c.settingsPath, c.settingsPath+".bak")
			}
			return nil
		}
	}
	// Nothing anywhere: seed from defaults.
	c.Settings = Defaults()
	c.applyOverrides()
	return c.SaveSettings()
}

// applyOverrides re-applies explicitly-passed CLI flags on top of the loaded
// settings document, so a flag is never silently dead (DESIGN §5 config.Audit).
func (c *Config) applyOverrides() {
	if v, ok := c.overrides["storage"]; ok && v != "" {
		c.Settings.StorageDriver = v
	}
}

func mergeDefaults(s Settings) Settings {
	d := Defaults()
	if s.Failover.CooldownSeconds == 0 {
		s.Failover.CooldownSeconds = d.Failover.CooldownSeconds
	}
	if s.AutoRoute.Strategy == "" {
		s.AutoRoute.Strategy = d.AutoRoute.Strategy
	}
	if s.AutoRoute.Threshold == 0 {
		s.AutoRoute.Threshold = d.AutoRoute.Threshold
	}
	if s.DefaultRoleId == "" {
		s.DefaultRoleId = d.DefaultRoleId
	}
	if s.Theme == "" {
		s.Theme = d.Theme
	}
	// Normalise the routing strategy the same way storageDriver is normalised:
	// an unknown value ("hybrid" from an older build, or a hand-edited typo)
	// must not survive into the UI, where it renders as an empty picker.
	switch s.AutoRoute.Strategy {
	case "rule", "model":
	default:
		s.AutoRoute.Strategy = d.AutoRoute.Strategy
	}
	switch s.StorageDriver {
	case storage.DriverJSON, storage.DriverSQLite:
	default:
		s.StorageDriver = d.StorageDriver
	}
	return s
}

// MergeSettings fills zero-valued fields of a settings document from Defaults.
// Kept for callers that hold a whole document; HTTP callers should use
// SettingsPatch, which can tell "absent" from "false".
//
// Note what this cannot do: a bool has no zero-value way to say "not supplied",
// so merging a partial document always resets every boolean to false.
func MergeSettings(s Settings) Settings { return mergeDefaults(s) }

// SettingsPatch is the wire shape of a settings PUT. Every field is a pointer
// so that "absent" is distinguishable from false / 0 / "".
//
// Without that distinction a partial PUT — say {"defaultRoleId":"general"} —
// decodes the missing booleans as false and silently switches off failover,
// auto-routing and memory extraction. All three fail quietly: nothing errors,
// the feature just stops happening, and two of them have no UI control left to
// notice them with.
type SettingsPatch struct {
	Failover          *FailoverPatch  `json:"failover"`
	AutoRoute         *AutoRoutePatch `json:"autoRoute"`
	MemoryAutoExtract *bool           `json:"memoryAutoExtract"`
	DefaultRoleId     *string         `json:"defaultRoleId"`
	DefaultProviderId *string         `json:"defaultProviderId"`
	DefaultModel      *string         `json:"defaultModel"`
	Theme             *string         `json:"theme"`
	StorageDriver     *string         `json:"storageDriver"`
	DisabledModules   *[]string       `json:"disabledModules"`
}

// FailoverPatch is a partial FailoverSettings.
type FailoverPatch struct {
	Enabled                *bool `json:"enabled"`
	CooldownSeconds        *int  `json:"cooldownSeconds"`
	AllowCrossProviderAuto *bool `json:"allowCrossProviderAuto"`
}

// AutoRoutePatch is a partial AutoRouteSettings.
type AutoRoutePatch struct {
	Enabled   *bool    `json:"enabled"`
	Strategy  *string  `json:"strategy"`
	Threshold *float64 `json:"threshold"`
}

// Apply overlays the supplied fields onto cur and re-normalises the result, so
// anything the caller did not mention keeps its current value rather than
// falling back to nothing.
func (p SettingsPatch) Apply(cur Settings) Settings {
	if p.Failover != nil {
		if p.Failover.Enabled != nil {
			cur.Failover.Enabled = *p.Failover.Enabled
		}
		if p.Failover.CooldownSeconds != nil {
			cur.Failover.CooldownSeconds = *p.Failover.CooldownSeconds
		}
		if p.Failover.AllowCrossProviderAuto != nil {
			cur.Failover.AllowCrossProviderAuto = *p.Failover.AllowCrossProviderAuto
		}
	}
	if p.AutoRoute != nil {
		if p.AutoRoute.Enabled != nil {
			cur.AutoRoute.Enabled = *p.AutoRoute.Enabled
		}
		if p.AutoRoute.Strategy != nil {
			cur.AutoRoute.Strategy = *p.AutoRoute.Strategy
		}
		if p.AutoRoute.Threshold != nil {
			cur.AutoRoute.Threshold = *p.AutoRoute.Threshold
		}
	}
	if p.MemoryAutoExtract != nil {
		cur.MemoryAutoExtract = *p.MemoryAutoExtract
	}
	if p.DefaultRoleId != nil {
		cur.DefaultRoleId = *p.DefaultRoleId
	}
	if p.DefaultProviderId != nil {
		cur.DefaultProviderId = *p.DefaultProviderId
	}
	if p.DefaultModel != nil {
		cur.DefaultModel = *p.DefaultModel
	}
	if p.Theme != nil {
		cur.Theme = *p.Theme
	}
	if p.StorageDriver != nil {
		cur.StorageDriver = *p.StorageDriver
	}
	if p.DisabledModules != nil {
		cur.DisabledModules = *p.DisabledModules
	}
	return mergeDefaults(cur)
}

// GetSettings returns a snapshot of the current settings. Every read goes
// through here so a concurrent PUT cannot hand out a half-updated struct.
func (c *Config) GetSettings() Settings {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.Settings
}

// UpdateSettings applies mutate to the settings under lock and persists them.
// The mutate function runs while the lock is held, so it must not call back
// into Config.
func (c *Config) UpdateSettings(mutate func(*Settings)) error {
	c.mu.Lock()
	mutate(&c.Settings)
	c.mu.Unlock()
	return c.SaveSettings()
}

// SaveSettings persists the current Settings into the settings namespace of the
// store. A nil store (before Init) is a no-op rather than an error.
func (c *Config) SaveSettings() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.settingsStore == nil {
		return nil
	}
	return c.settingsStore.Save("settings", c.Settings)
}

// SubDir returns (and ensures) a subdirectory under the data directory.
func (c *Config) SubDir(name string) string {
	p := filepath.Join(c.DataDir, name)
	os.MkdirAll(p, 0o755)
	return p
}

// Audit verifies configuration reachability (DESIGN §5 config.Audit):
// flags/settings must be observable and not silently dead. Returns a list of
// warnings. M2 performs a minimal check.
func (c *Config) Audit() []string {
	var warns []string
	if c.DataDir == "" {
		warns = append(warns, "data directory is empty")
	}
	if c.Settings.Failover.CooldownSeconds <= 0 {
		warns = append(warns, "failover.cooldownSeconds should be > 0")
	}
	if c.Settings.AutoRoute.Threshold <= 0 || c.Settings.AutoRoute.Threshold > 1 {
		warns = append(warns, "autoRoute.threshold should be within (0,1]")
	}
	return warns
}
