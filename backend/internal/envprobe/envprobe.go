// Package envprobe answers one question before any work starts: on THIS
// machine, what can do the thing the user just asked for?
//
// It exists because of the shape of the product's main flow (DESIGN §9.3). The
// user says one sentence — "我要做一个 2.5D 游戏" — and the agent then has to
// pick one of three branches:
//
//   - a program is already installed and can be scripted → start now;
//   - a program is installed but only has a user interface → work out how to
//     drive it first, which takes a while, so tell the user to wait;
//   - nothing suitable is installed → say what is needed and how to get it.
//
// All three branches start from the same inventory, so the inventory is read
// once and classified into exactly those three buckets. That is what Report is:
// not a list of software, but a decision the agent can act on.
//
// Nothing here writes, installs or launches anything. It reads directory names,
// PATH entries and Start Menu shortcuts, and reports what it saw.
package envprobe

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Program is one thing found on this machine.
type Program struct {
	Name string `json:"name"`
	// Version is best-effort, read from the install directory name (many
	// installers put it there) or left empty when the layout does not say.
	Version string `json:"version,omitempty"`
	// Path is the executable or install directory that matched.
	Path string `json:"path,omitempty"`
	// Source records how it was found, so a surprising hit can be explained
	// ("它在 PATH 里" reads very differently from "它在 Program Files 里").
	Source string `json:"source"`
	// Driveable is true when the program can be run without a person looking
	// at its window — a documented CLI or headless mode. False does not mean
	// "impossible", it means "the agent has to operate the interface", which is
	// the branch that costs preparation time.
	Driveable bool `json:"driveable"`
	// DriveNote is the concrete invocation the agent should start from.
	DriveNote string `json:"driveNote,omitempty"`
}

// Want is something that is not installed but would do the job.
type Want struct {
	Name string `json:"name"`
	// Why says what this program would be used for, in the user's terms.
	Why string `json:"why"`
	// Install is where to get it. Both halves are useful: the agent can run the
	// package-manager command itself when the user agrees, and the user can be
	// handed the URL to install it their own way.
	Install   string `json:"install"`
	PkgID     string `json:"pkgId,omitempty"`
	Free      bool   `json:"free"`
	Driveable bool   `json:"driveable"`
}

// Report is the answer: the three branches, already sorted into buckets.
//
// Ready / NeedsInit / Missing are mutually exclusive and together cover every
// relevant option, so the agent never has to re-derive which branch applies.
type Report struct {
	Platform string `json:"platform"`
	Query    string `json:"query"`
	// Ready: installed and scriptable — work can start immediately.
	Ready []Program `json:"ready"`
	// NeedsInit: installed but interface-only — usable after the agent works
	// out how to drive it, which is the "wait a moment" branch.
	NeedsInit []Program `json:"needsInit"`
	// Missing: not installed — the "install this first" branch.
	Missing []Want `json:"missing"`
	// MissingMore counts the suggestions that did not fit, so the agent knows
	// the list is a recommendation rather than the whole catalogue.
	MissingMore int `json:"missingMore,omitempty"`
	// Others is the rest of what is installed, trimmed. It is here because the
	// user's own machine is the best hint about what they would rather use: a
	// request that matched nothing in the catalogue can still be served by
	// something already present, and that only shows if it is mentioned.
	Others []string `json:"others,omitempty"`
	// NoMatch is true when the request matched no catalogue entry at all, so
	// the agent knows the buckets are empty because nothing was recognised
	// rather than because the machine has nothing on it.
	NoMatch   bool   `json:"noMatch"`
	ScannedAt string `json:"scannedAt"`
}

// entry is one catalogue row: a program that is useful for some class of task,
// where it would live, and whether it can be driven without a person.
//
// The catalogue is deliberately small and opinionated. A full software
// inventory cannot answer "can this machine make a 2.5D game" — only a curated
// list of the programs that actually do that job can.
type entry struct {
	Name string
	// Tags are the task words this program serves; see tagSynonyms.
	Tags []string
	// Why and Install are copied into Want when the program is absent.
	Why     string
	Install string
	PkgID   string
	Free    bool
	// Patterns are install-root-relative paths; '*' matches one segment.
	Patterns []string
	// Exes are file names looked up on PATH.
	Exes []string
	// Driveable plus DriveNote describe the scriptable entry point, when there
	// is one.
	Driveable bool
	DriveNote string
}

