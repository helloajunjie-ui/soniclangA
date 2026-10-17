// Command agenthub is the AgentHub local agent proxy entry point (DESIGN §15).
// It wires the zero-dependency Go backend: config + vault, every module service,
// the tool registry (with the memory searcher injected), the orchestrator and
// the HTTP server serving the embedded frontend + OpenAI-compatible API.
package main

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/agenthub/agent-proxy/internal/audit"
	"github.com/agenthub/agent-proxy/internal/brief"
	"github.com/agenthub/agent-proxy/internal/config"
	"github.com/agenthub/agent-proxy/internal/control"
	"github.com/agenthub/agent-proxy/internal/conversation"
	"github.com/agenthub/agent-proxy/internal/envprobe"
	"github.com/agenthub/agent-proxy/internal/job"
	"github.com/agenthub/agent-proxy/internal/keyvault"
	"github.com/agenthub/agent-proxy/internal/knowledge"
	"github.com/agenthub/agent-proxy/internal/knowledge/ingest"
	"github.com/agenthub/agent-proxy/internal/logx"
	"github.com/agenthub/agent-proxy/internal/mcp"
	"github.com/agenthub/agent-proxy/internal/memory"
	"github.com/agenthub/agent-proxy/internal/memory/consolidate"
	"github.com/agenthub/agent-proxy/internal/memory/extract"
	"github.com/agenthub/agent-proxy/internal/methodology"
	"github.com/agenthub/agent-proxy/internal/module"
	"github.com/agenthub/agent-proxy/internal/orchestrator"
	"github.com/agenthub/agent-proxy/internal/provider"
	"github.com/agenthub/agent-proxy/internal/rea"
	"github.com/agenthub/agent-proxy/internal/retrieval"
	"github.com/agenthub/agent-proxy/internal/role"
	"github.com/agenthub/agent-proxy/internal/server"
	"github.com/agenthub/agent-proxy/internal/server/handler"
	"github.com/agenthub/agent-proxy/internal/skill"
	"github.com/agenthub/agent-proxy/internal/storage"
	"github.com/agenthub/agent-proxy/internal/tool/builtin"
	"github.com/agenthub/agent-proxy/internal/tool/registry"
	"github.com/agenthub/agent-proxy/internal/tool/sandbox"
	"github.com/getlantern/systray"
)

