// Package sandbox runs user-authored JS/Python tools in a child process with a
// timeout (process isolation, not a security sandbox — see DESIGN §10.3). Scripts
// are disabled by default; an operator must enable each explicitly (DESIGN §5
// tool/sandbox, §10 脚本工具默认关闭).
//
// The Service owns persistence (scripts/{id}.json, DESIGN §7) and the
// registry mount: only an enabled, admission-passed script is registered as a
// callable tool, so a disabled script is invisible both to the role tool picker
// and to the model's tool list.
package sandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/agenthub/agent-proxy/internal/audit"
	"github.com/agenthub/agent-proxy/internal/storage"
	"github.com/agenthub/agent-proxy/internal/tool"
	"github.com/agenthub/agent-proxy/internal/tool/registry"
	"github.com/agenthub/agent-proxy/internal/tool/schema"
	"github.com/agenthub/agent-proxy/internal/util"
)

// Supported runtimes.
const (
	RuntimeJS     = "js"
	RuntimePython = "python"
)

// DefaultTimeoutSeconds bounds a single script run.
const DefaultTimeoutSeconds = 30

// admissionProbe is the input used for the triple-gate smoke run.
const admissionProbe = "agenthub-admission-probe"

// ScriptTool is a script-backed tool.
//
// declared/argList are attached from the persisted entity by buildTool rather
// than being stored on the JSON record, so the on-disk shape stays exactly
// DESIGN §7's script record.
type ScriptTool struct {
	ID              string
	ToolName        string
	ToolDescription string
	Runtime         string // js | python
	Code            string
	Enabled         bool
	// TimeoutSeconds overrides DefaultTimeoutSeconds when > 0.
	TimeoutSeconds int

	declared map[string]interface{}
	argList  []string
}

// ScriptToolEntity is the persisted script tool record (DESIGN §7 scripts/{id}.json).
type ScriptToolEntity struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Runtime     string `json:"runtime"`
	Code        string `json:"code"`
	Enabled     bool   `json:"enabled"`
	// TimeoutSeconds bounds one execution; 0 means DefaultTimeoutSeconds.
	TimeoutSeconds int `json:"timeoutSeconds,omitempty"`
	// Schema is the declared parameter schema. When empty the fixed
	// {input: string} schema is used.
	Schema map[string]interface{} `json:"schema,omitempty"`
	// Args maps a schema property name to the AGENTHUB_ARG_<NAME> env var /
	// argv[1] slot that carries its value.
	Args []string `json:"args,omitempty"`
	// Admission records the last triple-gate verdict (DESIGN §2 risk table).
	Admission *schema.AdmissionResult `json:"admission,omitempty"`
	LastRun   *RunRecord              `json:"lastRun,omitempty"`
	CreatedAt int64                   `json:"createdAt"`
	UpdatedAt int64                   `json:"updatedAt"`
}

// RunRecord is the outcome of the most recent execution, surfaced in the UI so
// a broken script is diagnosable without reading server logs.
type RunRecord struct {
	At         int64  `json:"at"`
	OK         bool   `json:"ok"`
	DurationMs int64  `json:"durationMs"`
	Output     string `json:"output,omitempty"`
	Error      string `json:"error,omitempty"`
}

// NewScriptTool builds a runnable ScriptTool from an entity.
func NewScriptTool(e ScriptToolEntity) *ScriptTool {
	return &ScriptTool{
		ID:              e.ID,
		ToolName:        e.Name,
		ToolDescription: e.Description,
		Runtime:         e.Runtime,
		Code:            e.Code,
		Enabled:         e.Enabled,
		TimeoutSeconds:  e.TimeoutSeconds,
	}
}

func (s *ScriptTool) Name() string        { return s.ToolName }
func (s *ScriptTool) Description() string { return s.ToolDescription }

// Schema returns the declared schema, or the fixed {input: string} shape.
func (s *ScriptTool) Schema() map[string]interface{} {
	if len(s.declared) > 0 {
		return s.declared
	}
	return defaultSchema()
}