// maxMissing caps the install suggestions; see Probe.
const maxMissing = 6

// tagSynonyms maps what a user actually types onto catalogue tags. Chinese
// requests carry no spaces, so substring matching against the raw query is the
// only workable approach — "我要做一个2.5D游戏" contains "游戏" and nothing else
// that a tokeniser could key on.
var tagSynonyms = []struct {
	Words []string
	Tags  []string
}{
	{[]string{"游戏", "game", "unity", "godot", "unreal", "虚幻", "引擎"}, []string{"游戏"}},
	{[]string{"2.5d", "2.5D", "等距", "斜45", "斜角"}, []string{"2d", "3d", "地图", "美术"}},
	{[]string{"2d", "2D", "二维", "像素", "pixel"}, []string{"2d", "美术"}},
	{[]string{"3d", "3D", "三维", "模型", "建模"}, []string{"3d", "模型"}},
	{[]string{"动画", "动画片", "骨骼", "animation"}, []string{"动画"}},
	{[]string{"美术", "画", "绘画", "图标", "素材", "贴图", "立绘"}, []string{"美术", "图像"}},
	{[]string{"图片", "图像", "照片", "修图", "抠图", "压缩图"}, []string{"图像"}},
	{[]string{"矢量", "svg", "logo", "标志"}, []string{"矢量"}},
	{[]string{"地图", "关卡", "场景", "tile", "关卡设计"}, []string{"地图"}},
	{[]string{"音乐", "配乐", "编曲", "midi"}, []string{"音乐"}},
	{[]string{"音频", "声音", "录音", "降噪", "配音"}, []string{"音频"}},
	{[]string{"视频", "剪辑", "转码", "字幕", "录屏"}, []string{"视频"}},
	{[]string{"网站", "网页", "前端", "页面", "h5"}, []string{"开发", "文档"}},
	{[]string{"脚本", "自动化", "爬虫", "批处理", "定时"}, []string{"脚本", "开发"}},
	{[]string{"文档", "报告", "论文", "排版", "pdf"}, []string{"文档"}},
	{[]string{"部署", "服务器", "环境", "代码"}, []string{"开发"}},
}

