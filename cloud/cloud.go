// Package cloud holds what the hosted OpenPersona service shares between
// its parts: API-key principals and scopes, async jobs, and usage records.
// The HTTP server (package server) depends on these interfaces; package
// registry implements them on Postgres (Neon), and this package has
// in-memory versions for a single process and for tests.
package cloud

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"sync"
	"time"
)

// Scopes an API key can hold.
const (
	ScopeKnowledge = "knowledge" // retrieve knowledge and system prompts; no LLM call
	ScopeChat      = "chat"      // answers: sync, streaming, async jobs, MCP ask
	ScopeDownload  = "download"  // fetch the pack file to run it locally
)

// AllScopes is every scope.
var AllScopes = []string{ScopeKnowledge, ScopeChat, ScopeDownload}

// Errors shared by the implementations.
var (
	ErrUnauthorized = errors.New("missing or invalid API key")
	ErrNotFound     = errors.New("not found")
)

// Principal is the caller an API key identifies.
type Principal struct {
	KeyID    string   `json:"key_id"`
	Owner    string   `json:"owner"`
	Personas []string `json:"personas"` // persona ids, or "*" for all
	Scopes   []string `json:"scopes"`
}

// Can reports whether p may use scope on personaID ("" checks the scope only).
func (p *Principal) Can(scope, personaID string) bool {
	if p == nil || !slices.Contains(p.Scopes, scope) {
		return false
	}
	return personaID == "" || slices.Contains(p.Personas, "*") || slices.Contains(p.Personas, personaID)
}

// Authenticator turns a bearer token into a principal, or returns
// ErrUnauthorized.
type Authenticator interface {
	Authenticate(ctx context.Context, token string) (*Principal, error)
}

// StaticKey is a single key with every scope on every persona, for a
// self-hosted server with one client.
type StaticKey string

// Authenticate implements Authenticator.
func (k StaticKey) Authenticate(_ context.Context, token string) (*Principal, error) {
	if k == "" || subtle.ConstantTimeCompare([]byte(token), []byte(k)) != 1 {
		return nil, ErrUnauthorized
	}
	return &Principal{KeyID: "static", Owner: "static", Personas: []string{"*"}, Scopes: AllScopes}, nil
}

// Open lets every request through with every scope. Use it only for a
// server bound to localhost or behind another authenticating layer.
type Open struct{}

// Authenticate implements Authenticator.
func (Open) Authenticate(context.Context, string) (*Principal, error) {
	return &Principal{KeyID: "open", Owner: "open", Personas: []string{"*"}, Scopes: AllScopes}, nil
}

// KeyPrefix starts every generated API key, so leaked keys are easy to
// recognise in logs and by secret scanners.
const KeyPrefix = "opk_"

// NewKey generates an API key. Store only HashKey(key); show the key once.
func NewKey() string {
	b := make([]byte, 32)
	rand.Read(b)
	return KeyPrefix + base64.RawURLEncoding.EncodeToString(b)
}

// HashKey is the stored form of a key.
func HashKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

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
	Op               string // knowledge, chat, stream, job, mcp, download
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

// AnyOf accepts a token that any of its authenticators accepts, in order:
// for example an operator's StaticKey next to the customer keys in the
// database.
type AnyOf []Authenticator

// Authenticate implements Authenticator.
func (a AnyOf) Authenticate(ctx context.Context, token string) (*Principal, error) {
	for _, x := range a {
		p, err := x.Authenticate(ctx, token)
		if err == nil || !errors.Is(err, ErrUnauthorized) {
			return p, err
		}
	}
	return nil, ErrUnauthorized
}
