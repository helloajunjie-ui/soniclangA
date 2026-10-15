// Package conversation owns the session lifecycle, append-only message stream
// and the task state machine (DESIGN §5.2 / §7 / §6.7). The task state machine
// lives here (not in orchestrator); orchestrator only calls UpsertStep/SaveCheckpoint.
//
// There is no approval inbox. An earlier revision carried one — an Approval
// entity with a pending/approved/rejected status, a Risk grade and a "remember
// this decision" flag — modelled on a corporate sign-off queue. It was never
// wired to anything (nothing created an Approval, no endpoint decided one, the
// UI never read it) and it contradicted the product's own principle of not
// grading risk. The user gives a task; the agent carries it out. Consent lives
// at the task level, not as a per-action application awaiting a stamp.
package conversation

import (
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/agenthub/agent-proxy/internal/provider"
	"github.com/agenthub/agent-proxy/internal/storage"
	"github.com/agenthub/agent-proxy/internal/util"
)

// Mode is the session mode (DESIGN §2.3 / §6.8).
type Mode string

const (
	ModeChat Mode = "chat"
	ModeTask Mode = "task"
	ModeAuto Mode = "auto"
)

// TaskStatus is the task-level state (DESIGN §15.2).
type TaskStatus string

const (
	TaskPending   TaskStatus = "pending"
	TaskRunning   TaskStatus = "running"
	TaskPaused    TaskStatus = "paused"
	TaskDone      TaskStatus = "done"
	TaskFailed    TaskStatus = "failed"
	TaskCancelled TaskStatus = "cancelled"
)

// StepStatus is a single task step's state.
type StepStatus string

const (
	StepPending StepStatus = "pending"
	StepRunning StepStatus = "running"
	StepDone    StepStatus = "done"
	StepFailed  StepStatus = "failed"
	StepSkipped StepStatus = "skipped"
)

// Citation is a retrieved knowledge/memory reference shown in the context panel.
type Citation struct {
	Collection string  `json:"collection,omitempty"`
	Doc        string  `json:"doc,omitempty"`
	Chunk      string  `json:"chunk,omitempty"`
	Score      float64 `json:"score,omitempty"`
}

// RouteInfo records an auto-routing decision (DESIGN §6.8).
type RouteInfo struct {
	Mode       string  `json:"mode"`
	Confidence float64 `json:"confidence"`
	Reason     string  `json:"reason"`
	Source     string  `json:"source"`
}

// ProviderRef identifies a (provider, model) pair.
type ProviderRef struct {
	ProviderId string `json:"providerId,omitempty"`
	Model      string `json:"model,omitempty"`
}

// FallbackInfo records a failover switch (DESIGN §6.6).
type FallbackInfo struct {
	From   ProviderRef `json:"from"`
	To     ProviderRef `json:"to"`
	Reason string      `json:"reason"`
	Kind   string      `json:"kind"` // inKey | crossProvider | roleFallback
}

// Attachment is a message attachment reference.
type Attachment struct {
	Type string `json:"type"`
	Name string `json:"name"`
	URL  string `json:"url,omitempty"`
}

// Message is one turn in the append-only stream (DESIGN §7 Message row).
type Message struct {
	ID           string              `json:"id"`
	Seq          int64               `json:"seq"`
	Role         string              `json:"role"`
	Content      string              `json:"content"`
	Reasoning    string              `json:"reasoning,omitempty"`
	ToolCalls    []provider.ToolCall `json:"toolCalls,omitempty"`
	ToolCallId   string              `json:"toolCallId,omitempty"`
	ProviderId   string              `json:"providerId,omitempty"`
	Model        string              `json:"model,omitempty"`
	Usage        provider.Usage      `json:"usage,omitempty"`
	FinishReason string              `json:"finishReason,omitempty"`
	Citations    []Citation          `json:"citations,omitempty"`
	Route        *RouteInfo          `json:"route,omitempty"`
	Fallback     *FallbackInfo       `json:"fallback,omitempty"`
	Attachments  []Attachment        `json:"attachments,omitempty"`
	Status       string              `json:"status,omitempty"`
	Version      int                 `json:"version,omitempty"`
	ParentId     string              `json:"parentId,omitempty"`
	CreatedAt    int64               `json:"createdAt"`
}