func main() {
	cfg, err := config.Parse()
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	if err := cfg.Init(); err != nil {
		log.Fatalf("init: %v", err)
	}
	for _, w := range cfg.Audit() {
		log.Printf("config warning: %s", w)
	}

	// Process-wide runtime log (HTTP, tools, external API calls, errors).
	// AGENTHUB_DEBUG=1 enables DEBUG-level output.
	appLog := logx.Init(filepath.Join(cfg.DataDir, "logs", "agenthub.log"),
		os.Getenv("AGENTHUB_DEBUG") == "1")
	appLog.Infof("AgentHub 启动中，数据目录 %s，日志目录 %s", cfg.DataDir, filepath.Join(cfg.DataDir, "logs"))
	defer appLog.Close()

	vault, err := keyvault.New(cfg.DataDir)
	if err != nil {
		log.Fatalf("vault: %v", err)
	}

	retr := retrieval.New()

	// open returns the configured persistence backend for a namespace. With the
	// sqlite driver any pre-existing JSON data is migrated across on first run,
	// so enabling it never loses existing configuration.
	open := func(ns, dir string) storage.Store {
		st, err := storage.Open(cfg.DataDir, ns, dir, cfg.Settings.StorageDriver)
		if err != nil {
			log.Printf("storage warning: %v", err)
		}
		return st
	}
	// dataDir/ns is the default layout for top-level modules
	openNS := func(ns string) storage.Store { return open(ns, cfg.SubDir(ns)) }

	prov := provider.New(openNS("providers"), vault)
	roles := role.New(openNS("roles"))
	convs := conversation.New(openNS("conversations"))
	skills := skill.New(openNS("skills"))
	knowBase := cfg.SubDir("knowledge")
	know := knowledge.New(
		open("knowledge/collections", filepath.Join(knowBase, "collections")),
		open("knowledge/index", filepath.Join(knowBase, "index")),
		knowBase, cfg.DataDir, cfg.Settings.StorageDriver, retr, prov)
	memBase := cfg.SubDir("memory")
	// L1 entities and their vectors live in separate namespaces for the same
	// reason knowledge does: vectors must not bloat the memory files.
	mem := memory.New(openNS("memories"), open("memory/index", filepath.Join(memBase, "index")),
		memBase, retr, prov, cfg.Settings.MemoryAutoExtract)
	mem.SetConversationSource(convs)
	meth := methodology.New(openNS("methodologies"))
	// Pick up any SKILL.md dropped into the scan directories since the last
	// boot (DESIGN §14#3). A failure here must not stop the server: the library
	// is still usable with its built-ins.
	scanDirs := skill.Dirs(cfg.DataDir)
	if res, err := skills.Scan(scanDirs); err != nil {
		log.Printf("skill scan warning: %v", err)
	} else if res.Added > 0 || res.Update > 0 {
		log.Printf("技能扫描：新增 %d，更新 %d（目录 %v）", res.Added, res.Update, scanDirs)
	}
	// methodology.json is the compatibility exchange file (DESIGN §14#9); it is
	// only imported when the namespaced store is empty, so a normal boot does
	// not duplicate entries.
	if len(meth.List()) == 0 {
		if n, err := meth.ImportFile(cfg.DataDir); err != nil {
			log.Printf("methodology import warning: %v", err)
		} else if n > 0 {
			log.Printf("已从 methodology.json 导入 %d 条方法论", n)
		}
	}
	// Software control (DESIGN §9.1, the "first capability"). It ships disabled:
	// the operator must flip the module switch before a single window can be
	// enumerated, and the built-in deny list is enforced regardless of that switch.
	jobs := job.New(openNS("jobs"))
	ing := ingest.New(know, jobs, filepath.Join(knowBase, "stage"))
	cons := consolidate.New(mem, prov, jobs)
	ex := extract.New(mem, prov, extract.DefaultGates())
	jobs.RegisterWorker(ing)
	jobs.RegisterWorker(cons)
	aud := audit.New(cfg.DataDir)
	ctrl := control.New(aud)
	// Memory and behaviour rules both end up inside the system prompt, so "who
	// changed this, and when" has to be answerable afterwards: deletions,
	// invalidations and rule edits all land in the shared audit log. Without
	// it, an assistant that suddenly answers differently leaves no trace, and
	// the only copy of what was lost is the one that was just destroyed.
	mem.SetAudit(aud)
	meth.SetAudit(aud)

	reg := registry.New()
	for _, t := range builtin.NewBuiltins() {
		reg.Register(t)
	}
	builtin.InjectMemorySearcher(reg, mem.Searcher())
	builtin.InjectAudit(reg, aud)
	// Software without an API/CLI: REA records the few steps that work once, so the
	// model stops rediscovering the same window controls on every turn.
	rea.New(filepath.Join(cfg.DataDir, "rea")).RegisterTools(reg)
	// Control capabilities join the same registry, so a role can grant them
	// like any other tool. They refuse at execution time while the module is
	// off, and the orchestrator's gate treats every control_* name as a
	// non-read-only tool (strictest tier blocks them before Execute).
	ctrl.RegisterTools(reg)

	// Environment probe and briefs: the two ends of the "one sentence and we
	// start" flow (DESIGN §9.3). The probe is how the agent finds out whether
	// this machine can do the job at all; the briefs are where the user's ideas
	// go while whatever is missing gets installed or worked out. Neither is
	// behind a module switch — one is a read of directory names, the other is a
	// folder of Markdown files the user can open themselves.
	envprobe.RegisterTools(reg)
	briefs, err := brief.New(filepath.Join(cfg.DataDir, "briefs"))
	if err != nil {
		log.Printf("简报目录不可用：%v", err)
		briefs, _ = brief.New("")
	} else {
		briefs.RegisterTools(reg)
	}

	// Self-built tools (DESIGN §5 tool/sandbox + mcp, §8 /api/v1/scripts and
	// /api/v1/mcp/servers). Both are mounted into the same registry as the
	// built-ins, so a script tool is callable from the same tool loop and shows
	// up in the role tool picker without any special casing.
	scripts := sandbox.NewService(openNS("scripts"), reg, aud)
	mcpMgr := mcp.NewManager(openNS("mcp_servers"), vault, reg, aud)

	// Module registry (DESIGN §5): every subsystem is registered so its
	// lifecycle runs and its disabled-set entry survives a restart. Only the
	// capabilities a user can meaningfully decide about are listed (`Hidden` is
	// false); the rest — the orchestrator core, the storage backend, the
	// per-provider and per-builtin-tool entries — stay registered but out of
	// the UI. Listing them produced switches that flipped without changing
	// anything a user could observe, which reads as a bug rather than a knob.
	mods := module.NewRegistry()
	for _, t := range reg.List() {
		mods.RegisterWithMeta(noopModule{id: "tool." + t.Name}, module.Meta{
			ID: "tool." + t.Name, Name: t.Name, Kind: module.KindTool,
			Description: t.Description, Builtin: true, Hidden: true,
		})
	}
	mods.RegisterWithMeta(noopModule{id: "runtime.orchestrator"}, module.Meta{
		ID: "runtime.orchestrator", Name: "智能体编排", Kind: module.KindRuntime,
		Description: "对话/任务编排、工具循环与容灾切换", Builtin: true, Hidden: true,
	})
	mods.RegisterWithMeta(noopModule{id: "storage." + cfg.Settings.StorageDriver}, module.Meta{
		ID: "storage." + cfg.Settings.StorageDriver, Name: "存储（" + cfg.Settings.StorageDriver + "）",
		Kind: module.KindStorage, Description: "会话、角色、资料库与记忆的持久化后端",
		Builtin: true, Hidden: true,
	})
	// 资料库检索 is genuinely optional, so it keeps a switch: with nothing
	// ingested it is a no-op, and with documents it only decides whether
	// answers may cite them. Off is usable, on is better — exactly what a
	// switch is for (DESIGN §17 原则五).
	mods.RegisterWithMeta(noopModule{id: "rag.knowledge"}, module.Meta{
		ID: "rag.knowledge", Name: "资料库检索", Kind: module.KindRAG,
		Description: "回答时引用你上传的资料。关闭后资料仍在，只是不再参与回答",
		Builtin:     true,
	})
	// 长期记忆 has no switch on purpose (DESIGN §17 原则五): memory is the point
	// of the product, so "要不要开" is not a real question. Asking it only
	// offers the user a one-click way to break the thing they came for, and a
	// fresh install that is quietly stateless reads as broken rather than
	// conservative. The entry stays registered so the disabled set and the
	// lifecycle still work for anyone hand-editing settings.json.
	mods.RegisterWithMeta(noopModule{id: "memory.engine"}, module.Meta{
		ID: "memory.engine", Name: "长期记忆", Kind: module.KindMemory,
		Description: "跨会话记住你的偏好与事实，由巩固任务自动整理",
		Builtin:     true, Hidden: true,
	})
	// Software control gets a real lifecycle object rather than a no-op wrapper:
	// its switch is the difference between "the agent can drive the desktop"
	// and "it cannot", so the state has to survive a restart and be readable
	// back into the service at boot.
	//
	// It is Hidden here on purpose: the switch belongs on the software-control
	// page, where it sits next to what the capability is for and what it does
	// when on. Two switches for one capability is how a UI ends up showing "on"
	// in one place and "off" in another.
	ctrlMod := &controlModule{svc: ctrl}
	mods.RegisterWithMeta(ctrlMod, module.Meta{
		ID: "control.desktop", Name: "软件控制（桌面）", Kind: module.KindRuntime,
		Description: "替用户操作本机软件把事做完（说一句想要什么，它去那个软件里做出来；仅限登记过的应用）",
		Builtin:     true, Hidden: true,
	})
	for _, p := range prov.List() {
		mods.RegisterWithMeta(noopModule{id: "provider." + p.ID}, module.Meta{
			ID: "provider." + p.ID, Name: p.Name, Kind: module.KindProvider,
			Description: "LLM 服务商接入（多 Key 轮询）", Builtin: false, Hidden: true,
		})
	}
	mods.RestoreDisabled(cfg.Settings.DisabledModules)
	// Run the module lifecycle so a real plugin's Init/Start actually happens
	// (currently every registered module is a no-op wrapper, but a future one
	// must not be silently skipped).
	if err := mods.InitAll(); err != nil {
		log.Printf("modules: init failed: %v", err)
	}
	// Init runs after RestoreDisabled so the control service adopts the
	// persisted switch state; starting it earlier would let the default (off)
	// silently overwrite a previously enabled setting.
	if err := mods.StartAll(); err != nil {
		log.Printf("modules: start failed: %v", err)
	}
	// Mirror the persisted switch into the control service, and keep mirroring
	// it on every later toggle. Without the mirror the UI could show "软件控制:
	// 开启" while the service still refuses every action — the worst kind of
	// disagreement, because it looks like a bug in the control code.
	//
	// The initial state needs care: module.IsEnabled treats "absent from the
	// disabled set" as on, which is right for ordinary modules but wrong here.
	// Software control ships *disabled* (DESIGN §9.1), so "not mentioned in
	// settings.json" has to mean off. Reading it through IsEnabled would hand
	// every fresh install a live desktop-control capability.
	mods.OnChange(func(id string, enabled bool) {
		if id == ctrlMod.ID() {
			ctrl.SetEnabled(enabled)
		}
	})
	ctrl.SetEnabled(controlEnabledAtBoot(cfg.Settings.DisabledModules, ctrlMod.ID()))

	orch := orchestrator.New(prov, convs, roles, reg, cfg, aud)
	orch.Memory = mem
	orch.Knowledge = know
	orch.Extractor = ex
	orch.Skills = skills
	orch.Methodologies = meth
	orch.Modules = mods
	// Every control_* tool call is checked here before it runs. The service's
	// own Run re-checks the deny list, allow list and capability, so this is the
	// early refusal that gives the model a usable explanation instead of a
	// surprise at execution time.
	orch.ControlGate = ctrl.ToolGate

	h := handler.New(cfg, prov, roles, convs, skills, know, mem, meth, ctrl, reg, jobs, ing, orch, ex, cons, server.StaticHandler())
	h.Modules = mods
	h.Briefs = briefs
	h.Scripts = scripts
	h.MCP = mcpMgr
	orch.OnModulesChanged = func() {
		disabled := mods.DisabledIDs()
		if err := cfg.UpdateSettings(func(s *config.Settings) { s.DisabledModules = disabled }); err != nil {
			log.Printf("modules: persist switches failed: %v", err)
		}
	}
	srv := server.New(h, cfg.Addr)

	// Memory consolidation used to have exactly one trigger: the button in the UI.
	// "自动巩固" therefore never ran — L1 kept accumulating while the L2 scenes and
	// the L3 persona stayed frozen. Check every half hour and only act once 20 new
	// L1 facts have appeared since the last completed run (each run costs one or
	// two LLM calls, so the threshold is what bounds the spend).
	go h.AutoConsolidate(30*time.Minute, 20)

	go func() {
		log.Printf("AgentHub 已启动: http://%s", cfg.Addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("server: %v", err)
		}
	}()

	// System tray: show that the service is running, right-click to open UI or quit.
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)

	var exitOnce sync.Once
	exited := false
	onExit := func() {
		exitOnce.Do(func() {
			log.Println("正在关闭...")
			_ = srv.Shutdown(context.Background())
			mcpMgr.StopAll()
			mods.StopAll()
			storage.CloseShared()
			exited = true
		})
	}

	onReady := func() {
		// Generate a simple 32x32 blue circle icon.
		icon := generateTrayIcon()
		systray.SetIcon(icon)
		systray.SetTitle("AgentHub")
		systray.SetTooltip("AgentHub 智能体代理")
		mOpen := systray.AddMenuItem("打开主界面", "在浏览器中打开")
		systray.AddSeparator()
		mQuit := systray.AddMenuItem("退出", "停止服务")
		go func() {
			for range mOpen.ClickedCh {
				exec.Command("cmd", "/c", "start", "http://"+cfg.Addr).Start()
			}
		}()
		go func() {
			<-mQuit.ClickedCh
			systray.Quit()
		}()
	}

	systray.Run(onReady, onExit)
	// Two ways out of systray.Run, and they need opposite handling:
	//   - the user chose 退出: onExit already ran, so the process must actually end.
	//     Waiting on a signal here used to leave a headless zombie (no tray, no
	//     window, closed database) that had to be killed by hand;
	//   - the tray could not start (no interactive session, access denied): Run
	//     returns without calling onExit, and the service must keep serving, so
	//     fall through and wait for a signal like before.
	if exited {
		return
	}
	<-stop
	onExit()
}

