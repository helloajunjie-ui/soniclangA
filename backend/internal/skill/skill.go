// Package skill implements the skill library: a reusable unit that binds an
// instruction set + tools + knowledge collections (DESIGN §5 skill / §7 / §9).
// Skills are layered on top of roles to specialise behaviour.
//
// Two ingestion channels are supported (DESIGN §14#3):
//   - UI/API built-ins: CRUD through the Service, seeded on first run.
//   - SKILL.md directory scan: a file with YAML-lite frontmatter
//     (name/description/tools/knowledge/tags) dropped into one of the scan
//     directories is picked up on boot or via POST /api/v1/skills/scan.
//     Scanning is idempotent: the source path is the dedup key, so re-scanning
//     updates the existing entry instead of creating duplicates.
package skill

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/agenthub/agent-proxy/internal/storage"
	"github.com/agenthub/agent-proxy/internal/util"
)

// Skill is the persisted skill entity.
type Skill struct {
	ID                     string   `json:"id"`
	Name                   string   `json:"name"`
	Description            string   `json:"description,omitempty"`
	Instructions           string   `json:"instructions"`
	Tools                  []string `json:"tools,omitempty"`
	KnowledgeCollectionIds []string `json:"knowledgeCollectionIds,omitempty"`
	Tags                   []string `json:"tags,omitempty"`
	Enabled                bool     `json:"enabled"`
	Version                int      `json:"version"`
	Builtin                bool     `json:"builtin"`
	SourcePath             string   `json:"sourcePath,omitempty"`
	CreatedAt              int64    `json:"createdAt"`
	UpdatedAt              int64    `json:"updatedAt"`
}

// Service manages skills.
type Service struct {
	store storage.Store
	mu    sync.RWMutex
	cache map[string]*Skill
}

// New constructs the skill Service and seeds built-ins when empty.
func New(store storage.Store) *Service {
	s := &Service{store: store, cache: map[string]*Skill{}}
	s.load()
	if len(s.cache) == 0 {
		s.seed()
	}
	return s
}

func (s *Service) load() {
	ids, err := s.store.IDs()
	if err != nil {
		return
	}
	for _, id := range ids {
		var sk Skill
		if err := s.store.Load(id, &sk); err == nil {
			// Skills created before the Enabled field existed would otherwise
			// be silently switched off by the zero value.
			if !sk.Enabled {
				sk.Enabled = true
			}
			s.cache[id] = &sk
		}
	}
}

// List returns all skills sorted by name for stable UI ordering.
func (s *Service) List() []*Skill {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Skill, 0, len(s.cache))
	for _, sk := range s.cache {
		cp := *sk
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name == out[j].Name {
			return out[i].ID < out[j].ID
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// ListEnabled returns the skills that are switched on.
func (s *Service) ListEnabled() []*Skill {
	out := make([]*Skill, 0)
	for _, sk := range s.List() {
		if sk.Enabled {
			out = append(out, sk)
		}
	}
	return out
}

// Get returns a skill by id.
func (s *Service) Get(id string) (*Skill, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sk, ok := s.cache[id]
	if !ok {
		return nil, fmt.Errorf("skill %s not found", id)
	}
	cp := *sk
	return &cp, nil
}

// GetMany resolves a list of ids, skipping unknown ones instead of failing the
// whole turn — a deleted skill must not break an existing role.
func (s *Service) GetMany(ids []string) []*Skill {
	out := make([]*Skill, 0, len(ids))
	for _, id := range ids {
		if sk, err := s.Get(id); err == nil {
			out = append(out, sk)
		}
	}
	return out
}

// Create persists a new skill. New skills are enabled by default: an entry
// that silently never fires is a worse failure than one you have to switch
// off. Import uses createPreserving so a bundle round-trip keeps its switches.
func (s *Service) Create(sk *Skill) error {
	sk.Enabled = true
	return s.create(sk)
}

// createPersisting stores the skill without touching the enabled flag.
func (s *Service) createPreserving(sk *Skill) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.create(sk)
}

func (s *Service) create(sk *Skill) error {
	if sk.ID == "" {
		sk.ID = util.NewID()
	}
	now := time.Now().UnixMilli()
	sk.CreatedAt, sk.UpdatedAt = now, now
	if sk.Version == 0 {
		sk.Version = 1
	}
	if err := s.store.Save(sk.ID, sk); err != nil {
		return err
	}
	cp := *sk
	s.cache[sk.ID] = &cp
	return nil
}

// Update replaces a skill.
func (s *Service) Update(id string, sk *Skill) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	sk.ID = id
	sk.UpdatedAt = time.Now().UnixMilli()
	if err := s.store.Save(id, sk); err != nil {
		return err
	}
	cp := *sk
	s.cache[id] = &cp
	return nil
}

