package config

import (
	"strings"
	"testing"
)

// The routing strategy reaches the UI as a dropdown option value, so an
// unrecognised value cannot just be ignored: the picker renders it as blank and
// the user sees an empty setting with no way to tell what is actually running.
// "hybrid" is exactly that case — it shipped as the default in an earlier build
// but the router only ever implemented "rule" and "model".
func TestMergeSettingsNormalisesStrategy(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"", Defaults().AutoRoute.Strategy},
		{"hybrid", Defaults().AutoRoute.Strategy},
		{"llm", Defaults().AutoRoute.Strategy},
		{"keyword", Defaults().AutoRoute.Strategy},
		{"rule", "rule"},
		{"model", "model"},
		{"MODEL", Defaults().AutoRoute.Strategy},
	}
	for _, c := range cases {
		got := MergeSettings(Settings{AutoRoute: AutoRouteSettings{Strategy: c.in}}).AutoRoute.Strategy
		if got != c.want {
			t.Errorf("strategy %q: want %q, got %q", c.in, c.want, got)
		}
	}
}

// The shipped default must be a value the router actually implements, otherwise
// a fresh install starts on a strategy the UI cannot represent.
func TestDefaultStrategyIsImplemented(t *testing.T) {
	switch Defaults().AutoRoute.Strategy {
	case "rule", "model":
	default:
		t.Fatalf("default strategy %q is not one the router implements",
			Defaults().AutoRoute.Strategy)
	}
}

// A fresh install must arrive fully capable. Anything the product needs in
// order to work at all has to be on out of the box: shipping memory off means
// the first thing a new user experiences is an assistant that forgets them,
// which reads as a defect rather than a conservative default. Capabilities
// only belong in the disabled set when the user deliberately switched them
// off (DESIGN §17 原则五).
func TestDefaultInstallEnablesCoreCapabilities(t *testing.T) {
	d := Defaults()
	if !d.MemoryAutoExtract {
		t.Fatal("long-term memory extraction must be on by default")
	}
	if len(d.DisabledModules) != 0 {
		t.Fatalf("a fresh install must not start with capabilities switched off: %v", d.DisabledModules)
	}
}

// A partial PUT must keep every field it did not mention. Booleans are the
// dangerous case: decoded straight into Settings, an absent bool becomes false,
// so saving one unrelated field used to switch off failover, auto-routing and
// memory extraction at once — all silently, and two of them are no longer
// visible anywhere in the UI.
func TestPatchKeepsUnmentionedBooleans(t *testing.T) {
	cur := Defaults()
	if !cur.AutoRoute.Enabled || !cur.MemoryAutoExtract {
		t.Fatalf("precondition changed: %+v", cur)
	}
	// Start failover from off so "kept as-is" is distinguishable from "reset to
	// the default", which now differs from the zero value.
	cur.Failover.Enabled = false
	id := "researcher"

	got := SettingsPatch{DefaultRoleId: &id}.Apply(cur)
	if got.DefaultRoleId != "researcher" {
		t.Fatalf("the field that was sent did not apply: %q", got.DefaultRoleId)
	}
	if got.Failover.Enabled {
		t.Fatal("a partial PUT re-enabled failover")
	}
	if !got.AutoRoute.Enabled {
		t.Fatal("a partial PUT switched auto-routing off")
	}
	if !got.MemoryAutoExtract {
		t.Fatal("a partial PUT switched memory extraction off")
	}
	if got.Failover.CooldownSeconds != cur.Failover.CooldownSeconds {
		t.Fatalf("cooldown was reset: %d", got.Failover.CooldownSeconds)
	}
}

// Presence has to work in the other direction too: switching something off must
// remain expressible, or the patch type would have traded one silent failure
// for another.
func TestPatchCanSwitchThingsOff(t *testing.T) {
	off := false
	got := SettingsPatch{
		MemoryAutoExtract: &off,
		Failover:          &FailoverPatch{Enabled: &off},
		AutoRoute:         &AutoRoutePatch{Enabled: &off},
	}.Apply(Defaults())

	if got.MemoryAutoExtract {
		t.Fatal("explicit false for memory extraction was ignored")
	}
	if got.Failover.Enabled {
		t.Fatal("explicit false for failover was ignored")
	}
	if got.AutoRoute.Enabled {
		t.Fatal("explicit false for auto-routing was ignored")
	}
}

// Normalisation still applies to a patch: an unknown strategy must not survive
// into the UI, where the picker renders it as blank.
func TestPatchStillNormalisesPlatformValues(t *testing.T) {
	bad := "hybrid"
	got := SettingsPatch{AutoRoute: &AutoRoutePatch{Strategy: &bad}}.Apply(Defaults())
	if got.AutoRoute.Strategy != Defaults().AutoRoute.Strategy {
		t.Fatalf("unrecognised strategy survived the patch: %q", got.AutoRoute.Strategy)
	}
}

// A partial PUT must not reset unrelated fields to zero — this is the reason
// the merge exists at all.
func TestMergeSettingsKeepsPartialFields(t *testing.T) {
	got := MergeSettings(Settings{DefaultRoleId: "researcher"})
	if got.DefaultRoleId != "researcher" {
		t.Fatalf("explicit role id was overwritten: %q", got.DefaultRoleId)
	}
	if got.StorageDriver != Defaults().StorageDriver {
		t.Fatalf("storage driver fell to zero: %q", got.StorageDriver)
	}
	if got.Failover.CooldownSeconds != Defaults().Failover.CooldownSeconds {
		t.Fatalf("cooldown fell to zero: %d", got.Failover.CooldownSeconds)
	}
}

// The API can read conversation history, rewrite provider credentials and drive
// the desktop, so binding it to a routable address without a token hands the
// machine to anyone on the network. The Origin check does not help here: it only
// constrains browsers, and a token-less install used to be one --addr away.
func TestNonLoopbackAddrRequiresToken(t *testing.T) {
	cases := []struct {
		name    string
		addr    string
		token   string
		wantErr bool
	}{
		{"default loopback", "127.0.0.1:8787", "", false},
		{"loopback ipv6", "[::1]:8787", "", false},
		{"localhost name", "localhost:8787", "", false},
		{"loopback with token", "127.0.0.1:8787", "s3cret", false},
		{"all interfaces (bare port)", ":8787", "", true},
		{"all interfaces (0.0.0.0)", "0.0.0.0:8787", "", true},
		{"lan address", "192.168.1.9:8787", "", true},
		{"lan address with token", "192.168.1.9:8787", "s3cret", false},
		{"public hostname", "hub.example.com:8787", "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := &Config{Addr: c.addr, Token: c.token}
			err := cfg.checkExposure()
			if c.wantErr && err == nil {
				t.Fatalf("addr %q without token was accepted; it must be refused", c.addr)
			}
			if !c.wantErr && err != nil {
				t.Fatalf("addr %q was refused: %v", c.addr, err)
			}
			if err != nil && !strings.Contains(err.Error(), "127.0.0.1:8787") {
				t.Errorf("error should suggest a loopback address to use, got: %v", err)
			}
		})
	}
}
