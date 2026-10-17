package job

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agenthub/agent-proxy/internal/storage"
)

// The job queue had no tests. The behaviours below are the ones a user can hit
// from the UI, and each one was wrong: pressing cancel twice panicked,
// cancelling a queued job left it running, and cancelling a running job reported
// "failed".

// gateWorker blocks until release is closed, so a test can hold all four pool
// workers and control exactly when the queue advances.
type gateWorker struct {
	typ     string
	ran     atomic.Int32
	release chan struct{}
}

func (w *gateWorker) Type() string { return w.typ }

func (w *gateWorker) Run(ctx context.Context, _ []byte) error {
	w.ran.Add(1)
	select {
	case <-w.release:
		return nil
	case <-ctx.Done():
		// A real worker aborts on cancel; report it the way the queue sees it.
		return errors.New("cancelled: " + ctx.Err().Error())
	}
}

func newQueue(t *testing.T) *Service {
	t.Helper()
	return New(storage.NewJSON(t.TempDir()))
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func status(t *testing.T, s *Service, id string) string {
	t.Helper()
	st, err := s.Status(id)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	return st.Status
}

// settle blocks until the queue has written the job's final status file. Without
// it a pool goroutine can still be writing into t.TempDir when the test returns,
// and TempDir removal fails with "directory is not empty".
func settle(t *testing.T, s *Service, id string) {
	t.Helper()
	path := filepath.Join(s.store.Dir(), statusDir, id+".json")
	waitFor(t, "status file for "+id, func() bool {
		_, err := os.Stat(path)
		return err == nil
	})
}

// The UI retries or double-clicks; closing an already-closed channel panics.
func TestCancelIsIdempotent(t *testing.T) {
	s := newQueue(t)
	w := &gateWorker{typ: "gate", release: make(chan struct{})}
	s.RegisterWorker(w)

	id, err := s.Submit(Job{Type: "gate"})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	waitFor(t, "worker to start", func() bool { return w.ran.Load() == 1 })

	if err := s.Cancel(id); err != nil {
		t.Fatalf("first cancel: %v", err)
	}
	if err := s.Cancel(id); err != nil {
		t.Fatalf("second cancel: %v", err)
	}
	close(w.release)
	settle(t, s, id)
}

// Cancelling a job that is still queued must stop it. It used to run anyway and
// overwrite the status, so an upload the user cancelled finished silently.
func TestCancelStopsQueuedJob(t *testing.T) {
	s := newQueue(t)
	w := &gateWorker{typ: "gate", release: make(chan struct{})}
	s.RegisterWorker(w)

	// Occupy all four pool workers.
	var ids []string
	for i := 0; i < 4; i++ {
		id, err := s.Submit(Job{Type: "gate"})
		if err != nil {
			t.Fatalf("submit blocker: %v", err)
		}
		ids = append(ids, id)
	}
	waitFor(t, "pool to be busy", func() bool { return w.ran.Load() == 4 })

	queued, err := s.Submit(Job{Type: "gate"})
	if err != nil {
		t.Fatalf("submit queued: %v", err)
	}
	if err := s.Cancel(queued); err != nil {
		t.Fatalf("cancel queued: %v", err)
	}

	// Release the pool: the next thing a worker picks up is the cancelled job.
	close(w.release)
	waitFor(t, "blockers to finish", func() bool {
		done := 0
		for _, id := range ids {
			if status(t, s, id) == "done" {
				done++
			}
		}
		return done == 4
	})

	if got := w.ran.Load(); got != 4 {
		t.Fatalf("the cancelled job ran anyway (worker executions = %d, want 4)", got)
	}
	if got := status(t, s, queued); got != "cancelled" {
		t.Errorf("queued job status = %q, want cancelled", got)
	}
	for _, id := range append(ids, queued) {
		settle(t, s, id)
	}
}

// A worker that aborts on cancel returns a context error. Reporting that as
// "failed" tells the user their cancel did not work.
func TestCancellingRunningJobReportsCancelled(t *testing.T) {
	s := newQueue(t)
	w := &gateWorker{typ: "gate", release: make(chan struct{})}
	s.RegisterWorker(w)

	id, err := s.Submit(Job{Type: "gate"})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	waitFor(t, "worker to start", func() bool { return w.ran.Load() == 1 })

	if err := s.Cancel(id); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	// The status file is written after the pool has recorded the outcome, so
	// waiting for it means the assertion below sees the final state rather than
	// racing the worker's return.
	settle(t, s, id)
	if got := status(t, s, id); got != "cancelled" {
		t.Errorf("status after cancel = %q, want cancelled (not failed)", got)
	}
}

// An unregistered job type fails cleanly instead of leaving the job "running"
// forever, which is what the UI would poll on.
func TestUnknownWorkerTypeFails(t *testing.T) {
	s := newQueue(t)
	id, err := s.Submit(Job{Type: "nobody-handles-this"})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	waitFor(t, "job to fail", func() bool { return status(t, s, id) == "failed" })
	settle(t, s, id)
}

// Statuses are persisted so a restart does not resurrect finished jobs as
// "pending" (the UI polled a status nothing would ever change), and a job caught
// mid-run is reported as interrupted rather than still running.
func TestStatusSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	store := storage.NewJSON(dir)

	// Job A finished; job B was running when the process died.
	doneStore := &Service{store: store, statuses: map[string]*JobStatus{
		"a": {ID: "a", Type: "gate", Status: "done", Progress: 100},
		"b": {ID: "b", Type: "gate", Status: "running", Progress: 40},
	}}
	for _, id := range []string{"a", "b"} {
		if err := store.Save(id, &Job{ID: id, Type: "gate"}); err != nil {
			t.Fatalf("save job %s: %v", id, err)
		}
		doneStore.persistStatus(id)
	}

	// A fresh service over the same directory = a restart.
	s := New(store)
	a, err := s.Status("a")
	if err != nil {
		t.Fatalf("status a: %v", err)
	}
	if a.Status != "done" || a.Progress != 100 {
		t.Errorf("job a = %+v, want done/100 (the sidecar was not restored)", a)
	}
	b, err := s.Status("b")
	if err != nil {
		t.Fatalf("status b: %v", err)
	}
	if b.Status != "failed" {
		t.Errorf("job b = %q, want failed (an interrupted run must not look alive)", b.Status)
	}
	if b.Error == "" {
		t.Error("interrupted job has no message")
	}

	// The status sidecars must not be enumerated as jobs.
	for _, st := range s.List() {
		if st.ID != "a" && st.ID != "b" {
			t.Errorf("phantom job %q appeared after restart", st.ID)
		}
	}
}