// Checkpoint is a resumable breakpoint (DESIGN §15.6 #3).
type Checkpoint struct {
	StepId         string `json:"stepId"`
	LastMessageId  string `json:"lastMessageId"`
	LastToolCallId string `json:"lastToolCallId"`
	Summary        string `json:"summary"`
	Ts             int64  `json:"ts"`
}

// TaskStep is one step of the plan (DESIGN §7 taskPlan.steps).
type TaskStep struct {
	ID         string              `json:"id"`
	Title      string              `json:"title"`
	Status     StepStatus          `json:"status"`
	Summary    string              `json:"summary,omitempty"`
	DependsOn  []string            `json:"dependsOn,omitempty"`
	ToolCalls  []provider.ToolCall `json:"toolCalls,omitempty"`
	Error      string              `json:"error,omitempty"`
	RetryCount int                 `json:"retryCount"`
	StartedAt  int64               `json:"startedAt,omitempty"`
	FinishedAt int64               `json:"finishedAt,omitempty"`
	Checkpoint *Checkpoint         `json:"checkpoint,omitempty"`
}

// TaskPlan is the task plan attached to a conversation (DESIGN §7).
type TaskPlan struct {
	Goal   string     `json:"goal,omitempty"`
	Status TaskStatus `json:"status,omitempty"`
	Steps  []TaskStep `json:"steps,omitempty"`
}

// Budget is the per-session/task token & cost budget (soft by default in M2).
type Budget struct {
	TokensSoftLimit int     `json:"tokensSoftLimit,omitempty"`
	TokensHardLimit int     `json:"tokensHardLimit,omitempty"`
	CostLimit       float64 `json:"costLimit,omitempty"`
}

// Usage is the cumulative session usage.
type Usage struct {
	Tokens int     `json:"tokens"`
	Cost   float64 `json:"cost"`
}

// Conversation is the session metadata entity (DESIGN §7 meta.json).
type Conversation struct {
	ID                string     `json:"id"`
	RoleId            string     `json:"roleId"`
	Title             string     `json:"title"`
	Pinned            bool       `json:"pinned"`
	Mode              Mode       `json:"mode"`
	Permission        string     `json:"permission"` // ask|auto|full
	DefaultProviderId string     `json:"defaultProviderId,omitempty"`
	TaskStatus        TaskStatus `json:"taskStatus,omitempty"`
	MaxSteps          int        `json:"maxSteps"`
	Budget            Budget     `json:"budget,omitempty"`
	ActualProviderId  string     `json:"actualProviderId,omitempty"`
	ActualModel       string     `json:"actualModel,omitempty"`
	TaskPlan          *TaskPlan  `json:"taskPlan,omitempty"`
	Usage             Usage      `json:"usage"`
	// Vars holds the values for the role's {{template}} variables (M4). Stored
	// per conversation so two sessions using the same role can fill them in
	// differently.
	Vars         map[string]string `json:"vars,omitempty"`
	MessageCount int               `json:"messageCount"`
	CreatedAt    int64             `json:"createdAt"`
	UpdatedAt    int64             `json:"updatedAt"`
}

// Summary is the lightweight list view.
type Summary struct {
	ID           string     `json:"id"`
	RoleId       string     `json:"roleId"`
	Title        string     `json:"title"`
	Mode         Mode       `json:"mode"`
	TaskStatus   TaskStatus `json:"taskStatus,omitempty"`
	Pinned       bool       `json:"pinned"`
	MessageCount int        `json:"messageCount"`
	UpdatedAt    int64      `json:"updatedAt"`
}

// MetaPatch is a partial update to conversation metadata.
type MetaPatch struct {
	Title             *string `json:"title,omitempty"`
	Pinned            *bool   `json:"pinned,omitempty"`
	Mode              *Mode   `json:"mode,omitempty"`
	Permission        *string `json:"permission,omitempty"`
	DefaultProviderId *string `json:"defaultProviderId,omitempty"`
}

