//go:build windows

package envprobe

import (
	"os"
	"path/filepath"
	"strings"
)

// scan reads the three places a Windows program announces itself: the install
// roots, the Start Menu, and PATH.
//
// It is deliberately shallow and read-only. A full recursive walk of Program
// Files would take seconds and find mostly redistributables; what the flow
// needs is "is Blender here", and that is answerable from directory names plus
// a bounded pattern expansion in envprobe.expand.
func scan() scanResult {
	roots := installRoots()
	res := scanResult{Platform: "windows", Roots: roots, PathExes: pathExes()}

	var names []string
	for _, r := range roots {
		names = append(names, topDirs(r)...)
	}
	names = append(names, startMenuNames()...)
	res.Names = dedupeNames(names, 400)
	return res
}

// installRoots are the directories Windows installers actually use. LocalAppData
// matters as much as the two Program Files: per-user installs (VS Code, many
// smaller tools, anything installed without an admin prompt) land there and
// nowhere else.
func installRoots() []string {
	cands := []string{
		os.Getenv("ProgramFiles"),
		os.Getenv("ProgramFiles(x86)"),
		filepath.Join(os.Getenv("LOCALAPPDATA"), "Programs"),
		filepath.Join(os.Getenv("ProgramData"), "chocolatey", "lib"),
	}
	var out []string
	seen := map[string]bool{}
	for _, c := range cands {
		if c == "" || seen[strings.ToLower(c)] {
			continue
		}
		if st, err := os.Stat(c); err == nil && st.IsDir() {
			seen[strings.ToLower(c)] = true
			out = append(out, c)
		}
	}
	return out
}

// startMenuNames reads shortcut names, which is the list a person would give if
// asked "what is installed on your machine". It catches programs whose install
// layout the catalogue does not know, which is what Others is for.
func startMenuNames() []string {
	var out []string
	for _, base := range []string{
		filepath.Join(os.Getenv("ProgramData"), "Microsoft", "Windows", "Start Menu", "Programs"),
		filepath.Join(os.Getenv("APPDATA"), "Microsoft", "Windows", "Start Menu", "Programs"),
	} {
		if base == "" {
			continue
		}
		_ = filepath.WalkDir(base, func(p string, d os.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if len(out) >= 500 {
				return filepath.SkipAll
			}
			if d.IsDir() {
				if strings.Count(strings.TrimPrefix(p, base), string(os.PathSeparator)) > 3 {
					return filepath.SkipDir
				}
				return nil
			}
			if strings.EqualFold(filepath.Ext(p), ".lnk") {
				out = append(out, strings.TrimSuffix(filepath.Base(p), filepath.Ext(p)))
			}
			return nil
		})
	}
	return out
}