// catalogue is the built-in table. Order matters only for tie-breaking: earlier
// rows are preferred when two fit the same request equally well.
var catalogue = []entry{
	{
		Name: "Blender", Tags: []string{"3d", "模型", "动画", "游戏", "美术", "2d"},
		Why:     "建 3D 模型、做动画、渲染素材（游戏里的角色、场景都可以在这里做）",
		Install: "https://www.blender.org/download/，或 winget install BlenderFoundation.Blender",
		PkgID:   "BlenderFoundation.Blender", Free: true,
		Patterns:  []string{`Blender Foundation\Blender *`, `Blender *`},
		Exes:      []string{"blender.exe"},
		Driveable: true,
		DriveNote: "blender --background --python script.py（无界面跑脚本）",
	},
	{
		Name: "Godot", Tags: []string{"游戏", "2d", "3d", "地图"},
		Why:     "做 2D / 3D 游戏的完整引擎，免费、体积小，脚本驱动最省事",
		Install: "https://godotengine.org/download/，或 winget install GodotEngine.Godot",
		PkgID:   "GodotEngine.Godot", Free: true,
		Patterns:  []string{`Godot *`, `Godot`},
		Exes:      []string{"godot.exe"},
		Driveable: true,
		DriveNote: "godot --headless --path <项目> --script（无界面导项目/导资源）",
	},
	{
		Name: "Unity", Tags: []string{"游戏", "2d", "3d", "地图"},
		Why:       "主流游戏引擎，2.5D 用得多；批处理模式可以直接编译出包",
		Install:   "https://unity.com/download（先装 Unity Hub），或 winget install Unity.UnityHub",
		PkgID:     "Unity.UnityHub",
		Patterns:  []string{`Unity\Hub\Editor\*\Editor`, `Unity Hub`, `Unity\Editor`},
		Exes:      []string{"Unity.exe", "Unity Hub.exe"},
		Driveable: true,
		DriveNote: "Unity.exe -batchmode -quit -projectPath <项目> -executeMethod <方法>",
	},
	{
		Name: "Unreal Engine", Tags: []string{"游戏", "3d", "地图"},
		Why:       "画质取向的 3D 游戏引擎，适合做写实风格的 2.5D / 3D",
		Install:   "https://www.unrealengine.com/download（走 Epic Games Launcher）",
		Patterns:  []string{`Epic Games\UE_*`},
		Exes:      []string{"UnrealEditor.exe", "UnrealEditor-Cmd.exe"},
		Driveable: true,
		DriveNote: "UnrealEditor-Cmd.exe <项目> -run=... -unattended",
	},
	{
		Name: "GameMaker", Tags: []string{"游戏", "2d"},
		Why:       "专做 2D 游戏的引擎，上手快",
		Install:   "https://gamemaker.io/download",
		Patterns:  []string{`GameMaker Studio 2`, `GameMaker`},
		Driveable: true,
		DriveNote: "Igor.exe / gm.exe 命令行构建",
	},
	{
		Name: "Tiled", Tags: []string{"地图", "2d", "游戏"},
		Why:     "拼 2.5D / 2D 关卡地图，导出给引擎用",
		Install: "https://www.mapeditor.org/download.html，或 winget install mapeditor.Tiled",
		PkgID:   "mapeditor.Tiled", Free: true,
		Patterns:  []string{`Tiled`},
		Exes:      []string{"tiled.exe"},
		Driveable: true,
		DriveNote: "tiled --export-map <tmx> <导出路径>",
	},
	{
		Name: "Aseprite", Tags: []string{"美术", "2d", "图像", "动画"},
		Why:       "画像素图和逐帧动画，2.5D 小人也常在这里做",
		Install:   "https://www.aseprite.org/download/",
		Patterns:  []string{`Aseprite`, `Steam\steamapps\common\Aseprite`},
		Exes:      []string{"aseprite.exe"},
		Driveable: true,
		DriveNote: "aseprite -b --script-param ... --script <脚本>",
	},
	{
		Name: "Krita", Tags: []string{"美术", "2d", "图像", "动画"},
		Why:     "免费绘画软件，画立绘、贴图、逐帧动画",
		Install: "https://krita.org/en/download/，或 winget install KDE.Krita",
		PkgID:   "KDE.Krita", Free: true,
		Patterns:  []string{`Krita*`},
		Exes:      []string{"krita.exe"},
		Driveable: true,
		DriveNote: "krita --export --export-filename <输出> <源文件>",
	},
	{
		Name: "GIMP", Tags: []string{"图像", "美术", "2d"},
		Why:     "免费的图片处理，修图、切图、批量导出",
		Install: "https://www.gimp.org/downloads/，或 winget install GIMP.GIMP",
		PkgID:   "GIMP.GIMP", Free: true,
		Patterns:  []string{`GIMP *`},
		Exes:      []string{"gimp-2.10.exe", "gimp.exe"},
		Driveable: true,
		DriveNote: "gimp -i -b '(脚本)' -b '(gimp-quit 0)'",
	},
	{
		Name: "Inkscape", Tags: []string{"矢量", "图像", "美术"},
		Why:     "矢量图 / 图标，导出各种尺寸",
		Install: "https://inkscape.org/release/，或 winget install Inkscape.Inkscape",
		PkgID:   "Inkscape.Inkscape", Free: true,
		Patterns:  []string{`Inkscape`},
		Exes:      []string{"inkscape.exe"},
		Driveable: true,
		DriveNote: "inkscape --export-type=png --export-filename=<输出> <svg>",
	},
	{
		Name: "Adobe Photoshop", Tags: []string{"图像", "美术", "2d"},
		Why:       "专业图片处理",
		Install:   "https://www.adobe.com/products/photoshop.html（Creative Cloud）",
		Patterns:  []string{`Adobe\Adobe Photoshop *`},
		Driveable: false,
	},
	{
		Name: "Clip Studio Paint", Tags: []string{"美术", "2d", "动画"},
		Why:       "画插画与逐帧动画",
		Install:   "https://www.clipstudio.net/en/dl/",
		Patterns:  []string{`CELSYS\*`},
		Driveable: false,
	},
	{
		Name: "ImageMagick", Tags: []string{"图像", "脚本", "开发"},
		Why:     "命令行批量处理图片：切图、改尺寸、转格式、拼雪碧图",
		Install: "https://imagemagick.org/script/download.php，或 winget install ImageMagick.ImageMagick",
		PkgID:   "ImageMagick.ImageMagick", Free: true,
		Exes:      []string{"magick.exe"},
		Driveable: true,
		DriveNote: "magick convert / montage（分批处理图片）",
	},
	{
		Name: "ffmpeg", Tags: []string{"视频", "音频", "图像", "脚本"},
		Why:     "命令行转码、剪辑、抽帧、压制，做游戏里任何音视频处理",
		Install: "https://www.gyan.dev/ffmpeg/builds/，或 winget install Gyan.FFmpeg",
		PkgID:   "Gyan.FFmpeg", Free: true,
		Exes:      []string{"ffmpeg.exe"},
		Driveable: true,
		DriveNote: "ffmpeg -i <输入> <参数> <输出>",
	},
	{
		Name: "Audacity", Tags: []string{"音频", "音乐"},
		Why:     "录音、降噪、剪音频",
		Install: "https://www.audacityteam.org/download/，或 winget install Audacity.Audacity",
		PkgID:   "Audacity.Audacity", Free: true,
		Patterns:  []string{`Audacity`},
		Exes:      []string{"audacity.exe"},
		Driveable: true,
		DriveNote: "audacity --batch 走脚本（新版为 mod-script-pipe）",
	},
	{
		Name: "FL Studio", Tags: []string{"音乐", "音频"},
		Why:       "做配乐、编曲",
		Install:   "https://www.image-line.com/fl-studio-download/",
		Patterns:  []string{`Image-Line\FL Studio *`},
		Driveable: false,
	},
	{
		Name: "LMMS", Tags: []string{"音乐", "音频"},
		Why:       "免费编曲软件",
		Install:   "https://lmms.io/download",
		Patterns:  []string{`LMMS`},
		Driveable: false,
	},
	{
		Name: "OBS Studio", Tags: []string{"视频", "录屏"},
		Why:     "录屏、直播，录制操作过程",
		Install: "https://obsproject.com/download，或 winget install OBSProject.OBSStudio",
		PkgID:   "OBSProject.OBSStudio", Free: true,
		Patterns:  []string{`obs-studio`},
		Exes:      []string{"obs64.exe"},
		Driveable: true,
		DriveNote: "obs64.exe --startrecording --minimize-to-tray",
	},
	{
		Name: "HandBrake", Tags: []string{"视频"},
		Why:     "视频压缩转码",
		Install: "https://handbrake.fr/downloads.php，或 winget install HandBrake.HandBrake",
		PkgID:   "HandBrake.HandBrake", Free: true,
		Patterns:  []string{`HandBrake`},
		Driveable: true,
		DriveNote: "HandBrakeCLI.exe -i <输入> -o <输出>",
	},
	{
		Name: "Visual Studio Code", Tags: []string{"开发", "文档", "脚本"},
		Why:     "写代码、改配置、看日志",
		Install: "https://code.visualstudio.com/download，或 winget install Microsoft.VisualStudioCode",
		PkgID:   "Microsoft.VisualStudioCode", Free: true,
		Patterns:  []string{`Microsoft VS Code`},
		Exes:      []string{"code.cmd", "code.exe"},
		Driveable: true,
		DriveNote: "code --new-window <目录>",
	},
	{
		Name: "Python", Tags: []string{"脚本", "开发", "图像", "文档"},
		Why:     "兜底：几乎所有批处理、转换、生成都能用它写脚本完成",
		Install: "https://www.python.org/downloads/，或 winget install Python.Python.3.13",
		PkgID:   "Python.Python.3.13", Free: true,
		Exes:      []string{"python.exe", "python3.exe"},
		Driveable: true,
		DriveNote: "python script.py",
	},
	{
		Name: "Node.js", Tags: []string{"开发", "脚本"},
		Why:     "跑前端工具链与 JS 脚本",
		Install: "https://nodejs.org/，或 winget install OpenJS.NodeJS",
		PkgID:   "OpenJS.NodeJS", Free: true,
		Exes:      []string{"node.exe"},
		Driveable: true,
		DriveNote: "node script.js",
	},
	{
		Name: "Git", Tags: []string{"开发", "文档"},
		Why:     "版本管理，也是很多工具链的前置",
		Install: "https://git-scm.com/download/win，或 winget install Git.Git",
		PkgID:   "Git.Git", Free: true,
		Exes:      []string{"git.exe"},
		Driveable: true,
		DriveNote: "git <子命令>",
	},
	{
		Name: "LibreOffice", Tags: []string{"文档"},
		Why:     "写文档、表格、导出 PDF",
		Install: "https://www.libreoffice.org/download/，或 winget install TheDocumentFoundation.LibreOffice",
		PkgID:   "TheDocumentFoundation.LibreOffice", Free: true,
		Patterns:  []string{`LibreOffice`},
		Exes:      []string{"soffice.exe"},
		Driveable: true,
		DriveNote: "soffice --headless --convert-to pdf <文件>",
	},
	{
		Name: "Microsoft Office", Tags: []string{"文档"},
		Why:       "写文档、表格、演示",
		Install:   "https://www.office.com/",
		Patterns:  []string{`Microsoft Office\root\Office*`, `Microsoft Office\Office*`},
		Driveable: false,
	},
}

