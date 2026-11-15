// Package graph is the storage kernel shared by the memory and persona
// engines: a property graph (nodes + edges) in a single SQLite file, with a
// BM25 full-text index over every node and an optional vector column.
//
// It is pure Go (no cgo), so the whole stack ships as one static binary.
package graph

import (
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"

	"go.vicoworks.com/openpersona/internal/textutil"
)

// Kind labels what a node is. The kernel does not interpret kinds; engines
// define their own. The ones below are shared.
type Kind string

const (
	KindEpisode Kind = "episode" // raw message or document chunk, stored verbatim
	KindFact    Kind = "fact"    // atomic statement extracted from episodes
	KindEntity  Kind = "entity"  // a named thing that facts and units mention
)

// Edge relations used by the built-in engines.
const (
	RelMentions = "mentions" // fact/unit -> entity
	RelSource   = "source"   // fact/unit -> episode it was derived from
)

// ErrReadOnly is returned by every write on a store opened read-only.
var ErrReadOnly = errors.New("graph: store is read-only")

// Node is a vertex in the graph. Title and Body are full-text indexed.
type Node struct {
	ID    int64
	Scope string // namespace: a user, a session, a persona
	Kind  Kind
	Title string
	Body  string
	Key   string // identity within (scope, kind); "" means no deduplication
	Meta  string // free-form, not indexed

	CreatedAt time.Time // when the system learned it (transaction time)
	ValidAt   time.Time // when it became true in the world (valid time)
	InvalidAt time.Time // when it stopped being true; zero = still valid

	SupersededBy int64 // node that replaced this one, if any
}

// Valid reports whether the node has not been invalidated.
func (n *Node) Valid() bool { return n.InvalidAt.IsZero() }

// Edge is a directed, typed link.
type Edge struct {
	Src, Dst int64
	Rel      string
	Weight   float64
}

// Hit is a node with a relevance score (higher is better).
type Hit struct {
	Node  *Node
	Score float64
}

// Store is a graph in one SQLite file. It is safe for concurrent use.
type Store struct {
	db       *sql.DB
	path     string
	readOnly bool
	analyze  textutil.Analyzer
	wmu      sync.Mutex // SQLite allows one writer; serialize in-process
}

// Analyzer expands one lowercase token into the terms that are indexed and
// searched: the token itself plus any normalized forms (stems, synonyms).
type Analyzer func(token string) []string

// Options configures Open.
type Options struct {
	ReadOnly bool
	// Analyzer customizes text analysis for your language or domain. It must
	// be the same every time a graph is opened, since it applies to both
	// indexing and queries. The default handles Indonesian affixes; English
	// stemming is always applied on top.
	Analyzer Analyzer
}

const schema = `
CREATE TABLE IF NOT EXISTS nodes(
  id INTEGER PRIMARY KEY,
  scope TEXT NOT NULL,
  kind TEXT NOT NULL,
  title TEXT NOT NULL DEFAULT '',
  body TEXT NOT NULL DEFAULT '',
  key TEXT NOT NULL DEFAULT '',
  meta TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL,
  valid_at INTEGER NOT NULL DEFAULT 0,
  invalid_at INTEGER NOT NULL DEFAULT 0,
  superseded_by INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS nodes_scope_kind ON nodes(scope, kind, valid_at);
CREATE INDEX IF NOT EXISTS nodes_key ON nodes(scope, kind, key) WHERE key != '';

CREATE TABLE IF NOT EXISTS edges(
  src INTEGER NOT NULL,
  dst INTEGER NOT NULL,
  rel TEXT NOT NULL,
  weight REAL NOT NULL DEFAULT 1,
  PRIMARY KEY(src, dst, rel)
) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS edges_dst ON edges(dst, rel);

-- The index stores only terms (contentless): text is analyzed in Go before
-- it is indexed, so the analyzer is pluggable, and the Porter stemmer then
-- handles English inflection. scope is indexed too, so a query is confined
-- to one namespace inside the index instead of matching every tenant and
-- filtering afterwards.
CREATE VIRTUAL TABLE IF NOT EXISTS nodes_fts USING fts5(
  scope, title, body,
  content='', contentless_delete=1,
  tokenize='porter unicode61 remove_diacritics 2'
);

CREATE TABLE IF NOT EXISTS vectors(
  node_id INTEGER PRIMARY KEY,
  vec BLOB NOT NULL
);
CREATE TABLE IF NOT EXISTS meta(k TEXT PRIMARY KEY, v TEXT NOT NULL);
`

