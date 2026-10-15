// Package role implements the role library: reusable configuration units that
// bind a system prompt, model params, skills, tools and knowledge bases
// (DESIGN §5/§7/§14). Eight built-in roles are seeded on first run.
package role

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/agenthub/agent-proxy/internal/storage"
	"github.com/agenthub/agent-proxy/internal/util"
)

// Fallback is a failover target in the role's fallback chain (DESIGN §6.6).
type Fallback struct {
	ProviderId string `json:"providerId"`
	Model      string `json:"model"`
	Reason     string `json:"reason,omitempty"`
}

// Budget is the per-task token/cost budget (soft by default in M2, DESIGN §14#8).
type Budget struct {
	TokensSoftLimit *int     `json:"tokensSoftLimit,omitempty"`
	TokensHardLimit *int     `json:"tokensHardLimit,omitempty"`
	CostLimit       *float64 `json:"costLimit,omitempty"`
}

// RoleVar is a template variable in the system prompt ({{name}}).
type RoleVar struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Required    bool   `json:"required"`
	Default     string `json:"default,omitempty"`
}

// AutoRoute overrides the global auto-routing config for this role (DESIGN §6.8).
type AutoRoute struct {
	Enabled   bool    `json:"enabled"`
	Strategy  string  `json:"strategy,omitempty"`
	Threshold float64 `json:"threshold,omitempty"`
}

// Role is the persisted role entity (DESIGN §7).
type Role struct {
	ID                     string   `json:"id"`
	Name                   string   `json:"name"`
	Avatar                 string   `json:"avatar,omitempty"`
	Description            string   `json:"description,omitempty"`
	SystemPrompt           string   `json:"systemPrompt"`
	DefaultMode            string   `json:"defaultMode"` // chat|task|auto
	ProviderId             string   `json:"providerId,omitempty"`
	Model                  string   `json:"model,omitempty"`
	Temperature            float64  `json:"temperature"`
	MaxTokens              int      `json:"maxTokens"`
	TopP                   float64  `json:"topP"`
	Tools                  []string `json:"tools,omitempty"`
	Skills                 []string `json:"skills,omitempty"`
	KnowledgeCollectionIds []string `json:"knowledgeCollectionIds,omitempty"`
	// MethodologyIds pins methodology entries for this role: they activate on
	// every turn even when the query has no keyword hit (M4, 三层激活).
	MethodologyIds   []string   `json:"methodologyIds,omitempty"`
	Fallbacks        []Fallback `json:"fallbacks,omitempty"`
	RetrievalTopK    int        `json:"retrievalTopK"`
	Permission       string     `json:"permission"` // ask|auto|full
	MaxSteps         int        `json:"maxSteps"`
	Budget           Budget     `json:"budget,omitempty"`
	AutoRoute        AutoRoute  `json:"autoRoute,omitempty"`
	Vars             []RoleVar  `json:"vars,omitempty"`
	MissingVarPolicy string     `json:"missingVarPolicy,omitempty"` // ask|error|skip
	Tags             []string   `json:"tags,omitempty"`
	Version          int        `json:"version"`
	Builtin          bool       `json:"builtin"`
	CreatedAt        int64      `json:"createdAt"`
	UpdatedAt        int64      `json:"updatedAt"`
}

// Service manages roles and seeds built-ins.
type Service struct {
	store storage.Store
	mu    sync.RWMutex
	cache map[string]*Role
}

// New constructs the role Service and seeds built-ins if empty.
func New(store storage.Store) *Service {
	s := &Service{store: store, cache: map[string]*Role{}}
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
		var r Role
		if err := s.store.Load(id, &r); err == nil {
			s.cache[id] = &r
		}
	}
}

// List returns all roles.
// List returns detached copies of every role. Callers (the HTTP handler)
// serialise these after the lock is released, while Update mutates the cached
// entity — returning the live pointer would be a data race.
func (s *Service) List() []*Role {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Role, 0, len(s.cache))
	for _, r := range s.cache {
		out = append(out, cloneRole(r))
	}
	return out
}

// Get returns a detached copy of a role by id.
func (s *Service) Get(id string) (*Role, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.cache[id]
	if !ok {
		return nil, fmt.Errorf("role %s not found", id)
	}
	return cloneRole(r), nil
}

// cloneRole deep-copies via JSON so newly added fields are carried along; on
// failure it degrades to a shallow copy.
func cloneRole(r *Role) *Role {
	if r == nil {
		return nil
	}
	b, err := json.Marshal(r)
	if err != nil {
		cp := *r
		return &cp
	}
	var out Role
	if err := json.Unmarshal(b, &out); err != nil {
		cp := *r
		return &cp
	}
	return &out
}

// Create persists a new role.
func (s *Service) Create(r *Role) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r.ID == "" {
		r.ID = util.NewID()
	}
	now := time.Now().UnixMilli()
	r.CreatedAt, r.UpdatedAt = now, now
	if r.Version == 0 {
		r.Version = 1
	}
	if err := s.store.Save(r.ID, r); err != nil {
		return err
	}
	s.cache[r.ID] = r
	return nil
}