// generateTrayIcon draws a 32x32 blue circle with a white "A" shape.
func generateTrayIcon() []byte {
	const size = 32
	img := image.NewRGBA(image.Rect(0, 0, size, size))
	// Fill with blue circle.
	center := size / 2
	radius := size/2 - 2
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			dx, dy := x-center, y-center
			if dx*dx+dy*dy <= radius*radius {
				img.Set(x, y, color.RGBA{64, 130, 247, 255})
			}
		}
	}
	// Simple white "A" shape (triangle).
	for y := 8; y < 24; y++ {
		half := (y - 8) / 2
		for x := center - half; x <= center+half; x++ {
			if x >= 0 && x < size && y >= 0 && y < size {
				img.Set(x, y, color.White)
			}
		}
	}
	// Crossbar of A.
	for x := center - 4; x <= center+4; x++ {
		img.Set(x, 18, color.White)
	}
	var buf bytes.Buffer
	png.Encode(&buf, img)
	return buf.Bytes()
}

// noopModule represents a subsystem that is wired directly into the runtime
// (no separate lifecycle object). It exists so the UI can still show and
// switch the feature.
type noopModule struct{ id string }

func (m noopModule) ID() string   { return m.id }
func (m noopModule) Init() error  { return nil }
func (m noopModule) Start() error { return nil }
func (m noopModule) Stop() error  { return nil }