func defaultSchema() map[string]interface{} {
	return map[string]interface{}{
		"type":       "object",
		"properties": map[string]interface{}{"input": map[string]interface{}{"type": "string", "description": "传给脚本的输入文本"}},
	}
}

func (s *ScriptTool) timeout() time.Duration {
	secs := s.TimeoutSeconds
	if secs <= 0 {
		secs = DefaultTimeoutSeconds
	}
	if secs > 600 {
		secs = 600 // hard ceiling: a script must never pin a worker forever
	}
	return time.Duration(secs) * time.Second
}

// interpreter resolves the runtime name to an executable, honouring an operator
// override. Returns an error naming the fix rather than "exec: not found".
func (s *ScriptTool) interpreter() (string, string, error) {
	envKey := "AGENTHUB_BIN_JS"
	bin := "node"
	ext := ".js"
	if strings.EqualFold(s.Runtime, RuntimePython) {
		envKey, bin, ext = "AGENTHUB_BIN_PYTHON", "python", ".py"
	}
	if custom := strings.TrimSpace(os.Getenv(envKey)); custom != "" {
		bin = custom
	}
	resolved, err := exec.LookPath(bin)
	if err != nil {
		return "", ext, fmt.Errorf("找不到 %s 解释器 %q：安装后重试，或设置环境变量 %s 指定路径", s.Runtime, bin, envKey)
	}
	return resolved, ext, nil
}

// Execute runs the script in a child process. Input is exposed both as the
// AGENTHUB_INPUT env var and on stdin (scripts written either way work without
// ceremony); each declared arg is passed as AGENTHUB_ARG_<NAME>. stdout is
// sanitized and returned.
func (s *ScriptTool) Execute(ctx context.Context, args map[string]interface{}) (string, error) {
	if !s.Enabled {
		return "", errors.New("script tool is disabled (enable it explicitly)")
	}
	input, _ := args["input"].(string)

	bin, ext, err := s.interpreter()
	if err != nil {
		return "", err
	}
	f, err := os.CreateTemp("", "agenthub-script-*"+ext)
	if err != nil {
		return "", err
	}
	defer os.Remove(f.Name())
	if _, err := f.WriteString(s.Code); err != nil {
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}

	cctx, cancel := context.WithTimeout(ctx, s.timeout())
	defer cancel()

	env := append(os.Environ(), "AGENTHUB_INPUT="+input)
	// Declared args travel as env vars so a script never has to parse argv.
	for _, k := range s.argNames() {
		v := ""
		if raw, ok := args[k]; ok {
			v = fmt.Sprintf("%v", raw)
		}
		env = append(env, "AGENTHUB_ARG_"+strings.ToUpper(strings.ReplaceAll(k, "-", "_"))+"="+v)
	}

	cmd := exec.CommandContext(cctx, bin, f.Name())
	cmd.Env = env
	cmd.Stdin = strings.NewReader(input)
	// Cap the buffers at ~4x the output limit: without a cap, `cat /dev/urandom`
	// grows the buffer until the process is OOM-killed instead of hitting the
	// timeout.
	out := &Limited{Cap: MaxCapturedBytes}
	serr := &Limited{Cap: 8 << 10}
	cmd.Stdout = out
	cmd.Stderr = serr

	runErr := cmd.Run()
	if cctx.Err() == context.DeadlineExceeded {
		return "", fmt.Errorf("脚本执行超时（%s）", s.timeout())
	}
	if runErr != nil {
		msg := strings.TrimSpace(serr.String())
		if msg == "" {
			msg = runErr.Error()
		}
		return "", fmt.Errorf("脚本错误: %s", schema.Clamp(msg))
	}
	return schema.Clamp(out.String()), nil
}

func (s *ScriptTool) argNames() []string { return s.argList }

var _ tool.Tool = (*ScriptTool)(nil)