// Open opens (and, unless read-only, creates) the graph at path.
func Open(path string, opts Options) (*Store, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	dsn := "file:" + filepath.ToSlash(abs) + "?_pragma=busy_timeout(5000)"
	if opts.ReadOnly {
		dsn += "&mode=ro&_pragma=query_only(1)"
	} else {
		dsn += "&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)"
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	s := &Store{db: db, path: abs, readOnly: opts.ReadOnly, analyze: textutil.Variants}
	if opts.Analyzer != nil {
		s.analyze = textutil.Analyzer(opts.Analyzer)
	}
	if opts.ReadOnly {
		if err := db.Ping(); err != nil {
			db.Close()
			return nil, fmt.Errorf("graph: open %s: %w", path, err)
		}
		return s, nil
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("graph: init schema: %w", err)
	}
	if v, _ := s.GetMeta(context.Background(), "frozen"); v == "1" {
		db.Close()
		return nil, fmt.Errorf("graph: %s is frozen; open it read-only", path)
	}
	return s, nil
}

// Close releases the database.
func (s *Store) Close() error { return s.db.Close() }

// ReadOnly reports whether the store rejects writes.
func (s *Store) ReadOnly() bool { return s.readOnly }

// Freeze compacts the file, marks it frozen and closes the store. A frozen
// graph is a single self-contained file that can only be opened read-only.
func (s *Store) Freeze(ctx context.Context) error {
	if s.readOnly {
		return ErrReadOnly
	}
	if err := s.SetMeta(ctx, "frozen", "1"); err != nil {
		return err
	}
	if err := s.db.Close(); err != nil {
		return err
	}
	// Reopen on a single connection: leaving WAL mode needs exclusive access.
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(s.path)+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	for _, q := range []string{
		`INSERT INTO nodes_fts(nodes_fts) VALUES ('optimize')`,
		`PRAGMA wal_checkpoint(TRUNCATE)`,
		`PRAGMA journal_mode=DELETE`,
		`VACUUM`,
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("graph: freeze (%s): %w", q, err)
		}
	}
	return nil
}

// SetFileFormat stamps the file header with an application ID and a format
// version (SQLite's application_id and user_version). Engines that ship a
// graph as a file use it so the file identifies itself; Freeze keeps both.
func (s *Store) SetFileFormat(ctx context.Context, appID, version int32) error {
	if s.readOnly {
		return ErrReadOnly
	}
	s.wmu.Lock()
	defer s.wmu.Unlock()
	_, err := s.db.ExecContext(ctx, fmt.Sprintf(`PRAGMA application_id = %d; PRAGMA user_version = %d`, appID, version))
	return err
}

// FileFormat returns the application ID and format version stamped by
// SetFileFormat (both 0 when the file was never stamped).
func (s *Store) FileFormat(ctx context.Context) (appID, version int32, err error) {
	if err = s.db.QueryRowContext(ctx, `PRAGMA application_id`).Scan(&appID); err != nil {
		return 0, 0, err
	}
	err = s.db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version)
	return appID, version, err
}