// Probe classifies this machine against one request.
//
// The query is matched against tagSynonyms first and against the catalogue names
// second, so both "我要做一个2.5D游戏" and a bare "Blender" work. A request that
// matches nothing returns NoMatch with the full inventory in Others — an empty
// answer would read as "your machine has nothing", which is a different and
// usually wrong statement.
func Probe(query string) Report { return probeWith(query, scan()) }

// probeWith is the testable half: classification without touching the machine.
// The split matters because the three-bucket decision is the part that has to
// be right, and it must be provable on a machine that has none of these
// programs installed.
func probeWith(query string, sc scanResult) Report {
	rep := Report{Platform: sc.Platform, Query: query, ScannedAt: time.Now().Format(time.RFC3339)}

	tags := tagsFor(query)
	matched := make([]entry, 0, 4)
	for _, e := range catalogue {
		if len(tags) > 0 {
			if intersects(e.Tags, tags) {
				matched = append(matched, e)
			}
			continue
		}
		if matchesName(e, query) {
			matched = append(matched, e)
		}
	}

	seen := map[string]bool{}
	for _, e := range matched {
		if p, ok := detect(e, sc); ok {
			seen[e.Name] = true
			if p.Driveable {
				rep.Ready = append(rep.Ready, p)
			} else {
				rep.NeedsInit = append(rep.NeedsInit, p)
			}
			continue
		}
		rep.Missing = append(rep.Missing, Want{
			Name: e.Name, Why: e.Why, Install: e.Install, PkgID: e.PkgID,
			Free: e.Free, Driveable: e.Driveable,
		})
	}
	// One request can legitimately fit a dozen programs; handing all of them to
	// the user is a menu, not an answer. The catalogue's own order is the
	// preference order, so the first few are the recommendation.
	if len(rep.Missing) > maxMissing {
		rep.MissingMore = len(rep.Missing) - maxMissing
		rep.Missing = rep.Missing[:maxMissing]
	}
	// Nothing recognised is worth saying out loud: an empty report reads as
	// "your machine has nothing installed", which is a different statement.
	rep.NoMatch = len(matched) == 0 && strings.TrimSpace(query) != ""

	// Anything installed that is not already accounted for, newest-looking
	// first is not available, so keep the order the scan produced and cap it.
	for _, name := range sc.Names {
		if len(rep.Others) >= 24 {
			break
		}
		if seen[name] || reportedName(rep, name) {
			continue
		}
		rep.Others = append(rep.Others, name)
	}
	// Empty slices rather than nil: JSON null for "nothing in this bucket"
	// forces every client to write a guard, and the obvious loop over it
	// crashes. The API says the same thing with [] and iterates safely.
	if rep.Ready == nil {
		rep.Ready = []Program{}
	}
	if rep.NeedsInit == nil {
		rep.NeedsInit = []Program{}
	}
	if rep.Missing == nil {
		rep.Missing = []Want{}
	}
	return rep
}