// Delete removes a skill.
func (s *Service) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = s.store.Remove(id)
	delete(s.cache, id)
	return nil
}

// Toggle flips the enabled flag and returns the updated skill.
func (s *Service) Toggle(id string) (*Skill, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sk, ok := s.cache[id]
	if !ok {
		return nil, fmt.Errorf("skill %s not found", id)
	}
	sk.Enabled = !sk.Enabled
	sk.UpdatedAt = time.Now().UnixMilli()
	if err := s.store.Save(id, sk); err != nil {
		sk.Enabled = !sk.Enabled // roll back: the write is the source of truth
		return nil, err
	}
	cp := *sk
	return &cp, nil
}

// Export returns a skill as JSON (schemaVersion wrapper, DESIGN §8).
func (s *Service) Export(id string) ([]byte, error) {
	sk, err := s.Get(id)
	if err != nil {
		return nil, err
	}
	doc := map[string]interface{}{"schemaVersion": 1}
	b, _ := json.Marshal(sk)
	_ = json.Unmarshal(b, &doc)
	return json.MarshalIndent(doc, "", "  ")
}

// ExportAll exports every skill as a single bundle (DESIGN §8 import/export).
func (s *Service) ExportAll() ([]byte, error) {
	doc := map[string]interface{}{
		"schemaVersion": 1,
		"kind":          "skills",
		"exportedAt":    time.Now().UnixMilli(),
		"items":         s.List(),
	}
	return json.MarshalIndent(doc, "", "  ")
}

// Import creates skills from exported JSON. Accepts both a single skill
// document and an {items:[...]} bundle.
func (s *Service) Import(data []byte) ([]*Skill, error) {
	var probe struct {
		Items []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return nil, fmt.Errorf("invalid skill document: %w", err)
	}
	raws := probe.Items
	if len(raws) == 0 {
		raws = []json.RawMessage{data}
	}
	out := make([]*Skill, 0, len(raws))
	for _, raw := range raws {
		var sk Skill
		if err := json.Unmarshal(raw, &sk); err != nil {
			return nil, fmt.Errorf("invalid skill entry: %w", err)
		}
		if strings.TrimSpace(sk.Name) == "" {
			return nil, fmt.Errorf("skill name is required")
		}
		sk.ID = ""
		sk.SourcePath = ""
		// Import preserves the exported enabled flag; Create would force it on.
		if err := s.createPreserving(&sk); err != nil {
			return nil, err
		}
		cp := sk
		out = append(out, &cp)
	}
	return out, nil
}

// Dirs returns the SKILL.md scan directories (DESIGN §14#3): the data dir copy
// plus the per-user one. Directories that do not exist yet are still listed so
// the UI can tell the user where to drop files.
func Dirs(dataDir string) []string {
	out := []string{filepath.Join(dataDir, "skills")}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		out = append(out, filepath.Join(home, ".agenthub", "skills"))
	}
	return out
}

// ScanDir discovers SKILL.md files under dir and parses them into skills
// (not persisted — callers decide whether to import).
func (s *Service) ScanDir(dir string) ([]*Skill, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, fmt.Errorf("empty directory")
	}
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return nil, nil // nothing to scan is not an error
	}
	var out []*Skill
	err = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable entries, keep scanning the rest
		}
		if d.IsDir() {
			// Skip hidden dirs and common noise (node_modules, .git).
			base := d.Name()
			if p != dir && (strings.HasPrefix(base, ".") || base == "node_modules") {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Name() != "SKILL.md" {
			return nil
		}
		sk, perr := ParseSkillMD(p)
		if perr != nil || strings.TrimSpace(sk.Instructions) == "" {
			return nil // a malformed file must not abort the whole scan
		}
		out = append(out, sk)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SourcePath < out[j].SourcePath })
	return out, nil
}