// Service manages conversations.
type Service struct {
	store storage.Store
	mu    sync.Mutex
}

// New constructs the conversation Service.
//
// It used to take a baseDir for the per-conversation approvals file. With the
// approval inbox gone (see the package comment) there is no side file left:
// everything a conversation owns now lives in the store, which is what keeps
// the two from drifting apart.
func New(store storage.Store) *Service {
	return &Service{store: store}
}

// List returns conversation summaries, newest first.
func (s *Service) List(filter map[string]string) ([]Summary, error) {
	ids, err := s.store.IDs()
	if err != nil {
		return nil, err
	}
	var out []Summary
	for _, id := range ids {
		var c Conversation
		if err := s.store.Load(id, &c); err != nil {
			continue
		}
		if filter != nil {
			if r, ok := filter["roleId"]; ok && c.RoleId != r {
				continue
			}
			if m, ok := filter["mode"]; ok && string(c.Mode) != m {
				continue
			}
		}
		out = append(out, Summary{ID: c.ID, RoleId: c.RoleId, Title: c.Title, Mode: c.Mode, TaskStatus: c.TaskStatus, Pinned: c.Pinned, MessageCount: c.MessageCount, UpdatedAt: c.UpdatedAt})
	}
	return out, nil
}

// Get returns a conversation's metadata.
func (s *Service) Get(id string) (*Conversation, error) {
	var c Conversation
	if err := s.store.Load(id, &c); err != nil {
		return nil, err
	}
	return &c, nil
}

// Delete removes a conversation and its message stream.
func (s *Service) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.store.Remove(id)
}

// Create starts a new conversation bound to a role and mode.
func (s *Service) Create(roleID string, mode Mode) (*Conversation, error) {
	// Held so two rapid creates cannot interleave ids in the same store; every
	// other mutator takes this lock too.
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UnixMilli()
	c := &Conversation{
		ID:           util.NewID(),
		RoleId:       roleID,
		Title:        "新对话",
		Mode:         mode,
		Permission:   "ask",
		MaxSteps:     30,
		MessageCount: 0,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	if mode == ModeTask {
		c.TaskStatus = TaskPending
		c.TaskPlan = &TaskPlan{Status: TaskPending}
	}
	if err := s.store.Save(c.ID, c); err != nil {
		return nil, err
	}
	return c, nil
}

// SetVars stores the role template-variable values for a conversation (M4).
func (s *Service) SetVars(id string, vars map[string]string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var c Conversation
	if err := s.store.Load(id, &c); err != nil {
		return err
	}
	c.Vars = vars
	c.UpdatedAt = time.Now().UnixMilli()
	return s.store.Save(id, &c)
}

// UpdateMeta applies a partial patch.
func (s *Service) UpdateMeta(id string, patch MetaPatch) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var c Conversation
	if err := s.store.Load(id, &c); err != nil {
		return err
	}
	if patch.Title != nil {
		c.Title = *patch.Title
	}
	if patch.Pinned != nil {
		c.Pinned = *patch.Pinned
	}
	if patch.Mode != nil {
		c.Mode = *patch.Mode
	}
	if patch.Permission != nil {
		c.Permission = *patch.Permission
	}
	if patch.DefaultProviderId != nil {
		c.DefaultProviderId = *patch.DefaultProviderId
	}
	c.UpdatedAt = time.Now().UnixMilli()
	return s.store.Save(id, c)
}

// AppendMessage appends one message to the stream (atomic seq assignment).
func (s *Service) AppendMessage(id string, msg Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var c Conversation
	if err := s.store.Load(id, &c); err != nil {
		return err
	}
	if msg.ID == "" {
		msg.ID = util.NewID()
	}
	if msg.Seq == 0 {
		msg.Seq = int64(c.MessageCount) + 1
	}
	if msg.CreatedAt == 0 {
		msg.CreatedAt = time.Now().UnixMilli()
	}
	c.MessageCount++
	c.UpdatedAt = msg.CreatedAt
	if err := s.store.Save(id, c); err != nil {
		return err
	}
	return s.store.AppendJSONL(id, msg)
}