// reportedName guards against listing a program in Others that is already named
// in one of the three buckets under a different spelling.
func reportedName(rep Report, name string) bool {
	for _, p := range rep.Ready {
		if strings.EqualFold(p.Name, name) {
			return true
		}
	}
	for _, p := range rep.NeedsInit {
		if strings.EqualFold(p.Name, name) {
			return true
		}
	}
	return false
}

// tagsFor turns a request into catalogue tags.
func tagsFor(q string) []string {
	if strings.TrimSpace(q) == "" {
		return nil
	}
	low := strings.ToLower(q)
	var out []string
	seen := map[string]bool{}
	for _, syn := range tagSynonyms {
		for _, w := range syn.Words {
			if strings.Contains(low, strings.ToLower(w)) {
				for _, t := range syn.Tags {
					if !seen[t] {
						seen[t] = true
						out = append(out, t)
					}
				}
				break
			}
		}
	}
	return out
}

func matchesName(e entry, q string) bool {
	low := strings.ToLower(strings.TrimSpace(q))
	if low == "" {
		return false
	}
	if strings.Contains(low, strings.ToLower(e.Name)) {
		return true
	}
	for _, ex := range e.Exes {
		if strings.Contains(low, strings.ToLower(strings.TrimSuffix(ex, ".exe"))) {
			return true
		}
	}
	return false
}