// Limited is a bounded io.Writer: writes past Cap are dropped, never fatal.
type Limited struct {
	Cap int
	buf bytes.Buffer
}

func (l *Limited) Write(p []byte) (int, error) {
	if room := l.Cap - l.buf.Len(); room > 0 {
		if len(p) > room {
			l.buf.Write(p[:room])
			return len(p), nil // pretend success so the child is not killed by EPIPE
		}
		l.buf.Write(p)
	}
	return len(p), nil
}

func (l *Limited) String() string { return l.buf.String() }

// MaxCapturedBytes bounds stdout captured from a script process.
const MaxCapturedBytes = 4 * schema.MaxOutputBytes

// ---- service ----

// Service persists script tools and mounts enabled ones into the registry.
type Service struct {
	store storage.Store
	reg   *registry.Registry
	aud   *audit.Audit

	mu    sync.RWMutex
	cache map[string]*ScriptToolEntity
}

// NewService constructs the script tool service and registers every script that
// is both enabled and previously admitted. A script that fails to register
// (e.g. its interpreter disappeared) is left stored-but-disabled with the
// reason recorded, never silently callable.
func NewService(store storage.Store, reg *registry.Registry, aud *audit.Audit) *Service {
	s := &Service{store: store, reg: reg, aud: aud, cache: map[string]*ScriptToolEntity{}}
	s.load()
	return s
}

func (s *Service) load() {
	ids, err := s.store.IDs()
	if err != nil {
		return
	}
	for _, id := range ids {
		var e ScriptToolEntity
		if err := s.store.Load(id, &e); err != nil {
			continue
		}
		if e.ID == "" {
			e.ID = id
		}
		if e.TimeoutSeconds <= 0 {
			e.TimeoutSeconds = DefaultTimeoutSeconds
		}
		s.cache[id] = &e
		if e.Enabled {
			s.mount(&e)
		}
	}
}

// List returns all script tools, sorted by name.
func (s *Service) List() []*ScriptToolEntity {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*ScriptToolEntity, 0, len(s.cache))
	for _, e := range s.cache {
		cp := *e
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Get returns one script tool.
func (s *Service) Get(id string) (*ScriptToolEntity, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, ok := s.cache[id]
	if !ok {
		return nil, fmt.Errorf("script tool %s not found", id)
	}
	cp := *e
	return &cp, nil
}

// Create persists a new script tool. A brand-new tool is stored DISABLED on
// purpose (DESIGN §10): running arbitrary code must be an explicit operator
// decision, not a side effect of saving a draft.
func (s *Service) Create(e *ScriptToolEntity) error {
	if err := normalise(e); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if e.ID == "" {
		e.ID = util.NewID()
	}
	if e.TimeoutSeconds <= 0 {
		e.TimeoutSeconds = DefaultTimeoutSeconds
	}
	now := util.NowUnix()
	e.CreatedAt, e.UpdatedAt = now, now
	if e.Enabled {
		e.Enabled = false // §10: creation never implies execution
	}
	if err := s.store.Save(e.ID, e); err != nil {
		return err
	}
	cp := *e
	s.cache[e.ID] = &cp
	return nil
}

// Update replaces a script tool's definition. Changing code invalidates the
// previous admission verdict: the new body has not passed the gate yet, so the
// tool drops back to disabled rather than running unvetted code.
//
// Fields the caller does not send are preserved rather than reset. A PATCH that
// only changes the description must not silently wipe the verdict and switch a
// running tool off — that is exactly the invisible-failure mode this codebase
// treats as a bug.
func (s *Service) Update(id string, e *ScriptToolEntity) error {
	if err := normalise(e); err != nil {
		return err
	}
	s.mu.Lock()
	old, ok := s.cache[id]
	if !ok {
		s.mu.Unlock()
		return fmt.Errorf("script tool %s not found", id)
	}
	e.ID = id
	e.CreatedAt = old.CreatedAt
	e.UpdatedAt = util.NowUnix()
	e.LastRun = old.LastRun
	codeChanged := e.Code != old.Code
	if codeChanged {
		e.Admission = nil
		e.Enabled = false
	} else {
		// Same code: carry the lifecycle state forward unless the caller
		// explicitly set it via SetEnabled.
		e.Admission = old.Admission
		e.Enabled = old.Enabled
	}
	if e.TimeoutSeconds <= 0 {
		e.TimeoutSeconds = DefaultTimeoutSeconds
	}
	if err := s.store.Save(id, e); err != nil {
		s.mu.Unlock()
		return err
	}
	cp := *e
	s.cache[id] = &cp
	s.mu.Unlock()

	if !e.Enabled {
		s.reg.UnregisterOwner(id)
	} else {
		s.mount(e)
	}
	return nil
}

// Delete removes a script tool and unregisters its tools.
func (s *Service) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.cache[id]; !ok {
		return fmt.Errorf("script tool %s not found", id)
	}
	_ = s.store.Remove(id)
	delete(s.cache, id)
	s.reg.UnregisterOwner(id)
	if s.aud != nil {
		s.aud.Log("script_tool_deleted", "", map[string]string{"id": id})
	}
	return nil
}