// ScanResult summarises a scan run.
type ScanResult struct {
	Dirs   []string `json:"dirs"`
	Added  int      `json:"added"`
	Update int      `json:"updated"`
	Total  int      `json:"total"`
}

// Scan imports every SKILL.md found under dirs. It is idempotent: skills whose
// sourcePath already exists are updated in place, others are created.
func (s *Service) Scan(dirs []string) (*ScanResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	res := &ScanResult{Dirs: dirs}
	byPath := map[string]*Skill{}
	for _, sk := range s.cache {
		if sk.SourcePath != "" {
			byPath[sk.SourcePath] = sk
		}
	}

	now := time.Now().UnixMilli()
	for _, dir := range dirs {
		found, serr := s.ScanDir(dir)
		if serr != nil {
			return res, serr
		}
		for _, sk := range found {
			if prev, ok := byPath[sk.SourcePath]; ok {
				// Only rewrite when the file actually changed — an unchanged
				// rescan must not bump updatedAt nor clobber the stored copy.
				if prev.Name == sk.Name && prev.Description == sk.Description &&
					prev.Instructions == sk.Instructions && util.SlicesEqual(prev.Tools, sk.Tools) &&
					util.SlicesEqual(prev.KnowledgeCollectionIds, sk.KnowledgeCollectionIds) &&
					util.SlicesEqual(prev.Tags, sk.Tags) {
					continue
				}
				prev.Name = sk.Name
				prev.Description = sk.Description
				prev.Instructions = sk.Instructions
				prev.Tools = sk.Tools
				prev.KnowledgeCollectionIds = sk.KnowledgeCollectionIds
				prev.Tags = sk.Tags
				prev.UpdatedAt = now
				if err := s.store.Save(prev.ID, prev); err != nil {
					return res, err
				}
				res.Update++
				continue
			}
			sk.ID = util.NewID()
			sk.Enabled = true
			sk.Version = 1
			sk.CreatedAt, sk.UpdatedAt = now, now
			if err := s.store.Save(sk.ID, sk); err != nil {
				return res, err
			}
			cp := *sk
			s.cache[sk.ID] = &cp
			byPath[sk.SourcePath] = &cp
			res.Added++
		}
	}
	res.Total = len(s.cache)
	return res, nil
}

// ParseSkillMD parses a SKILL.md file: optional YAML-lite frontmatter delimited
// by `---` followed by the instruction body.
func ParseSkillMD(path string) (*Skill, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var lines []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}

	sk := &Skill{SourcePath: path, Version: 1, Enabled: true}
	body := lines
	if len(lines) > 0 && strings.TrimSpace(lines[0]) == "---" {
		end := -1
		for i := 1; i < len(lines); i++ {
			if strings.TrimSpace(lines[i]) == "---" {
				end = i
				break
			}
		}
		if end > 0 {
			applyFrontmatter(sk, lines[1:end])
			body = lines[end+1:]
		}
	}

	var b strings.Builder
	for _, ln := range body {
		b.WriteString(ln)
		b.WriteString("\n")
	}
	sk.Instructions = strings.TrimSpace(b.String())

	// Fall back to the first markdown heading when frontmatter has no name.
	if sk.Name == "" && len(body) > 0 {
		if strings.HasPrefix(strings.TrimSpace(body[0]), "#") {
			sk.Name = strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(body[0]), "#"))
		}
	}
	if sk.Name == "" {
		sk.Name = filepath.Base(filepath.Dir(path))
	}
	return sk, nil
}

// applyFrontmatter reads a flat `key: value` / `key: [a, b]` / `key:` + `- item`
// frontmatter block. Only keys we understand are honoured; unknown ones are
// ignored rather than rejected, so a richer SKILL.md still imports.
func applyFrontmatter(sk *Skill, lines []string) {
	key := ""
	for _, ln := range lines {
		trimmed := strings.TrimSpace(ln)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if idx := strings.Index(trimmed, ":"); idx > 0 {
			key = strings.ToLower(strings.TrimSpace(trimmed[:idx]))
			sk.setFrontmatterValue(key, strings.TrimSpace(trimmed[idx+1:]))
			continue
		}
		// Continuation line of a list value ("  - item").
		if key != "" && strings.HasPrefix(trimmed, "-") {
			sk.setFrontmatterValue(key, strings.TrimSpace(strings.TrimPrefix(trimmed, "-")))
		}
	}
}

