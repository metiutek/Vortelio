package downloads

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"time"
)

// State of a download job.
type State string

const (
	Queued      State = "queued"
	Running     State = "running"
	Done        State = "done"
	Failed      State = "failed"
	Cancelled   State = "cancelled"
	Interrupted State = "interrupted" // Vortelio stopped while it ran; resumable
)

// Finished reports whether the state is terminal.
func (s State) Finished() bool { return s == Done || s == Failed || s == Cancelled }

// Job is the public snapshot of one download (a model, an engine, …).
type Job struct {
	ID        string            `json:"id"`
	Kind      string            `json:"kind"` // "model" | "engine"
	Key       string            `json:"key"`  // identity: one job per key at a time
	Title     string            `json:"title"`
	State     State             `json:"state"`
	Phase     string            `json:"phase,omitempty"` // what it is doing right now
	Done      int64             `json:"done"`
	Total     int64             `json:"total"` // 0 = unknown (indeterminate)
	Speed     float64           `json:"speed"` // bytes per second
	Error     string            `json:"error,omitempty"`
	Meta      map[string]string `json:"meta,omitempty"`
	CreatedAt time.Time         `json:"created_at"`
	UpdatedAt time.Time         `json:"updated_at"`
}

// Task does the work of a job. It must honour ctx and report through p.
type Task func(ctx context.Context, p *Progress) error

// Resumer rebuilds the task of a job of its kind after a restart.
type Resumer func(key string, meta map[string]string) (Task, error)

type entry struct {
	job    Job
	task   Task
	cancel context.CancelFunc
	waitCh chan struct{} // closed when the run ends
	// speed sampling
	lastAt   time.Time
	lastDone int64
}

// Manager runs download jobs in the background, independent of any HTTP
// request: closing the browser never stops a download.
type Manager struct {
	mu        sync.Mutex
	jobs      map[string]*entry
	byKey     map[string]string
	resumers  map[string]Resumer
	subs      map[chan Job]struct{}
	path      string
	slots     chan struct{}
	seq       int
	lastSave  time.Time
	saveTimer *time.Timer
}

// MaxParallel bounds how many jobs transfer at once; the rest queue.
var MaxParallel = 2

// NewManager creates a manager persisting its jobs to path ("" = in memory).
// Jobs that were running when the process last stopped become Interrupted.
func NewManager(path string) *Manager {
	m := &Manager{
		jobs:     map[string]*entry{},
		byKey:    map[string]string{},
		resumers: map[string]Resumer{},
		subs:     map[chan Job]struct{}{},
		path:     path,
		slots:    make(chan struct{}, MaxParallel),
	}
	m.load()
	return m
}

var (
	defaultMu  sync.Mutex
	defaultMgr *Manager
)

// Default is the process-wide manager, persisted in <home>/downloads.json.
// SetHome must be called first; otherwise it lives in memory only.
func Default() *Manager {
	defaultMu.Lock()
	defer defaultMu.Unlock()
	if defaultMgr == nil {
		defaultMgr = NewManager("")
	}
	return defaultMgr
}

// SetHome binds the default manager to a state directory. Call once at start.
func SetHome(dir string) *Manager {
	defaultMu.Lock()
	defer defaultMu.Unlock()
	if defaultMgr == nil {
		defaultMgr = NewManager(filepath.Join(dir, "downloads.json"))
	}
	return defaultMgr
}

// RegisterKind lets jobs of kind be resumed; interrupted jobs of that kind are
// restarted right away.
func (m *Manager) RegisterKind(kind string, r Resumer) {
	m.mu.Lock()
	m.resumers[kind] = r
	var resume []string
	for id, e := range m.jobs {
		if e.job.Kind == kind && e.job.State == Interrupted {
			resume = append(resume, id)
		}
	}
	m.mu.Unlock()
	for _, id := range resume {
		_ = m.Retry(id)
	}
}

// Start begins (or joins) the job for key. When a job for key is already
// queued or running, that job is returned with joined=true and task is ignored.
func (m *Manager) Start(kind, key, title string, meta map[string]string, task Task) (Job, bool) {
	m.mu.Lock()
	if id, ok := m.byKey[key]; ok {
		if e := m.jobs[id]; e != nil && (e.job.State == Running || e.job.State == Queued) {
			j := e.job
			m.mu.Unlock()
			return j, true
		}
	}
	m.seq++
	now := time.Now()
	e := &entry{
		job: Job{
			ID:        strconv.FormatInt(now.UnixNano(), 36) + strconv.Itoa(m.seq),
			Kind:      kind,
			Key:       key,
			Title:     title,
			State:     Queued,
			Meta:      meta,
			CreatedAt: now,
			UpdatedAt: now,
		},
		task: task,
	}
	// Replace an older finished job for the same key.
	if old, ok := m.byKey[key]; ok {
		delete(m.jobs, old)
	}
	m.jobs[e.job.ID] = e
	m.byKey[key] = e.job.ID
	m.launchLocked(e)
	j := e.job
	m.mu.Unlock()
	m.publish(j, true)
	return j, false
}