func unix(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

func fromUnix(v int64) time.Time {
	if v == 0 {
		return time.Time{}
	}
	return time.Unix(v, 0).UTC()
}

const nodeCols = `n.id, n.scope, n.kind, n.title, n.body, n.key, n.meta, n.created_at, n.valid_at, n.invalid_at, n.superseded_by`

type scanner interface{ Scan(dest ...any) error }

func scanNode(row scanner, extra ...any) (*Node, error) {
	var n Node
	var created, valid, invalid int64
	dest := append([]any{&n.ID, &n.Scope, &n.Kind, &n.Title, &n.Body, &n.Key, &n.Meta, &created, &valid, &invalid, &n.SupersededBy}, extra...)
	if err := row.Scan(dest...); err != nil {
		return nil, err
	}
	n.CreatedAt, n.ValidAt, n.InvalidAt = fromUnix(created), fromUnix(valid), fromUnix(invalid)
	return &n, nil
}

// Insert writes nodes and edges in one transaction and fills in node IDs.
//
// A node with a non-empty Key that already exists (and is still valid) in its
// (scope, kind) is not inserted again; it receives the existing ID. A key
// whose node was invalidated can be asserted again as a new node, so a fact
// that becomes true a second time is recorded. An edge may refer to a node of
// the same batch before it has an ID by using the negative index
// -(i+1) of that node in the nodes slice.
func (s *Store) Insert(ctx context.Context, nodes []*Node, edges []Edge) error {
	if s.readOnly {
		return ErrReadOnly
	}
	s.wmu.Lock()
	defer s.wmu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	now := time.Now().UTC()
	for _, n := range nodes {
		if n.Scope == "" || n.Kind == "" {
			return errors.New("graph: node needs scope and kind")
		}
		if n.Key != "" {
			var id int64
			err := tx.QueryRowContext(ctx, `SELECT id FROM nodes WHERE scope=? AND kind=? AND key=? AND invalid_at=0 LIMIT 1`, n.Scope, n.Kind, n.Key).Scan(&id)
			if err == nil {
				n.ID = id
				continue
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
		}
		if n.CreatedAt.IsZero() {
			n.CreatedAt = now
		}
		if n.ValidAt.IsZero() {
			n.ValidAt = n.CreatedAt
		}
		res, err := tx.ExecContext(ctx,
			`INSERT INTO nodes(scope, kind, title, body, key, meta, created_at, valid_at, invalid_at, superseded_by) VALUES (?,?,?,?,?,?,?,?,?,?)`,
			n.Scope, string(n.Kind), n.Title, n.Body, n.Key, n.Meta, unix(n.CreatedAt), unix(n.ValidAt), unix(n.InvalidAt), n.SupersededBy)
		if err != nil {
			return err
		}
		if n.ID, err = res.LastInsertId(); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO nodes_fts(rowid, scope, title, body) VALUES (?,?,?,?)`,
			n.ID, n.Scope, textutil.IndexText(n.Title, s.analyze), textutil.IndexText(n.Body, s.analyze)); err != nil {
			return err
		}
	}
	resolve := func(id int64) (int64, error) {
		if id >= 0 {
			return id, nil
		}
		i := int(-id - 1)
		if i >= len(nodes) {
			return 0, fmt.Errorf("graph: edge refers to node index %d of %d", i, len(nodes))
		}
		return nodes[i].ID, nil
	}
	for _, e := range edges {
		src, err := resolve(e.Src)
		if err != nil {
			return err
		}
		dst, err := resolve(e.Dst)
		if err != nil {
			return err
		}
		w := e.Weight
		if w == 0 {
			w = 1
		}
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO edges(src, dst, rel, weight) VALUES (?,?,?,?)`, src, dst, e.Rel, w); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Ref returns the placeholder ID for the i-th node of an Insert batch.
func Ref(i int) int64 { return int64(-(i + 1)) }

// Invalidate marks nodes as no longer true from time at, recording the node
// that replaced them. History is kept: nothing is deleted. Nodes that are
// already invalid, or belong to another scope, are left untouched.
func (s *Store) Invalidate(ctx context.Context, scope string, ids []int64, at time.Time, supersededBy int64) error {
	if s.readOnly {
		return ErrReadOnly
	}
	if len(ids) == 0 {
		return nil
	}
	s.wmu.Lock()
	defer s.wmu.Unlock()
	args := []any{unix(at), supersededBy, scope}
	for _, id := range ids {
		args = append(args, id)
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE nodes SET invalid_at=?, superseded_by=? WHERE scope=? AND invalid_at=0 AND id IN (`+placeholders(len(ids))+`)`, args...)
	return err
}

func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.Repeat("?,", n-1) + "?"
}

func kindArgs(kinds []Kind) []any {
	out := make([]any, len(kinds))
	for i, k := range kinds {
		out[i] = string(k)
	}
	return out
}

// SearchOptions tunes Search.
type SearchOptions struct {
	Limit          int
	IncludeInvalid bool    // also return invalidated nodes
	TitleWeight    float64 // BM25 weight of the title column (default 2)
}

// Search ranks the nodes of scope with the given kinds against free-text
// query using BM25. It needs no LLM and no embeddings.
func (s *Store) Search(ctx context.Context, scope string, kinds []Kind, query string, opt SearchOptions) ([]Hit, error) {
	q := textutil.FTSQuery(query, s.analyze)
	if q == "" || len(kinds) == 0 {
		return nil, nil
	}
	if opt.Limit <= 0 {
		opt.Limit = 20
	}
	if opt.TitleWeight <= 0 {
		opt.TitleWeight = 2
	}
	match := "{title body} : (" + q + ")"
	if len(textutil.Tokenize(scope)) > 0 {
		match = "scope : " + textutil.FTSPhrase(scope) + " AND " + match
	}
	sqlq := `SELECT ` + nodeCols + `, -bm25(nodes_fts, 0.0, ?, 1.0) AS score
FROM nodes_fts JOIN nodes n ON n.id = nodes_fts.rowid
WHERE nodes_fts MATCH ? AND n.scope = ? AND n.kind IN (` + placeholders(len(kinds)) + `)`
	if !opt.IncludeInvalid {
		sqlq += ` AND n.invalid_at = 0`
	}
	sqlq += ` ORDER BY score DESC LIMIT ?`
	args := append([]any{opt.TitleWeight, match, scope}, kindArgs(kinds)...)
	args = append(args, opt.Limit)

	rows, err := s.db.QueryContext(ctx, sqlq, args...)
	if err != nil {
		return nil, fmt.Errorf("graph: search: %w", err)
	}
	defer rows.Close()
	var hits []Hit
	for rows.Next() {
		var score float64
		n, err := scanNode(rows, &score)
		if err != nil {
			return nil, err
		}
		hits = append(hits, Hit{Node: n, Score: score})
	}
	return hits, rows.Err()
}

// Link is a node reached over an edge, with the node it was reached from.
type Link struct {
	From int64
	Node *Node
}

// Incoming returns the nodes that point at any of ids over rel (for example
// the facts that mention an entity), newest first, at most perID each.
func (s *Store) Incoming(ctx context.Context, ids []int64, rel string, perID int) ([]Link, error) {
	return s.linked(ctx, ids, rel, perID, "e.dst", "e.src")
}

// Outgoing returns the nodes that any of ids point at over rel (for example
// the episodes a fact was derived from), newest first, at most perID each.
func (s *Store) Outgoing(ctx context.Context, ids []int64, rel string, perID int) ([]Link, error) {
	return s.linked(ctx, ids, rel, perID, "e.src", "e.dst")
}

func (s *Store) linked(ctx context.Context, ids []int64, rel string, perID int, fromCol, toCol string) ([]Link, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	if perID <= 0 {
		perID = 20
	}
	args := make([]any, 0, len(ids)+1)
	for _, id := range ids {
		args = append(args, id)
	}
	args = append(args, rel)
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+nodeCols+`, `+fromCol+` FROM edges e JOIN nodes n ON n.id = `+toCol+`
WHERE `+fromCol+` IN (`+placeholders(len(ids))+`) AND e.rel = ?
ORDER BY n.valid_at DESC, n.id DESC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	count := make(map[int64]int)
	var out []Link
	for rows.Next() {
		var from int64
		n, err := scanNode(rows, &from)
		if err != nil {
			return nil, err
		}
		if count[from] >= perID {
			continue
		}
		count[from]++
		out = append(out, Link{From: from, Node: n})
	}
	return out, rows.Err()
}

// InDegree returns, for each of ids, how many nodes point at it over rel.
func (s *Store) InDegree(ctx context.Context, ids []int64, rel string) (map[int64]int, error) {
	out := make(map[int64]int, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	args := make([]any, 0, len(ids)+1)
	for _, id := range ids {
		args = append(args, id)
	}
	args = append(args, rel)
	rows, err := s.db.QueryContext(ctx, `SELECT dst, COUNT(*) FROM edges WHERE dst IN (`+placeholders(len(ids))+`) AND rel = ? GROUP BY dst`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			return nil, err
		}
		out[id] = n
	}
	return out, rows.Err()
}

// Get returns the nodes with the given IDs (order not guaranteed).
func (s *Store) Get(ctx context.Context, ids []int64) ([]*Node, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	return s.queryNodes(ctx, `SELECT `+nodeCols+` FROM nodes n WHERE n.id IN (`+placeholders(len(ids))+`)`, args...)
}

// List returns nodes of one kind in a scope, newest first.
func (s *Store) List(ctx context.Context, scope string, kind Kind, limit int) ([]*Node, error) {
	if limit <= 0 {
		limit = 1000
	}
	return s.queryNodes(ctx, `SELECT `+nodeCols+` FROM nodes n WHERE n.scope=? AND n.kind=? ORDER BY n.valid_at DESC, n.id DESC LIMIT ?`, scope, string(kind), limit)
}

func (s *Store) queryNodes(ctx context.Context, q string, args ...any) ([]*Node, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Node
	for rows.Next() {
		n, err := scanNode(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// Count returns the number of nodes per kind in scope ("" = all scopes).
func (s *Store) Count(ctx context.Context, scope string) (map[Kind]int, error) {
	q, args := `SELECT kind, COUNT(*) FROM nodes GROUP BY kind`, []any{}
	if scope != "" {
		q, args = `SELECT kind, COUNT(*) FROM nodes WHERE scope=? GROUP BY kind`, []any{scope}
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[Kind]int)
	for rows.Next() {
		var k string
		var c int
		if err := rows.Scan(&k, &c); err != nil {
			return nil, err
		}
		out[Kind(k)] = c
	}
	return out, rows.Err()
}

// SetMeta stores a key/value pair describing the graph itself.
func (s *Store) SetMeta(ctx context.Context, k, v string) error {
	if s.readOnly {
		return ErrReadOnly
	}
	s.wmu.Lock()
	defer s.wmu.Unlock()
	_, err := s.db.ExecContext(ctx, `INSERT INTO meta(k, v) VALUES (?, ?) ON CONFLICT(k) DO UPDATE SET v=excluded.v`, k, v)
	return err
}

// GetMeta returns the value for k, or "" if unset.
func (s *Store) GetMeta(ctx context.Context, k string) (string, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT v FROM meta WHERE k=?`, k).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

// PutVectors stores one embedding per node ID.
func (s *Store) PutVectors(ctx context.Context, vecs map[int64][]float32) error {
	if s.readOnly {
		return ErrReadOnly
	}
	if len(vecs) == 0 {
		return nil
	}
	s.wmu.Lock()
	defer s.wmu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for id, v := range vecs {
		if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO vectors(node_id, vec) VALUES (?, ?)`, id, encodeVec(v)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// SearchVector ranks nodes by cosine similarity to query. It scans the
// vectors of one scope, which is exact and fast enough up to roughly 10^5
// vectors per scope; plug in an ANN index behind this method beyond that.
func (s *Store) SearchVector(ctx context.Context, scope string, kinds []Kind, query []float32, opt SearchOptions) ([]Hit, error) {
	if len(query) == 0 || len(kinds) == 0 {
		return nil, nil
	}
	if opt.Limit <= 0 {
		opt.Limit = 20
	}
	q := `SELECT v.node_id, v.vec FROM vectors v JOIN nodes n ON n.id = v.node_id WHERE n.scope = ? AND n.kind IN (` + placeholders(len(kinds)) + `)`
	if !opt.IncludeInvalid {
		q += ` AND n.invalid_at = 0`
	}
	rows, err := s.db.QueryContext(ctx, q, append([]any{scope}, kindArgs(kinds)...)...)
	if err != nil {
		return nil, err
	}
	type scored struct {
		id    int64
		score float64
	}
	var all []scored
	qn := norm(query)
	for rows.Next() {
		var id int64
		var blob []byte
		if err := rows.Scan(&id, &blob); err != nil {
			rows.Close()
			return nil, err
		}
		v := decodeVec(blob)
		if len(v) != len(query) {
			continue
		}
		var dot float64
		for i := range v {
			dot += float64(v[i]) * float64(query[i])
		}
		if d := qn * norm(v); d > 0 {
			all = append(all, scored{id, dot / d})
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Slice(all, func(i, j int) bool { return all[i].score > all[j].score })
	if len(all) > opt.Limit {
		all = all[:opt.Limit]
	}
	ids := make([]int64, len(all))
	for i, a := range all {
		ids[i] = a.id
	}
	nodes, err := s.Get(ctx, ids)
	if err != nil {
		return nil, err
	}
	byID := make(map[int64]*Node, len(nodes))
	for _, n := range nodes {
		byID[n.ID] = n
	}
	hits := make([]Hit, 0, len(all))
	for _, a := range all {
		if n := byID[a.id]; n != nil {
			hits = append(hits, Hit{Node: n, Score: a.score})
		}
	}
	return hits, nil
}

func norm(v []float32) float64 {
	var s float64
	for _, x := range v {
		s += float64(x) * float64(x)
	}
	return math.Sqrt(s)
}

func encodeVec(v []float32) []byte {
	b := make([]byte, 4*len(v))
	for i, x := range v {
		binary.LittleEndian.PutUint32(b[4*i:], math.Float32bits(x))
	}
	return b
}

func decodeVec(b []byte) []float32 {
	v := make([]float32, len(b)/4)
	for i := range v {
		v[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[4*i:]))
	}
	return v
}
