// Package job is the unified background task queue (DESIGN §5 job / §5.2). All
// long operations (doc parsing, embedding, memory consolidation, uploads, task
// execution) enter here and return a jobId; status is persisted to data/jobs so
// progress survives restarts. A single in-process worker pool runs the jobs.
package job

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/agenthub/agent-proxy/internal/storage"
	"github.com/agenthub/agent-proxy/internal/util"
)

// Job is a queued unit of work.
type Job struct {
	ID        string `json:"id"`
	Type      string `json:"type"`
	Payload   []byte `json:"payload"`
	Priority  int    `json:"priority"`
	CreatedAt int64  `json:"createdAt"`
}

// JobStatus is the observable state of a job.
type JobStatus struct {
	ID        string `json:"id"`
	Type      string `json:"type"`
	Status    string `json:"status"` // pending|running|done|failed|cancelled
	Progress  int    `json:"progress"`
	Error     string `json:"error,omitempty"`
	CreatedAt int64  `json:"createdAt"`
	UpdatedAt int64  `json:"updatedAt"`
}

// Progress is a progress update emitted on a subscription channel.
type Progress struct {
	JobID    string `json:"jobId"`
	Status   string `json:"status"`
	Progress int    `json:"progress"`
	Message  string `json:"message,omitempty"`
}

// JobQueue is the public queue contract (DESIGN §5.2).
type JobQueue interface {
	Submit(job Job) (string, error)
	Status(jobID string) (JobStatus, error)
	Subscribe(jobID string) (<-chan Progress, error)
	Cancel(jobID string) error
}

// Worker executes one job type.
type Worker interface {
	Type() string
	Run(ctx context.Context, payload []byte) error
}

// Service implements JobQueue with an in-process worker pool.
type Service struct {
	store       storage.Store
	mu          sync.RWMutex
	queue       chan string
	jobs        map[string]*Job
	statuses    map[string]*JobStatus
	subscribers map[string][]chan Progress
	cancels     map[string]chan struct{}
	workers     map[string]Worker
}

// New constructs the job Service and starts the worker pool.
func New(store storage.Store) *Service {
	s := &Service{
		store:       store,
		queue:       make(chan string, 1024),
		jobs:        map[string]*Job{},
		statuses:    map[string]*JobStatus{},
		subscribers: map[string][]chan Progress{},
		cancels:     map[string]chan struct{}{},
		workers:     map[string]Worker{},
	}
	s.load()
	for i := 0; i < 4; i++ {
		go s.loop()
	}
	return s
}

func (s *Service) load() {
	ids, err := s.store.IDs()
	if err != nil {
		return
	}
	for _, id := range ids {
		var j Job
		if err := s.store.Load(id, &j); err == nil {
			s.jobs[id] = &j
			s.statuses[id] = &JobStatus{ID: id, Type: j.Type, Status: "pending", CreatedAt: j.CreatedAt, UpdatedAt: j.CreatedAt}
		}
	}
}

// RegisterWorker adds a worker for a job type.
func (s *Service) RegisterWorker(w Worker) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.workers[w.Type()] = w
}

// Submit enqueues a job and returns its id.
func (s *Service) Submit(job Job) (string, error) {
	if job.ID == "" {
		job.ID = util.NewID()
	}
	now := time.Now().UnixMilli()
	job.CreatedAt = now
	s.mu.Lock()
	s.jobs[job.ID] = &job
	s.statuses[job.ID] = &JobStatus{ID: job.ID, Type: job.Type, Status: "pending", CreatedAt: now, UpdatedAt: now}
	s.cancels[job.ID] = make(chan struct{})
	s.mu.Unlock()
	if err := s.persist(&job); err != nil {
		return "", err
	}
	s.queue <- job.ID
	return job.ID, nil
}

// Status returns a job's status.
func (s *Service) Status(jobID string) (JobStatus, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	st, ok := s.statuses[jobID]
	if !ok {
		return JobStatus{}, fmt.Errorf("job %s not found", jobID)
	}
	return *st, nil
}