// Messages returns the full message stream for a conversation (history replay).
func (s *Service) Messages(id string) ([]Message, error) {
	lines, err := s.store.ReadJSONL(id)
	if err != nil {
		return nil, err
	}
	out := make([]Message, 0, len(lines))
	for _, l := range lines {
		var m Message
		if json.Unmarshal(l, &m) == nil {
			out = append(out, m)
		}
	}
	return out, nil
}

// StreamMessages is reserved for live replay/subscription (M2: not implemented).
func (s *Service) StreamMessages(id string, sinceSeq int64) (<-chan Message, error) {
	return nil, nil
}

// SetMode switches the session mode.
func (s *Service) SetMode(id string, mode Mode) error {
	return s.UpdateMeta(id, MetaPatch{Mode: &mode})
}

// SwitchRole rebinds the session to another role.
func (s *Service) SwitchRole(id string, roleID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var c Conversation
	if err := s.store.Load(id, &c); err != nil {
		return err
	}
	c.RoleId = roleID
	c.UpdatedAt = time.Now().UnixMilli()
	return s.store.Save(id, c)
}

// GetTask returns the task plan.
func (s *Service) GetTask(id string) (*TaskPlan, error) {
	c, err := s.Get(id)
	if err != nil {
		return nil, err
	}
	if c.TaskPlan == nil {
		return &TaskPlan{Status: c.TaskStatus}, nil
	}
	return c.TaskPlan, nil
}

// UpsertStep inserts or updates a task step.
func (s *Service) UpsertStep(id string, step TaskStep) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var c Conversation
	if err := s.store.Load(id, &c); err != nil {
		return err
	}
	if c.TaskPlan == nil {
		c.TaskPlan = &TaskPlan{Status: TaskRunning}
	}
	found := false
	for i := range c.TaskPlan.Steps {
		if c.TaskPlan.Steps[i].ID == step.ID {
			c.TaskPlan.Steps[i] = step
			found = true
			break
		}
	}
	if !found {
		c.TaskPlan.Steps = append(c.TaskPlan.Steps, step)
	}
	c.TaskPlan.Status = TaskRunning
	c.UpdatedAt = time.Now().UnixMilli()
	return s.store.Save(id, c)
}

// SaveCheckpoint records a resumable breakpoint on a step.
func (s *Service) SaveCheckpoint(id string, cp Checkpoint) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var c Conversation
	if err := s.store.Load(id, &c); err != nil {
		return err
	}
	if c.TaskPlan == nil {
		return fmt.Errorf("no task plan for conversation %s", id)
	}
	for i := range c.TaskPlan.Steps {
		if c.TaskPlan.Steps[i].ID == cp.StepId {
			cp.Ts = time.Now().UnixMilli()
			c.TaskPlan.Steps[i].Checkpoint = &cp
			break
		}
	}
	c.UpdatedAt = time.Now().UnixMilli()
	return s.store.Save(id, c)
}

// Export renders the conversation as markdown or json.
func (s *Service) Export(id string, format string) ([]byte, error) {
	c, err := s.Get(id)
	if err != nil {
		return nil, err
	}
	lines, err := s.store.ReadJSONL(id)
	if err != nil {
		return nil, err
	}
	if format == "json" {
		doc := map[string]interface{}{"schemaVersion": 1, "conversation": c}
		var msgs []Message
		for _, l := range lines {
			var m Message
			if json.Unmarshal(l, &m) == nil {
				msgs = append(msgs, m)
			}
		}
		doc["messages"] = msgs
		return json.MarshalIndent(doc, "", "  ")
	}
	// markdown
	out := "# " + c.Title + "\n\n"
	for _, l := range lines {
		var m Message
		if json.Unmarshal(l, &m) != nil {
			continue
		}
		if m.Role == "system" {
			continue
		}
		out += "### " + m.Role + "\n\n" + m.Content + "\n\n"
	}
	return []byte(out), nil
}