// Admit runs the triple gate (contract → consistency → smoke) against the
// stored definition. The verdict is persisted so the UI can show why a tool is
// still disabled after a save.
func (s *Service) Admit(ctx context.Context, id string) (*schema.AdmissionResult, error) {
	e, err := s.Get(id)
	if err != nil {
		return nil, err
	}
	st := buildTool(e, true)

	// The smoke probe runs once and its output is recorded: running it twice
	// would double the wall time on a slow script and double the audit noise.
	probeOut := ""
	probeMS := int64(0)
	res := schema.Admit(ctx, schema.Candidate{
		Name:        e.Name,
		Description: e.Description,
		Schema:      st.Schema(),
		Smoke: func(c context.Context) error {
			// Probe with a recognisable input so the smoke run also proves the
			// input channel works, not merely that the process starts.
			start := time.Now()
			out, err := st.Execute(c, map[string]interface{}{"input": admissionProbe})
			probeMS = time.Since(start).Milliseconds()
			if err != nil {
				return err
			}
			probeOut = out
			return nil
		},
	})

	e.Admission = &res
	rec := RunRecord{At: util.NowUnix(), OK: res.OK, DurationMs: probeMS}
	if res.OK {
		rec.Output = probeOut
	} else {
		rec.Error = res.Message
	}
	e.LastRun = &rec

	s.mu.Lock()
	e.UpdatedAt = util.NowUnix()
	if err := s.store.Save(id, e); err != nil {
		s.mu.Unlock()
		return &res, err
	}
	cp := *e
	s.cache[id] = &cp
	s.mu.Unlock()

	if s.aud != nil {
		s.aud.Log("script_tool_admission", "", map[string]string{
			"id": id, "name": e.Name, "gate": res.Gate, "ok": boolStr(res.OK), "message": res.Message,
		})
	}
	return &res, nil
}

// SetEnabled turns a script tool on or off. Enabling requires a passing
// admission verdict: an unadmitted tool would otherwise be callable without ever
// having been proven to run.
func (s *Service) SetEnabled(id string, enabled bool) (*ScriptToolEntity, error) {
	s.mu.Lock()
	e, ok := s.cache[id]
	if !ok {
		s.mu.Unlock()
		return nil, fmt.Errorf("script tool %s not found", id)
	}
	if enabled && (e.Admission == nil || !e.Admission.OK) {
		s.mu.Unlock()
		gate := "未校验"
		if e.Admission != nil {
			gate = e.Admission.Gate + " 关失败：" + e.Admission.Message
		}
		return nil, fmt.Errorf("启用前须通过准入三关（当前 %s）。先执行「准入校验」", gate)
	}
	prev := e.Enabled
	e.Enabled = enabled
	e.UpdatedAt = util.NowUnix()
	err := s.store.Save(id, e)
	cp := *e
	s.cache[id] = &cp
	s.mu.Unlock()

	if err != nil {
		return nil, err
	}
	if !enabled {
		s.reg.UnregisterOwner(id)
	} else if !prev {
		s.mount(&cp)
	}
	if s.aud != nil {
		s.aud.Log("script_tool_toggled", "", map[string]string{"id": id, "enabled": boolStr(enabled)})
	}
	return &cp, nil
}