// Update replaces a role.
func (s *Service) Update(id string, r *Role) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r.ID = id
	r.UpdatedAt = time.Now().UnixMilli()
	if err := s.store.Save(id, r); err != nil {
		return err
	}
	s.cache[id] = r
	return nil
}

// Delete removes a role.
func (s *Service) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = s.store.Remove(id)
	delete(s.cache, id)
	return nil
}

// Duplicate clones a role under a new id, so a user can fork a built-in
// instead of editing the seed in place.
func (s *Service) Duplicate(id, newName string) (*Role, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	src, ok := s.cache[id]
	if !ok {
		return nil, fmt.Errorf("role %s not found", id)
	}
	cp := *src
	cp.Vars = append([]RoleVar(nil), src.Vars...)
	cp.Tools = append([]string(nil), src.Tools...)
	cp.Skills = append([]string(nil), src.Skills...)
	cp.KnowledgeCollectionIds = append([]string(nil), src.KnowledgeCollectionIds...)
	cp.Fallbacks = append([]Fallback(nil), src.Fallbacks...)
	cp.Tags = append([]string(nil), src.Tags...)

	cp.ID = util.NewID()
	cp.Builtin = false
	cp.Version = 1
	if name := strings.TrimSpace(newName); name != "" {
		cp.Name = name
	} else {
		cp.Name = src.Name + " 副本"
	}
	now := time.Now().UnixMilli()
	cp.CreatedAt, cp.UpdatedAt = now, now
	if err := s.store.Save(cp.ID, &cp); err != nil {
		return nil, err
	}
	out := cp
	s.cache[cp.ID] = &out
	return &out, nil
}

// ExportAll exports every role as a single bundle (DESIGN §8 import/export).
func (s *Service) ExportAll() ([]byte, error) {
	doc := map[string]interface{}{
		"schemaVersion": 1,
		"kind":          "roles",
		"exportedAt":    time.Now().UnixMilli(),
		"items":         s.List(),
	}
	return json.MarshalIndent(doc, "", "  ")
}

// Export returns a role as JSON with schemaVersion (DESIGN §8 import/export).
func (s *Service) Export(id string) ([]byte, error) {
	r, err := s.Get(id)
	if err != nil {
		return nil, err
	}
	doc := map[string]interface{}{"schemaVersion": 1}
	b, _ := json.Marshal(r)
	_ = json.Unmarshal(b, &doc)
	return json.MarshalIndent(doc, "", "  ")
}

// Import creates a role from exported JSON, assigning a fresh id.
func (s *Service) Import(data []byte) (*Role, error) {
	var doc map[string]interface{}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	b, _ := json.Marshal(doc)
	var r Role
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, err
	}
	r.ID = ""
	if err := s.Create(&r); err != nil {
		return nil, err
	}
	return &r, nil
}

func (s *Service) seed() {
	now := time.Now().UnixMilli()
	for _, r := range builtins() {
		r.CreatedAt, r.UpdatedAt = now, now
		_ = s.store.Save(r.ID, r)
		s.cache[r.ID] = r
	}
}

func builtins() []*Role {
	def := func(id, name, desc, prompt string) *Role {
		return &Role{
			ID:            id,
			Name:          name,
			Description:   desc,
			SystemPrompt:  prompt,
			DefaultMode:   "auto",
			Temperature:   0.7,
			MaxTokens:     2048,
			TopP:          1.0,
			Permission:    "ask",
			MaxSteps:      30,
			RetrievalTopK: 5,
			Version:       1,
			Builtin:       true,
			AutoRoute:     AutoRoute{Enabled: true},
		}
	}
	return []*Role{
		def("general", "通用助手", "通用对话与问答", "你是一个乐于助人的通用 AI 助手，擅长回答疑问、创作内容、分析与推理。回答简洁准确，必要时给出步骤或示例。"),
		def("translator", "翻译", "中英与多语种互译", "你是一名专业翻译，擅长中英互译与多语种转换。保持原意、语气与术语一致性；仅输出译文，除非用户要求解释。"),
		def("codereviewer", "代码审查", "代码质量与安全检查", "你是资深代码审查专家。关注正确性、安全性、性能、可维护性与边界情况，给出具体可执行的修改建议，并按严重程度分级。"),
		def("pm", "产品经理", "需求拆解与方案评估", "你是产品经理，擅长需求拆解、用户故事撰写、优先级排序与方案权衡。输出结构清晰，先确认目标与约束再给方案。"),
		def("techwriter", "技术写作", "结构化技术文档", "你是技术写作专家，产出清晰、结构化、面向读者的文档：标题层级分明，示例准确，术语一致。"),
		def("interviewer", "面试官", "岗位技术面试", "你是面试官，针对目标岗位提出有梯度的问题，评估回答的深度与正确性，并给出简洁的反馈与进一步追问。"),
		def("learccoach", "学习教练", "学习计划与理解检查", "你是学习教练，帮助用户制定学习计划、拆解知识点、用提问检查理解，并在卡点时给出最小必要的讲解。"),
		def("dataanalyst", "数据分析师", "数据解读与统计建议", "你是数据分析师，擅长解读数据、选择合适统计方法、指出偏差与陷阱，并给出可落地的可视化与下一步分析建议。"),
	}
}
