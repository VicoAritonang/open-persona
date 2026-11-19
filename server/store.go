package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"sync"
	"time"
)

// NewID returns a random identifier with a prefix, such as "job_3f9a...".
func NewID(prefix string) string {
	b := make([]byte, 12)
	rand.Read(b)
	return prefix + hex.EncodeToString(b)
}

// Job statuses.
const (
	JobQueued    = "queued"
	JobRunning   = "running"
	JobSucceeded = "succeeded"
	JobFailed    = "failed"
)

// Job is one async persona run.
type Job struct {
	ID         string          `json:"id"`
	KeyID      string          `json:"-"`
	PersonaID  string          `json:"persona"`
	Kind       string          `json:"kind"`
	Status     string          `json:"status"`
	Request    json.RawMessage `json:"request,omitempty"`
	Result     json.RawMessage `json:"result,omitempty"`
	Error      string          `json:"error,omitempty"`
	Attempts   int             `json:"attempts"`
	CreatedAt  time.Time       `json:"created_at"`
	StartedAt  *time.Time      `json:"started_at,omitempty"`
	FinishedAt *time.Time      `json:"finished_at,omitempty"`
}

// Done reports whether the job has finished, either way.
func (j *Job) Done() bool { return j.Status == JobSucceeded || j.Status == JobFailed }

// JobStore persists jobs.
type JobStore interface {
	// CreateJob stores j as queued and fills in its ID and CreatedAt.
	CreateJob(ctx context.Context, j *Job) error
	// GetJob returns the job, or ErrNotFound.
	GetJob(ctx context.Context, id string) (*Job, error)
	// ClaimJob marks a queued job (or one running for longer than stale,
	// whose worker died) as running and returns it. It returns nil, nil
	// when the job is not claimable: already done, or running elsewhere.
	ClaimJob(ctx context.Context, id string, stale time.Duration) (*Job, error)
	// FinishJob records the outcome: result on success, errMsg on failure.
	FinishJob(ctx context.Context, id string, result json.RawMessage, errMsg string) error
}

// Usage is one billable operation.
type Usage struct {
	KeyID            string
	PersonaID        string
	Op               string // knowledge, chat, stream, job, mcp_knowledge, mcp_ask, download
	PromptTokens     int64
	CompletionTokens int64
	Calls            int64
	Duration         time.Duration
}

// UsageRecorder stores usage.
type UsageRecorder interface {
	RecordUsage(ctx context.Context, u Usage) error
}

// MemoryJobs is a JobStore in process memory: a single server, or tests.
type MemoryJobs struct {
	mu   sync.Mutex
	jobs map[string]*Job
}

// CreateJob implements JobStore.
func (m *MemoryJobs) CreateJob(_ context.Context, j *Job) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.jobs == nil {
		m.jobs = make(map[string]*Job)
	}
	j.ID, j.Status, j.CreatedAt = NewID("job_"), JobQueued, time.Now().UTC()
	c := *j
	m.jobs[j.ID] = &c
	return nil
}

// GetJob implements JobStore.
func (m *MemoryJobs) GetJob(_ context.Context, id string) (*Job, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	j, ok := m.jobs[id]
	if !ok {
		return nil, ErrNotFound
	}
	c := *j
	return &c, nil
}

// ClaimJob implements JobStore.
func (m *MemoryJobs) ClaimJob(_ context.Context, id string, stale time.Duration) (*Job, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	j, ok := m.jobs[id]
	if !ok {
		return nil, ErrNotFound
	}
	now := time.Now().UTC()
	if j.Status == JobQueued || (j.Status == JobRunning && j.StartedAt != nil && now.Sub(*j.StartedAt) > stale) {
		j.Status, j.StartedAt = JobRunning, &now
		j.Attempts++
		c := *j
		return &c, nil
	}
	return nil, nil
}

// FinishJob implements JobStore.
func (m *MemoryJobs) FinishJob(_ context.Context, id string, result json.RawMessage, errMsg string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	j, ok := m.jobs[id]
	if !ok {
		return ErrNotFound
	}
	now := time.Now().UTC()
	j.Status, j.Result, j.Error, j.FinishedAt = JobSucceeded, result, errMsg, &now
	if errMsg != "" {
		j.Status = JobFailed
	}
	return nil
}