// Run executes a script tool outside the chat loop (the UI's "test run" button).
func (s *Service) Run(ctx context.Context, id, input string) (*RunRecord, error) {
	e, err := s.Get(id)
	if err != nil {
		return nil, err
	}
	st := buildTool(e, true)
	start := time.Now()
	out, runErr := st.Execute(ctx, map[string]interface{}{"input": input})
	rec := &RunRecord{
		At: util.NowUnix(), OK: runErr == nil,
		DurationMs: time.Since(start).Milliseconds(), Output: out,
	}
	if runErr != nil {
		rec.Error = runErr.Error()
	}
	s.mu.Lock()
	e.LastRun = rec
	_ = s.store.Save(id, e)
	cp := *e
	s.cache[id] = &cp
	s.mu.Unlock()

	if s.aud != nil {
		s.aud.Log("script_tool_run", "", map[string]string{
			"id": id, "name": e.Name, "ok": boolStr(rec.OK), "error": rec.Error,
		})
	}
	return rec, nil
}

// mount registers an enabled script as a callable tool.
func (s *Service) mount(e *ScriptToolEntity) {
	s.reg.RegisterDynamic(registry.SourceScript, e.ID, buildTool(e, true))
}

func buildTool(e *ScriptToolEntity, forceEnabled bool) *ScriptTool {
	st := NewScriptTool(*e)
	if forceEnabled {
		st.Enabled = true
	}
	st.argList = e.Args
	if len(e.Schema) > 0 {
		st.declared = e.Schema
	}
	return st
}

func normalise(e *ScriptToolEntity) error {
	e.Name = strings.TrimSpace(e.Name)
	e.Description = strings.TrimSpace(e.Description)
	e.Runtime = strings.ToLower(strings.TrimSpace(e.Runtime))
	e.Code = strings.ReplaceAll(e.Code, "\r\n", "\n")
	if e.Name == "" {
		return errors.New("工具名不能为空")
	}
	if e.Code == "" {
		return errors.New("脚本代码不能为空")
	}
	switch e.Runtime {
	case RuntimeJS, RuntimePython:
	case "":
		return errors.New("请选择运行时：js 或 python")
	default:
		return fmt.Errorf("不支持的运行时 %q（仅支持 js / python）", e.Runtime)
	}
	if len(e.Schema) > 0 {
		if _, err := schema.Sanitize(e.Schema); err != nil {
			return fmt.Errorf("参数 schema 不合法: %w", err)
		}
	}
	if e.TimeoutSeconds < 0 {
		return errors.New("超时时间不能为负")
	}
	return nil
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// RuntimeBinaries reports which interpreters are actually present, so the UI
// can grey out a runtime the machine cannot run instead of letting the user save
// a tool that will never execute.
func RuntimeBinaries() map[string]bool {
	out := map[string]bool{}
	for rt, env := range map[string]string{RuntimeJS: "AGENTHUB_BIN_JS", RuntimePython: "AGENTHUB_BIN_PYTHON"} {
		bin := os.Getenv(env)
		if bin == "" {
			bin = "node"
			if rt == RuntimePython {
				bin = "python"
			}
		}
		_, err := exec.LookPath(bin)
		out[rt] = err == nil
	}
	return out
}

// SuggestedExt returns the temp-file extension used for a runtime (used by the
// UI template hint).
func SuggestedExt(rt string) string {
	if strings.EqualFold(rt, RuntimePython) {
		return ".py"
	}
	return ".js"
}