// List returns every known job status, newest first. Without this the UI can
// only poll a job whose id it already happens to hold.
func (s *Service) List() []JobStatus {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]JobStatus, 0, len(s.statuses))
	for _, st := range s.statuses {
		out = append(out, *st)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt > out[j].CreatedAt })
	return out
}

// ReportProgress updates a running job's progress and notifies subscribers.
// Unknown job ids are ignored (a worker may outlive a cancelled/cleared job).
func (s *Service) ReportProgress(jobID string, progress int, message string) {
	s.mu.Lock()
	st := s.statuses[jobID]
	if st != nil {
		if progress >= 0 {
			st.Progress = progress
		}
		if message != "" {
			// reuse Error as the human-readable stage message while running.
			st.Error = message
		}
		st.UpdatedAt = time.Now().UnixMilli()
	}
	subs := append([]chan Progress(nil), s.subscribers[jobID]...)
	s.mu.Unlock()
	for _, ch := range subs {
		select {
		case ch <- Progress{JobID: jobID, Status: statusOf(st), Progress: progress, Message: message}:
		default:
		}
	}
}

func statusOf(st *JobStatus) string {
	if st == nil {
		return ""
	}
	return st.Status
}

// Subscribe returns a channel that receives progress updates for a job.
func (s *Service) Subscribe(jobID string) (<-chan Progress, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.statuses[jobID]; !ok {
		return nil, fmt.Errorf("job %s not found", jobID)
	}
	ch := make(chan Progress, 16)
	s.subscribers[jobID] = append(s.subscribers[jobID], ch)
	return ch, nil
}

// Cancel requests cancellation of a job.
func (s *Service) Cancel(jobID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.statuses[jobID]
	if !ok {
		return fmt.Errorf("job %s not found", jobID)
	}
	if c, ok := s.cancels[jobID]; ok {
		close(c)
	}
	st.Status = "cancelled"
	st.UpdatedAt = time.Now().UnixMilli()
	return nil
}

func (s *Service) loop() {
	for jobID := range s.queue {
		s.process(jobID)
	}
}

func (s *Service) process(jobID string) {
	s.mu.RLock()
	job := s.jobs[jobID]
	st := s.statuses[jobID]
	cancel := s.cancels[jobID]
	s.mu.RUnlock()
	if job == nil || st == nil {
		return
	}
	s.update(jobID, "running", 0, "")
	w, ok := s.workers[job.Type]
	if !ok {
		s.update(jobID, "failed", 0, "no worker registered for type "+job.Type)
		s.persistStatus(jobID)
		return
	}
	ctx, c := context.WithCancel(context.Background())
	defer c()
	go func() {
		select {
		case <-cancel:
			c()
		case <-ctx.Done():
		}
	}()
	err := w.Run(ctx, job.Payload)
	if err != nil {
		s.update(jobID, "failed", 0, err.Error())
	} else {
		s.update(jobID, "done", 100, "")
	}
	s.persistStatus(jobID)
}

func (s *Service) update(jobID, status string, progress int, msg string) {
	s.mu.Lock()
	st := s.statuses[jobID]
	if st != nil {
		st.Status = status
		st.Progress = progress
		st.Error = msg
		st.UpdatedAt = time.Now().UnixMilli()
	}
	subs := append([]chan Progress(nil), s.subscribers[jobID]...)
	s.mu.Unlock()
	for _, ch := range subs {
		select {
		case ch <- Progress{JobID: jobID, Status: status, Progress: progress, Message: msg}:
		default:
		}
	}
}

func (s *Service) persist(job *Job) error {
	return s.store.Save(job.ID, job)
}

func (s *Service) persistStatus(jobID string) {
	s.mu.RLock()
	st := s.statuses[jobID]
	s.mu.RUnlock()
	if st == nil {
		return
	}
	data, _ := json.Marshal(st)
	p := filepath.Join(s.store.Dir(), jobID+".status.json")
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return
	}
	_ = os.Rename(tmp, p)
}

// Snapshot returns a JSON view of one job's status (used by handlers).
func (s *Service) Snapshot(jobID string) ([]byte, error) {
	st, err := s.Status(jobID)
	if err != nil {
		return nil, err
	}
	return json.Marshal(st)
}