func (m *Manager) launchLocked(e *entry) {
	ctx, cancel := context.WithCancel(context.Background())
	e.cancel = cancel
	e.waitCh = make(chan struct{})
	e.job.State = Queued
	e.job.Error = ""
	e.job.Speed = 0
	go m.run(ctx, e)
}

func (m *Manager) run(ctx context.Context, e *entry) {
	defer close(e.waitCh)
	select {
	case m.slots <- struct{}{}:
	case <-ctx.Done():
		m.finish(e, ErrCancelled)
		return
	}
	defer func() { <-m.slots }()

	m.mu.Lock()
	e.job.State = Running
	e.job.UpdatedAt = time.Now()
	e.lastAt, e.lastDone = time.Now(), e.job.Done
	j := e.job
	m.mu.Unlock()
	m.publish(j, true)

	var err error
	func() {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("internal error: %v", r)
			}
		}()
		err = e.task(ctx, &Progress{m: m, e: e})
	}()
	if err == nil && ctx.Err() != nil {
		err = ErrCancelled
	}
	m.finish(e, err)
}

func (m *Manager) finish(e *entry, err error) {
	m.mu.Lock()
	switch {
	case err == nil:
		e.job.State = Done
		if e.job.Total > 0 {
			e.job.Done = e.job.Total
		}
		e.job.Phase = ""
	case errors.Is(err, ErrCancelled):
		e.job.State = Cancelled
		e.job.Phase = ""
	default:
		e.job.State = Failed
		e.job.Error = err.Error()
	}
	e.job.Speed = 0
	e.job.UpdatedAt = time.Now()
	j := e.job
	m.mu.Unlock()
	m.publish(j, true)
}

// Cancel stops a queued or running job. Partial files are kept, so Retry
// continues where it stopped.
func (m *Manager) Cancel(id string) error {
	m.mu.Lock()
	e := m.lookup(id)
	if e == nil {
		m.mu.Unlock()
		return errors.New("download not found")
	}
	if e.job.State.Finished() || e.cancel == nil {
		m.mu.Unlock()
		return nil
	}
	cancel := e.cancel
	m.mu.Unlock()
	cancel()
	return nil
}

// Retry restarts a failed, cancelled or interrupted job.
func (m *Manager) Retry(id string) error {
	m.mu.Lock()
	e := m.lookup(id)
	if e == nil {
		m.mu.Unlock()
		return errors.New("download not found")
	}
	if e.job.State == Running || e.job.State == Queued {
		m.mu.Unlock()
		return nil
	}
	if e.task == nil {
		r := m.resumers[e.job.Kind]
		if r == nil {
			m.mu.Unlock()
			return fmt.Errorf("cannot resume a %s download yet", e.job.Kind)
		}
		t, err := r(e.job.Key, e.job.Meta)
		if err != nil {
			e.job.State, e.job.Error = Failed, err.Error()
			j := e.job
			m.mu.Unlock()
			m.publish(j, true)
			return err
		}
		e.task = t
	}
	m.launchLocked(e)
	j := e.job
	m.mu.Unlock()
	m.publish(j, true)
	return nil
}

// Dismiss removes a finished job from the list.
func (m *Manager) Dismiss(id string) error {
	m.mu.Lock()
	e := m.lookup(id)
	if e == nil {
		m.mu.Unlock()
		return errors.New("download not found")
	}
	if e.job.State == Running || e.job.State == Queued {
		m.mu.Unlock()
		return errors.New("cancel the download first")
	}
	delete(m.jobs, e.job.ID)
	if m.byKey[e.job.Key] == e.job.ID {
		delete(m.byKey, e.job.Key)
	}
	j := e.job
	m.mu.Unlock()
	j.State = "dismissed"
	m.publish(j, true)
	return nil
}

// lookup finds a job by id or key. Caller holds m.mu.
func (m *Manager) lookup(idOrKey string) *entry {
	if e, ok := m.jobs[idOrKey]; ok {
		return e
	}
	if id, ok := m.byKey[idOrKey]; ok {
		return m.jobs[id]
	}
	return nil
}

// Get returns the job with the given id or key.
func (m *Manager) Get(idOrKey string) (Job, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e := m.lookup(idOrKey); e != nil {
		return e.job, true
	}
	return Job{}, false
}

