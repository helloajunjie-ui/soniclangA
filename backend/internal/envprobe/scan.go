package envprobe

import (
	"os"
	"path/filepath"
	"strings"
)

// scanResult is the raw inventory a platform backend produces. Everything the
// classification step needs is here, so the platform files stay free of policy.
type scanResult struct {
	Platform string
	// Roots are the install directories to match catalogue patterns under.
	Roots []string
	// PathExes maps a lower-cased executable name to its full path on PATH.
	PathExes map[string]string
	// Names are human-readable program names from the installed-software list
	// (Start Menu shortcuts and install-root directories on Windows).
	Names []string
}

// dedupeNames keeps the first occurrence of each name, case-insensitively, and
// drops the noise that install roots are full of: redistributable folders,
// uninstallers and anything with no letter in it.
func dedupeNames(names []string, limit int) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(names))
	for _, n := range names {
		n = strings.TrimSpace(n)
		if n == "" || seen[strings.ToLower(n)] {
			continue
		}
		if isNoise(n) {
			continue
		}
		seen[strings.ToLower(n)] = true
		out = append(out, n)
		if len(out) >= limit {
			break
		}
	}
	return out
}

var noiseWords = []string{
	"uninstall", "redistributable", "update", "updater", "driver", "runtime",
	"windows kits", "windowsapps", "common files", "modifiablewindowsapps",
	"microsoft", "intel", "amd", "nvidia", "realtek", "temp", "cache",
	"documentation", "licenses", "resources", "plugins",
}

func isNoise(n string) bool {
	low := strings.ToLower(n)
	for _, w := range noiseWords {
		if strings.Contains(low, w) {
			return true
		}
	}
	return false
}

// pathExes indexes the executables reachable through PATH.
//
// PATH lookup is one of the three ways a program announces itself, and the only
// one that catches the portable/CLI tools the flow actually prefers — Godot
// unpacked to a folder, ffmpeg dropped into C:\bin, a Python installed for a
// side project. Missing it would push the agent toward "you need to install
// something" when the machine already has it.
func pathExes() map[string]string {
	out := map[string]string{}
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if dir == "" {
			continue
		}
		ents, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, ent := range ents {
			if ent.IsDir() || !strings.EqualFold(filepath.Ext(ent.Name()), ".exe") {
				continue
			}
			key := strings.ToLower(ent.Name())
			if _, ok := out[key]; !ok {
				out[key] = filepath.Join(dir, ent.Name())
			}
		}
	}
	return out
}

// topDirs lists the immediate subdirectory names of a root, which on Windows is
// usually (publisher) or (program name) — enough to recognise a product.
func topDirs(root string) []string {
	ents, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	var out []string
	for _, ent := range ents {
		if ent.IsDir() {
			out = append(out, ent.Name())
		}
	}
	return out
}