func (sk *Skill) setFrontmatterValue(key, val string) {
	val = strings.Trim(strings.TrimSpace(val), `"'`)
	if strings.HasPrefix(val, "[") && strings.HasSuffix(val, "]") {
		inner := strings.Trim(val, "[]")
		for _, part := range strings.Split(inner, ",") {
			if v := strings.Trim(strings.TrimSpace(part), `"'`); v != "" {
				sk.appendFrontmatter(key, v)
			}
		}
		return
	}
	if val == "" {
		return
	}
	sk.appendFrontmatter(key, val)
}

func (sk *Skill) appendFrontmatter(key, val string) {
	switch key {
	case "name":
		if sk.Name == "" {
			sk.Name = val
		}
	case "description", "desc":
		if sk.Description == "" {
			sk.Description = val
		}
	case "tools", "tool":
		sk.Tools = appendUnique(sk.Tools, val)
	case "knowledge", "knowledge_collection_ids", "collections":
		sk.KnowledgeCollectionIds = appendUnique(sk.KnowledgeCollectionIds, val)
	case "tags", "tag":
		sk.Tags = appendUnique(sk.Tags, val)
	case "enabled":
		sk.Enabled = strings.EqualFold(val, "true") || val == "1" || val == "on"
	}
}

func appendUnique(list []string, v string) []string {
	for _, item := range list {
		if item == v {
			return list
		}
	}
	return append(list, v)
}

func (s *Service) seed() {
	now := time.Now().UnixMilli()
	for _, sk := range Builtins() {
		sk.CreatedAt, sk.UpdatedAt = now, now
		_ = s.store.Save(sk.ID, sk)
		cp := *sk
		s.cache[sk.ID] = &cp
	}
}

// Builtins returns the five seeded skills (DESIGN §14#4).
func Builtins() []*Skill {
	return []*Skill{
		{
			ID:           "skill-deep-research",
			Name:         "深度研究",
			Description:  "多轮检索与交叉验证的课题研究",
			Instructions: "面对开放式课题：先澄清目标与边界，再拆解为 3-5 个子问题；逐个子问题检索资料库与记忆，交叉比对不同来源；给出带依据的结论，并显式标注不确定处与后续待验证项。",
			Tags:         []string{"研究", "检索"},
			Enabled:      true,
			Builtin:      true,
			Version:      1,
		},
		{
			ID:           "skill-doc-qa",
			Name:         "文档问答",
			Description:  "基于资料库的引文式问答",
			Instructions: "只依据资料库检索到的片段作答，并用 [n] 标注每一处引用来源；检索结果不足以回答时，明确说明缺什么，不要凭记忆编造。",
			Tags:         []string{"问答", "资料库"},
			Enabled:      true,
			Builtin:      true,
			Version:      1,
		},
		{
			ID:           "skill-data-analysis",
			Name:         "数据分析",
			Description:  "结构化数据解读与统计建议",
			Instructions: "先确认数据口径、样本量与时间范围；再给出描述性统计、趋势与异常点；区分相关与因果，指出可能的偏差来源；最后给出可视化建议与下一步分析方向。",
			Tags:         []string{"数据", "统计"},
			Enabled:      true,
			Builtin:      true,
			Version:      1,
		},
		{
			ID:           "skill-translate-polish",
			Name:         "翻译润色",
			Description:  "多语种互译与行文润色",
			Instructions: "翻译时保持原意、语气与术语一致，仅输出译文；润色时保留作者风格，只改必要之处，并说明主要改动理由。不确定处给出备选译法而非擅自取舍。",
			Tags:         []string{"翻译", "写作"},
			Enabled:      true,
			Builtin:      true,
			Version:      1,
		},
		{
			ID:           "skill-brainstorm",
			Name:         "头脑风暴",
			Description:  "发散收敛式创意生成",
			Instructions: "先发散：围绕主题产出 10 个以上差异化想法，禁止自我审查与过早评判；再收敛：按可行性、新颖度、成本三维筛选，给出 Top 3 及推荐理由与最小验证方案。",
			Tags:         []string{"创意", "发散"},
			Enabled:      true,
			Builtin:      true,
			Version:      1,
		},
	}
}