// Wait blocks until the job's current run ends or ctx is done, and returns
// its final snapshot.
func (m *Manager) Wait(ctx context.Context, id string) (Job, error) {
	m.mu.Lock()
	e := m.lookup(id)
	if e == nil {
		m.mu.Unlock()
		return Job{}, errors.New("download not found")
	}
	ch := e.waitCh
	m.mu.Unlock()
	if ch != nil {
		select {
		case <-ch:
		case <-ctx.Done():
			return Job{}, ctx.Err()
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return e.job, nil
}

// List returns every job, newest first.
func (m *Manager) List() []Job {
	m.mu.Lock()
	out := make([]Job, 0, len(m.jobs))
	for _, e := range m.jobs {
		out = append(out, e.job)
	}
	m.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out
}

// Subscribe returns a channel of job updates. Slow readers miss intermediate
// progress, never state changes (those block briefly).
func (m *Manager) Subscribe() (<-chan Job, func()) {
	ch := make(chan Job, 64)
	m.mu.Lock()
	m.subs[ch] = struct{}{}
	m.mu.Unlock()
	return ch, func() {
		m.mu.Lock()
		if _, ok := m.subs[ch]; ok {
			delete(m.subs, ch)
			close(ch)
		}
		m.mu.Unlock()
	}
}

func (m *Manager) publish(j Job, important bool) {
	m.mu.Lock()
	for ch := range m.subs {
		if important {
			select {
			case ch <- j:
			case <-time.After(200 * time.Millisecond):
			}
		} else {
			select {
			case ch <- j:
			default:
			}
		}
	}
	m.mu.Unlock()
	m.scheduleSave(important)
}

// Progress is how a task reports what it is doing.
type Progress struct {
	m *Manager
	e *entry
}

// Phase sets the human-readable status line.
func (p *Progress) Phase(msg string) {
	p.m.mu.Lock()
	p.e.job.Phase = msg
	p.e.job.UpdatedAt = time.Now()
	j := p.e.job
	p.m.mu.Unlock()
	p.m.publish(j, false)
}

// SetTotal sets the expected total bytes (0 = unknown).
func (p *Progress) SetTotal(n int64) {
	p.m.mu.Lock()
	p.e.job.Total = n
	j := p.e.job
	p.m.mu.Unlock()
	p.m.publish(j, false)
}

// SetDone sets the bytes completed so far.
func (p *Progress) SetDone(n int64) {
	p.m.mu.Lock()
	e := p.e
	now := time.Now()
	e.job.Done = n
	e.job.UpdatedAt = now
	if dt := now.Sub(e.lastAt).Seconds(); dt >= 1 {
		inst := float64(n-e.lastDone) / dt
		if inst < 0 {
			inst = 0
		}
		if e.job.Speed == 0 {
			e.job.Speed = inst
		} else {
			e.job.Speed = 0.7*e.job.Speed + 0.3*inst
		}
		e.lastAt, e.lastDone = now, n
	}
	j := e.job
	p.m.mu.Unlock()
	p.m.publish(j, false)
}

// SetMeta records a value that is persisted with the job.
func (p *Progress) SetMeta(k, v string) {
	p.m.mu.Lock()
	if p.e.job.Meta == nil {
		p.e.job.Meta = map[string]string{}
	}
	p.e.job.Meta[k] = v
	p.m.mu.Unlock()
}

// ── persistence ─────────────────────────────────────────────────────────────

const keepFinished = 30

func (m *Manager) load() {
	if m.path == "" {
		return
	}
	data, err := os.ReadFile(m.path)
	if err != nil {
		return
	}
	var jobs []Job
	if json.Unmarshal(data, &jobs) != nil {
		return
	}
	for _, j := range jobs {
		if j.State == Running || j.State == Queued {
			j.State = Interrupted
			j.Speed = 0
			j.Phase = "Paused while Vortelio was closed"
		}
		jj := j
		m.jobs[j.ID] = &entry{job: jj}
		m.byKey[j.Key] = j.ID
	}
}

func (m *Manager) scheduleSave(now bool) {
	if m.path == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if now || time.Since(m.lastSave) > 3*time.Second {
		m.saveLocked()
		return
	}
	if m.saveTimer == nil {
		m.saveTimer = time.AfterFunc(3*time.Second, func() {
			m.mu.Lock()
			m.saveTimer = nil
			m.saveLocked()
			m.mu.Unlock()
		})
	}
}

func (m *Manager) saveLocked() {
	m.lastSave = time.Now()
	jobs := make([]Job, 0, len(m.jobs))
	for _, e := range m.jobs {
		jobs = append(jobs, e.job)
	}
	sort.Slice(jobs, func(i, j int) bool { return jobs[i].CreatedAt.After(jobs[j].CreatedAt) })
	// Bound the history: active jobs always stay, finished ones are trimmed.
	kept, finished := jobs[:0], 0
	for _, j := range jobs {
		if j.State.Finished() {
			finished++
			if finished > keepFinished {
				continue
			}
		}
		kept = append(kept, j)
	}
	data, err := json.MarshalIndent(kept, "", "  ")
	if err != nil {
		return
	}
	_ = os.MkdirAll(filepath.Dir(m.path), 0o755)
	tmp := m.path + ".tmp"
	if os.WriteFile(tmp, data, 0o644) == nil {
		_ = renameRetry(tmp, m.path)
	}
}
