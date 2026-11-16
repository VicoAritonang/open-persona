package registry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"go.vicoworks.com/open-persona/cloud"
)

// The registry implements the hosted service's stores on the same database.
var (
	_ cloud.Authenticator = (*Registry)(nil)
	_ cloud.JobStore      = (*Registry)(nil)
	_ cloud.UsageRecorder = (*Registry)(nil)
)

// Key is a stored API key (never the key itself).
type Key struct {
	ID         string     `json:"id"`
	Prefix     string     `json:"prefix"`
	Name       string     `json:"name"`
	Owner      string     `json:"owner"`
	Personas   []string   `json:"personas"`
	Scopes     []string   `json:"scopes"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty"`
}

// KeySpec describes a key to create.
type KeySpec struct {
	Owner    string   // who the key is for (customer, team)
	Name     string   // label, e.g. "production backend"
	Personas []string // persona ids; default all ("*")
	Scopes   []string // default knowledge and chat
}

// CreateKey creates an API key and returns it with its record. The key is
// not stored and cannot be recovered: hand it over now.
func (r *Registry) CreateKey(ctx context.Context, spec KeySpec) (string, *Key, error) {
	if strings.TrimSpace(spec.Owner) == "" {
		return "", nil, errors.New("registry: a key needs an owner")
	}
	if len(spec.Personas) == 0 {
		spec.Personas = []string{"*"}
	}
	if len(spec.Scopes) == 0 {
		spec.Scopes = []string{cloud.ScopeKnowledge, cloud.ScopeChat}
	}
	for _, s := range spec.Scopes {
		if !slices.Contains(cloud.AllScopes, s) {
			return "", nil, fmt.Errorf("registry: unknown scope %q (have %s)", s, strings.Join(cloud.AllScopes, ", "))
		}
	}
	key := cloud.NewKey()
	k := &Key{ID: cloud.NewID("key_"), Prefix: key[:len(cloud.KeyPrefix)+6], Name: spec.Name, Owner: spec.Owner, Personas: spec.Personas, Scopes: spec.Scopes}
	err := r.pool.QueryRow(ctx, r.q(`INSERT INTO {{schema}}.api_keys (id, key_sha256, prefix, name, owner, personas, scopes)
		VALUES ($1,$2,$3,$4,$5,$6,$7) RETURNING created_at`),
		k.ID, cloud.HashKey(key), k.Prefix, k.Name, k.Owner, k.Personas, k.Scopes).Scan(&k.CreatedAt)
	if err != nil {
		return "", nil, fmt.Errorf("registry: create key: %w", err)
	}
	return key, k, nil
}

// Authenticate implements cloud.Authenticator.
func (r *Registry) Authenticate(ctx context.Context, token string) (*cloud.Principal, error) {
	if !strings.HasPrefix(token, cloud.KeyPrefix) {
		return nil, cloud.ErrUnauthorized
	}
	p := &cloud.Principal{}
	// Bump last_used_at at most once a minute, in the same round trip.
	err := r.pool.QueryRow(ctx, r.q(`UPDATE {{schema}}.api_keys
		SET last_used_at = CASE WHEN last_used_at IS NULL OR last_used_at < now() - interval '1 minute' THEN now() ELSE last_used_at END
		WHERE key_sha256 = $1 AND revoked_at IS NULL
		RETURNING id, owner, personas, scopes`), cloud.HashKey(token)).Scan(&p.KeyID, &p.Owner, &p.Personas, &p.Scopes)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, cloud.ErrUnauthorized
	}
	if err != nil {
		return nil, err
	}
	return p, nil
}

// ListKeys lists every key, newest first.
func (r *Registry) ListKeys(ctx context.Context) ([]Key, error) {
	rows, err := r.pool.Query(ctx, r.q(`SELECT id, prefix, name, owner, personas, scopes, created_at, last_used_at, revoked_at
		FROM {{schema}}.api_keys ORDER BY created_at DESC`))
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Key, error) {
		var k Key
		err := row.Scan(&k.ID, &k.Prefix, &k.Name, &k.Owner, &k.Personas, &k.Scopes, &k.CreatedAt, &k.LastUsedAt, &k.RevokedAt)
		return k, err
	})
}

// RevokeKey disables a key. It cannot be undone.
func (r *Registry) RevokeKey(ctx context.Context, id string) error {
	tag, err := r.pool.Exec(ctx, r.q(`UPDATE {{schema}}.api_keys SET revoked_at = now() WHERE id = $1 AND revoked_at IS NULL`), id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: active key %s", ErrNotFound, id)
	}
	return nil
}

const jobCols = `id, key_id, persona_id, kind, status, request, result, coalesce(error, ''), attempts, created_at, started_at, finished_at`