// controlModule adapts the control service to the module lifecycle so the UI
// switch is the single source of truth for "may the agent drive the desktop".
//
// It deliberately does not persist anything itself: the module registry already
// persists the disabled set into settings.json, and writing a second copy here
// would give two sources of truth that could disagree after a partial failure.
type controlModule struct{ svc *control.Service }

func (m *controlModule) ID() string { return "control.desktop" }

// Start adopts the persisted switch state. It is the last lifecycle hook to run
// on purpose — by then RestoreDisabled has already applied the user's setting.
func (m *controlModule) Start() error {
	if _, supported := m.svc.Platform(); !supported {
		log.Printf("软件控制：当前平台不支持（%s），模块已注册但不可用",
			mustPlatform(m.svc))
	}
	return nil
}

func (m *controlModule) Init() error { return nil }
func (m *controlModule) Stop() error { m.svc.SetEnabled(false); return nil }

func mustPlatform(s *control.Service) string {
	p, _ := s.Platform()
	return p
}

// controlEnabledAtBoot reports whether software control should be on at boot,
// given the persisted disabled-module list.
//
// Software control is on by default: it is a first-class feature of this build
// (the user opted in at the product level). The settings file records only the
// disabled set, so the safe default for a missing entry is *on*; an explicit
// entry in the disabled list is the only thing that turns it off.
func controlEnabledAtBoot(disabled []string, id string) bool {
	for _, d := range disabled {
		if d == id {
			return false // explicitly disabled
		}
	}
	return true // enabled by default unless explicitly switched off
}