func intersects(a, b []string) bool {
	for _, x := range a {
		for _, y := range b {
			if x == y {
				return true
			}
		}
	}
	return false
}

// detect looks for one catalogue entry, preferring an install-root match (which
// carries a path and often a version) over a PATH lookup.
func detect(e entry, sc scanResult) (Program, bool) {
	for _, root := range sc.Roots {
		for _, pat := range e.Patterns {
			hits := expand(root, pat)
			if len(hits) == 0 {
				continue
			}
			sort.Strings(hits)
			// Prefer the newest-looking match: version directories sort
			// lexically, so take the last one.
			p := hits[len(hits)-1]
			name := filepath.Base(p)
			return Program{
				Name: e.Name, Version: versionFrom(name), Path: p, Source: "安装目录",
				Driveable: e.Driveable, DriveNote: e.DriveNote,
			}, true
		}
	}
	for _, ex := range e.Exes {
		if p, ok := sc.PathExes[strings.ToLower(ex)]; ok {
			return Program{
				Name: e.Name, Path: p, Source: "PATH",
				Driveable: e.Driveable, DriveNote: e.DriveNote,
			}, true
		}
	}
	for _, short := range e.Exes {
		stem := strings.ToLower(strings.TrimSuffix(short, ".exe"))
		for _, n := range sc.Names {
			if strings.EqualFold(n, e.Name) || strings.EqualFold(n, stem) {
				return Program{Name: e.Name, Source: "已安装列表", Driveable: e.Driveable, DriveNote: e.DriveNote}, true
			}
		}
	}
	for _, n := range sc.Names {
		if strings.EqualFold(n, e.Name) {
			return Program{Name: e.Name, Source: "已安装列表", Driveable: e.Driveable, DriveNote: e.DriveNote}, true
		}
	}
	return Program{}, false
}

// versionFrom pulls a version out of an install directory name such as
// "Blender 4.2" or "Krita (x64) 5.2.2". Returns "" when nothing looks like one.
func versionFrom(name string) string {
	fields := strings.FieldsFunc(name, func(r rune) bool {
		return r == ' ' || r == '(' || r == ')' || r == '_' || r == '-'
	})
	for _, f := range fields {
		if len(f) < 2 {
			continue
		}
		digits, dots := 0, 0
		for _, r := range f {
			switch {
			case r >= '0' && r <= '9':
				digits++
			case r == '.':
				dots++
			}
		}
		if digits >= 2 && dots >= 1 && digits+dots == len(f) {
			return f
		}
	}
	return ""
}

// expand resolves a pattern under one root. A '*' segment lists the directory
// rather than recursing blindly, which keeps a probe of Program Files bounded.
func expand(root, pattern string) []string {
	segs := strings.Split(strings.Trim(pattern, `\/`), `\`)
	cur := []string{root}
	for _, seg := range segs {
		var next []string
		for _, base := range cur {
			if strings.ContainsAny(seg, "*?") {
				ents, err := os.ReadDir(base)
				if err != nil {
					continue
				}
				for _, ent := range ents {
					if ok, _ := filepath.Match(seg, ent.Name()); ok {
						next = append(next, filepath.Join(base, ent.Name()))
					}
				}
				continue
			}
			p := filepath.Join(base, seg)
			if _, err := os.Stat(p); err == nil {
				next = append(next, p)
			}
		}
		cur = next
		if len(cur) == 0 {
			return nil
		}
	}
	return cur
}
