package envprobe

import (
	"strings"
	"testing"
)

// The three buckets are the whole point of this package: they are the three
// branches of the flow (start now / wait while it is prepared / install it
// first). A classification that leaks an installed program into Missing sends
// the user to install something they already have, so each bucket is pinned.
func TestProbeSortsInstalledAndAbsent(t *testing.T) {
	sc := scanResult{
		Platform: "test",
		Roots:    []string{`R:\Program Files`},
		PathExes: map[string]string{"ffmpeg.exe": `R:\bin\ffmpeg.exe`},
		Names:    []string{"Blender"},
	}
	// Blender resolves through the installed-names list, ffmpeg through PATH.
	rep := probeWith("我要做一个 2.5D 游戏", sc)

	got := map[string]bool{}
	for _, p := range rep.Ready {
		got[p.Name] = true
	}
	for _, p := range rep.NeedsInit {
		got[p.Name] = true
	}
	for _, w := range rep.Missing {
		if got[w.Name] {
			t.Errorf("%s is reported both as installed and as missing", w.Name)
		}
	}
	if !got["Blender"] {
		t.Errorf("Blender is installed but was not recognised: ready=%v needsInit=%v", names(rep.Ready), names2(rep.NeedsInit))
	}
	if got["Unity"] {
		t.Error("Unity is not installed and must not appear as installed")
	}
	for _, w := range rep.Missing {
		if w.Install == "" {
			t.Errorf("%s is offered as missing with no way to install it", w.Name)
		}
	}
}

// A program with no scriptable entry point is exactly the case that costs
// preparation time, and it must land in NeedsInit rather than Ready — going
// into Ready would have the agent promise work it cannot start.
func TestInterfaceOnlyProgramsNeedPreparation(t *testing.T) {
	sc := scanResult{Platform: "test", Roots: []string{`R:\Program Files`}, Names: []string{"Adobe Photoshop"}}
	rep := probeWith("帮我处理一批图片", sc)

	for _, p := range rep.Ready {
		if p.Name == "Adobe Photoshop" {
			t.Fatal("a UI-only program was reported as ready to drive")
		}
	}
	for _, p := range rep.NeedsInit {
		if p.Name == "Adobe Photoshop" {
			return
		}
	}
	t.Fatalf("Photoshop is installed but appeared in no bucket: ready=%v needs=%v", names(rep.Ready), names2(rep.NeedsInit))
}

// A request the catalogue does not know must say so. An empty report would read
// as "your machine has nothing installed", which is a different statement.
func TestUnknownRequestSaysSoAndStillListsInventory(t *testing.T) {
	sc := scanResult{
		Platform: "test",
		Names:    []string{"Some Bespoke CAD", "Blender"},
	}
	rep := probeWith("帮我整理一批建筑图纸", sc)
	if !rep.NoMatch {
		t.Fatal("an unmatched request must be flagged as unmatched")
	}
	if len(rep.Others) == 0 {
		t.Fatal("the inventory must still be reported when nothing matched")
	}
	joined := strings.Join(rep.Others, "|")
	if !strings.Contains(joined, "Some Bespoke CAD") {
		t.Errorf("the user's own software is the best hint and must be listed, got %v", rep.Others)
	}
}

// A crowded machine must not push the relevant answers out of the report.
func TestOthersIsCappedAndExcludesChosenPrograms(t *testing.T) {
	var installed []string
	for i := 0; i < 200; i++ {
		installed = append(installed, "Filler App "+string(rune('A'+i%26))+strings.Repeat("x", i%5)+itoa(i))
	}
	installed = append(installed, "Blender")
	sc := scanResult{Platform: "test", Names: installed}
	rep := probeWith("做个 3d 模型", sc)

	if len(rep.Others) > 24 {
		t.Fatalf("Others must stay bounded, got %d entries", len(rep.Others))
	}
	for _, o := range rep.Others {
		if strings.EqualFold(o, "Blender") {
			t.Error("a program already reported in a bucket must not be repeated in Others")
		}
	}
}

// Tags come from what the user typed, and Chinese carries no word boundaries —
// the whole match hinges on substring detection working for mixed strings.
func TestTagsForMixedChineseRequests(t *testing.T) {
	cases := map[string]string{
		"我需要做一个2.5D游戏": "游戏",
		"帮我剪个视频":       "视频",
		"写个爬虫把价格抓下来":   "脚本",
		"给我画个像素小人":     "美术",
	}
	for q, want := range cases {
		tags := tagsFor(q)
		if !contains(tags, want) {
			t.Errorf("tagsFor(%q) = %v, expected it to include %q", q, tags, want)
		}
	}
	if tags := tagsFor(""); tags != nil {
		t.Errorf("an empty request must produce no tags, got %v", tags)
	}
}

// Version numbers live in directory names and are worth extracting: they are
// what the user sees in the console and what tells the agent which CLI syntax
// applies.
func TestVersionFromInstallDirectoryNames(t *testing.T) {
	cases := map[string]string{
		"Blender 4.2":        "4.2",
		"Krita (x64) 5.2.2":  "5.2.2",
		"Godot":              "",
		"Adobe Photoshop":    "",
		"Python312":          "",
		"Blender Foundation": "",
	}
	for in, want := range cases {
		if got := versionFrom(in); got != want {
			t.Errorf("versionFrom(%q) = %q, want %q", in, got, want)
		}
	}
}

// The catalogue is only useful if every row can actually answer the two
// questions the flow asks — what is it for, and how does one get it.
func TestCatalogueRowsAreComplete(t *testing.T) {
	for _, e := range catalogue {
		if e.Name == "" || len(e.Tags) == 0 || len(e.Patterns) == 0 && len(e.Exes) == 0 {
			t.Errorf("catalogue row %q cannot be detected or is untagged", e.Name)
		}
		if e.Why == "" {
			t.Errorf("catalogue row %q does not say what it is for", e.Name)
		}
		if e.Install == "" {
			t.Errorf("catalogue row %q offers no way to install it", e.Name)
		}
		if e.Driveable && e.DriveNote == "" {
			t.Errorf("catalogue row %q claims to be scriptable but names no entry point", e.Name)
		}
	}
}

func names(ps []Program) []string {
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		out = append(out, p.Name)
	}
	return out
}

func names2(ps []Program) []string { return names(ps) }

func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}