func scanJob(row pgx.Row) (*cloud.Job, error) {
	j := &cloud.Job{}
	var req, res []byte
	err := row.Scan(&j.ID, &j.KeyID, &j.PersonaID, &j.Kind, &j.Status, &req, &res, &j.Error, &j.Attempts, &j.CreatedAt, &j.StartedAt, &j.FinishedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, cloud.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	j.Request, j.Result = req, res
	return j, nil
}

// CreateJob implements cloud.JobStore.
func (r *Registry) CreateJob(ctx context.Context, j *cloud.Job) error {
	j.ID, j.Status = cloud.NewID("job_"), cloud.JobQueued
	if j.Kind == "" {
		j.Kind = "chat"
	}
	return r.pool.QueryRow(ctx, r.q(`INSERT INTO {{schema}}.jobs (id, key_id, persona_id, kind, request) VALUES ($1,$2,$3,$4,$5) RETURNING created_at`),
		j.ID, j.KeyID, j.PersonaID, j.Kind, jsonArg(j.Request)).Scan(&j.CreatedAt)
}

// GetJob implements cloud.JobStore.
func (r *Registry) GetJob(ctx context.Context, id string) (*cloud.Job, error) {
	return scanJob(r.pool.QueryRow(ctx, r.q(`SELECT `+jobCols+` FROM {{schema}}.jobs WHERE id = $1`), id))
}

// ClaimJob implements cloud.JobStore. The conditional UPDATE makes a claim
// atomic, so a retried Lambda event cannot run a job twice at once.
func (r *Registry) ClaimJob(ctx context.Context, id string, stale time.Duration) (*cloud.Job, error) {
	j, err := scanJob(r.pool.QueryRow(ctx, r.q(`UPDATE {{schema}}.jobs SET status = 'running', started_at = now(), attempts = attempts + 1
		WHERE id = $1 AND (status = 'queued' OR (status = 'running' AND started_at < now() - make_interval(secs => $2)))
		RETURNING `+jobCols), id, stale.Seconds()))
	if errors.Is(err, cloud.ErrNotFound) {
		if _, err := r.GetJob(ctx, id); err != nil {
			return nil, err
		}
		return nil, nil
	}
	return j, err
}

// FinishJob implements cloud.JobStore.
func (r *Registry) FinishJob(ctx context.Context, id string, result json.RawMessage, errMsg string) error {
	status := cloud.JobSucceeded
	if errMsg != "" {
		status = cloud.JobFailed
	}
	var res any
	if len(result) > 0 {
		res = jsonArg(result)
	}
	tag, err := r.pool.Exec(ctx, r.q(`UPDATE {{schema}}.jobs SET status = $2, result = $3, error = nullif($4, ''), finished_at = now() WHERE id = $1`),
		id, status, res, errMsg)
	if err == nil && tag.RowsAffected() == 0 {
		err = cloud.ErrNotFound
	}
	return err
}

// RecordUsage implements cloud.UsageRecorder.
func (r *Registry) RecordUsage(ctx context.Context, u cloud.Usage) error {
	_, err := r.pool.Exec(ctx, r.q(`INSERT INTO {{schema}}.usage_events (key_id, persona_id, op, prompt_tokens, completion_tokens, calls, duration_ms)
		VALUES ($1,$2,$3,$4,$5,$6,$7)`), u.KeyID, u.PersonaID, u.Op, u.PromptTokens, u.CompletionTokens, u.Calls, u.Duration.Milliseconds())
	return err
}

// UsageRow is usage summed per key, persona and operation.
type UsageRow struct {
	KeyID            string `json:"key_id"`
	Owner            string `json:"owner"`
	PersonaID        string `json:"persona"`
	Op               string `json:"op"`
	Events           int64  `json:"events"`
	PromptTokens     int64  `json:"prompt_tokens"`
	CompletionTokens int64  `json:"completion_tokens"`
}

// UsageSince sums usage recorded since t.
func (r *Registry) UsageSince(ctx context.Context, t time.Time) ([]UsageRow, error) {
	rows, err := r.pool.Query(ctx, r.q(`SELECT u.key_id, coalesce(k.owner, ''), u.persona_id, u.op, count(*), sum(u.prompt_tokens), sum(u.completion_tokens)
		FROM {{schema}}.usage_events u LEFT JOIN {{schema}}.api_keys k ON k.id = u.key_id
		WHERE u.at >= $1 GROUP BY 1, 2, 3, 4 ORDER BY 2, 3, 4`), t)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (UsageRow, error) {
		var u UsageRow
		err := row.Scan(&u.KeyID, &u.Owner, &u.PersonaID, &u.Op, &u.Events, &u.PromptTokens, &u.CompletionTokens)
		return u, err
	})
}
