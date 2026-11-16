// Package server exposes the memory and persona engines over HTTP and MCP,
// so any backend, in any language, and any MCP-capable agent can use them.
//
// The same handler runs as a self-hosted server (openpersona serve), on a
// small VM, and on AWS Lambda (cmd/openpersona-lambda). What differs is
// wiring: where packs come from (Personas or Catalog), who may call
// (Auth), and where async jobs run (Jobs and Dispatch).
package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"go.vicoworks.com/open-persona/cloud"
	"go.vicoworks.com/open-persona/llm"
	"go.vicoworks.com/open-persona/memory"
	"go.vicoworks.com/open-persona/persona"
	"go.vicoworks.com/open-persona/recall"
)

// Catalog supplies personas on demand. registry.Catalog implements it on
// the Postgres registry; Packs implements it on packs already open.
type Catalog interface {
	// Persona returns the current version of id, or an error wrapping
	// cloud.ErrNotFound.
	Persona(ctx context.Context, id string) (*persona.Persona, error)
	// List describes the available personas without loading them.
	List(ctx context.Context) ([]persona.Spec, error)
}

// Packs is a Catalog over open packs, keyed by persona id.
type Packs map[string]*persona.Persona

// Persona implements Catalog.
func (m Packs) Persona(_ context.Context, id string) (*persona.Persona, error) {
	if p := m[id]; p != nil {
		return p, nil
	}
	return nil, fmt.Errorf("persona %q: %w", id, cloud.ErrNotFound)
}

// List implements Catalog.
func (m Packs) List(context.Context) ([]persona.Spec, error) {
	specs := make([]persona.Spec, 0, len(m))
	for _, p := range m {
		specs = append(specs, p.Spec)
	}
	return specs, nil
}

// Server wires the engines to HTTP handlers. Any field may be nil; the
// endpoints that need it then answer 503.
type Server struct {
	Memory *memory.Engine
	// Personas serves a fixed set of open packs; Catalog serves them on
	// demand. Catalog wins when both are set.
	Personas map[string]*persona.Persona
	Catalog  Catalog
	Chat     llm.Client // model that speaks for personas; streams if it implements llm.Streamer

	// Auth checks the bearer token of every request except /healthz. When
	// nil, APIKey (if set) is the single accepted key; with neither, the
	// server is open (only for localhost or behind another auth layer).
	Auth   cloud.Authenticator
	APIKey string

	// Jobs stores async jobs (POST /v1/personas/{id}/jobs). Dispatch starts
	// a stored job outside the request: on Lambda it invokes the function
	// asynchronously. When Dispatch is nil, jobs run in a goroutine of this
	// process, so the process must outlive them (a server, not a Lambda).
	Jobs     cloud.JobStore
	Dispatch func(ctx context.Context, jobID string) error
	// JobTimeout bounds one job run (default 10 minutes).
	JobTimeout time.Duration

	// AllowOrigins lists the browser origins allowed to call the API (CORS),
	// such as "https://app.example.com" for a frontend on Vercel or
	// Cloudflare Pages; "*" allows any. Empty: no CORS headers.
	AllowOrigins []string

	Usage   cloud.UsageRecorder // optional: one record per billable operation
	Logf    func(format string, args ...any)
	Version string // reported to MCP clients

	shaMu sync.Mutex
	shas  map[string]string // pack path -> sha256
}

// Handler returns the HTTP API.
//
//	GET  /healthz
//	POST /v1/memory/{scope}/messages          store messages
//	POST /v1/memory/{scope}/search            facts, messages and a prompt-ready context block
//	GET  /v1/personas                         personas this key may use
//	GET  /v1/personas/{id}                    one persona's manifest
//	POST /v1/personas/{id}/knowledge          retrieved knowledge and system prompt; no LLM call
//	POST /v1/personas/{id}/prompt             same as knowledge (older name)
//	POST /v1/personas/{id}/chat               answer as the persona; "stream": true answers as server-sent events
//	POST /v1/personas/{id}/jobs               answer asynchronously; returns a job to poll
//	GET  /v1/jobs/{id}                        an async job and, when done, its result
//	GET  /v1/personas/{id}/pack               the pack file, for running it locally (supports Range)
//	POST /mcp                                 Model Context Protocol (streamable HTTP, JSON responses)
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, 200, map[string]string{"status": "ok"}) })
	mux.HandleFunc("POST /v1/memory/{scope}/messages", s.addMessages)
	mux.HandleFunc("POST /v1/memory/{scope}/search", s.searchMemory)
	mux.HandleFunc("GET /v1/personas", s.listPersonas)
	mux.HandleFunc("GET /v1/personas/{name}", s.personaInfo)
	mux.HandleFunc("POST /v1/personas/{name}/knowledge", s.personaKnowledge)
	mux.HandleFunc("POST /v1/personas/{name}/prompt", s.personaKnowledge)
	mux.HandleFunc("POST /v1/personas/{name}/chat", s.personaChat)
	mux.HandleFunc("POST /v1/personas/{name}/jobs", s.submitJob)
	mux.HandleFunc("GET /v1/jobs/{id}", s.getJob)
	mux.HandleFunc("GET /v1/personas/{name}/pack", s.downloadPack)
	mux.HandleFunc("POST /mcp", s.mcp)
	mux.HandleFunc("GET /mcp", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Allow", "POST")
		fail(w, http.StatusMethodNotAllowed, errors.New("this MCP server answers POST with JSON and offers no SSE stream"))
	})
	return s.cors(s.authenticate(mux))
}

// cors answers preflight requests and adds CORS headers for allowed
// origins. Browsers send no credentials cookies here: the API key travels
// in the Authorization header.
func (s *Server) cors(next http.Handler) http.Handler {
	if len(s.AllowOrigins) == 0 {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" && (slices.Contains(s.AllowOrigins, "*") || slices.Contains(s.AllowOrigins, origin)) {
			h := w.Header()
			h.Set("Access-Control-Allow-Origin", origin)
			h.Add("Vary", "Origin")
			h.Set("Access-Control-Expose-Headers", "Location, Retry-After, X-Pack-SHA256, X-Persona-Id, X-Persona-Version, Content-Range")
			if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
				h.Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
				h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Accept, Range, X-API-Key, Mcp-Protocol-Version, Mcp-Session-Id")
				h.Set("Access-Control-Max-Age", "600")
				w.WriteHeader(http.StatusNoContent)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

type principalKey struct{}

func principalOf(ctx context.Context) *cloud.Principal {
	p, _ := ctx.Value(principalKey{}).(*cloud.Principal)
	return p
}

func (s *Server) authenticator() cloud.Authenticator {
	switch {
	case s.Auth != nil:
		return s.Auth
	case s.APIKey != "":
		return cloud.StaticKey(s.APIKey)
	}
	return cloud.Open{}
}

func (s *Server) authenticate(next http.Handler) http.Handler {
	auth := s.authenticator()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}
		token := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		if token == "" {
			token = r.Header.Get("X-API-Key")
		}
		p, err := auth.Authenticate(r.Context(), token)
		if err != nil {
			if !errors.Is(err, cloud.ErrUnauthorized) {
				s.logf("auth: %v", err)
				fail(w, http.StatusServiceUnavailable, errors.New("authentication is unavailable, retry"))
				return
			}
			fail(w, http.StatusUnauthorized, err)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), principalKey{}, p)))
	})
}

func (s *Server) logf(format string, args ...any) {
	if s.Logf != nil {
		s.Logf(format, args...)
	} else {
		log.Printf(format, args...)
	}
}

func (s *Server) catalog() Catalog {
	if s.Catalog != nil {
		return s.Catalog
	}
	return Packs(s.Personas)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<20)).Decode(v); err != nil {
		fail(w, http.StatusBadRequest, errors.New("invalid JSON body: "+err.Error()))
		return false
	}
	return true
}

func (s *Server) record(ctx context.Context, personaID, op string, u llm.Usage, start time.Time) {
	if s.Usage == nil {
		return
	}
	p := principalOf(ctx)
	if p == nil {
		return
	}
	err := s.Usage.RecordUsage(context.WithoutCancel(ctx), cloud.Usage{KeyID: p.KeyID, PersonaID: personaID, Op: op,
		PromptTokens: u.PromptTokens, CompletionTokens: u.CompletionTokens, Calls: u.Calls, Duration: time.Since(start)})
	if err != nil {
		s.logf("usage: %v", err)
	}
}

func (s *Server) addMessages(w http.ResponseWriter, r *http.Request) {
	if s.Memory == nil {
		fail(w, http.StatusServiceUnavailable, errors.New("memory is not configured"))
		return
	}
	var req struct {
		Messages []memory.Message `json:"messages"`
	}
	if !decode(w, r, &req) {
		return
	}
	if err := s.Memory.Add(r.Context(), r.PathValue("scope"), req.Messages); err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]int{"stored": len(req.Messages)})
}

type itemJSON struct {
	ID        int64    `json:"id"`
	Kind      string   `json:"kind"`
	Title     string   `json:"title,omitempty"`
	Text      string   `json:"text"`
	ValidAt   string   `json:"valid_at,omitempty"`
	InvalidAt string   `json:"invalid_at,omitempty"`
	Score     float64  `json:"score"`
	Channels  []string `json:"channels,omitempty"`
}

func toJSON(items []recall.Item) []itemJSON {
	out := make([]itemJSON, len(items))
	for i, it := range items {
		n := it.Node
		out[i] = itemJSON{ID: n.ID, Kind: string(n.Kind), Title: n.Title, Text: n.Body, Score: it.Score, Channels: it.Channels}
		if !n.ValidAt.IsZero() {
			out[i].ValidAt = n.ValidAt.Format(time.RFC3339)
		}
		if !n.InvalidAt.IsZero() {
			out[i].InvalidAt = n.InvalidAt.Format(time.RFC3339)
		}
	}
	return out
}

func (s *Server) searchMemory(w http.ResponseWriter, r *http.Request) {
	if s.Memory == nil {
		fail(w, http.StatusServiceUnavailable, errors.New("memory is not configured"))
		return
	}
	var req struct {
		Query          string `json:"query"`
		Facts          int    `json:"facts"`
		Episodes       int    `json:"episodes"`
		IncludeHistory bool   `json:"include_history"`
		Rerank         bool   `json:"rerank"`
		MaxTokens      int    `json:"max_tokens"`
	}
	if !decode(w, r, &req) {
		return
	}
	res, err := s.Memory.Search(r.Context(), r.PathValue("scope"), req.Query, memory.SearchOptions{
		Facts: req.Facts, Episodes: req.Episodes, IncludeHistory: req.IncludeHistory, Rerank: req.Rerank,
	})
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, 200, map[string]any{
		"facts":    toJSON(res.Facts),
		"episodes": toJSON(res.Episodes),
		"context":  res.Context(req.MaxTokens),
	})
}

// canAny reports whether the caller may use personaID in any way.
func canAny(p *cloud.Principal, personaID string) bool {
	for _, sc := range cloud.AllScopes {
		if p.Can(sc, personaID) {
			return true
		}
	}
	return false
}

func (s *Server) listPersonas(w http.ResponseWriter, r *http.Request) {
	specs, err := s.catalog().List(r.Context())
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	p := principalOf(r.Context())
	allowed := specs[:0]
	for _, sp := range specs {
		if canAny(p, sp.ID) {
			allowed = append(allowed, sp)
		}
	}
	sort.Slice(allowed, func(i, j int) bool { return allowed[i].ID < allowed[j].ID })
	writeJSON(w, 200, map[string]any{"personas": allowed})
}

// persona resolves the {name} path value for a caller holding scope. It
// writes the error response and returns nil when it cannot.
func (s *Server) persona(w http.ResponseWriter, r *http.Request, scope string) *persona.Persona {
	id := r.PathValue("name")
	pr := principalOf(r.Context())
	if !canAny(pr, id) { // do not reveal personas the key cannot see
		fail(w, http.StatusNotFound, errors.New("unknown persona: "+id))
		return nil
	}
	if scope != "" && !pr.Can(scope, id) {
		fail(w, http.StatusForbidden, fmt.Errorf("this API key lacks the %q scope for %s", scope, id))
		return nil
	}
	p, err := s.catalog().Persona(r.Context(), id)
	if errors.Is(err, cloud.ErrNotFound) {
		fail(w, http.StatusNotFound, errors.New("unknown persona: "+id))
		return nil
	}
	if err != nil {
		s.logf("load persona %s: %v", id, err)
		fail(w, http.StatusServiceUnavailable, fmt.Errorf("persona %s is unavailable, retry", id))
		return nil
	}
	return p
}

func (s *Server) personaInfo(w http.ResponseWriter, r *http.Request) {
	p := s.persona(w, r, "")
	if p == nil {
		return
	}
	m, err := p.Manifest(r.Context())
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, 200, m)
}

// KnowledgeRequest is the body of POST /v1/personas/{id}/knowledge.
type KnowledgeRequest struct {
	Query string `json:"query"`
	// Units and Sources override the pack's retrieval budget. An agent that
	// calls this as a knowledge tool, once per sub-question, can ask for a
	// few units per call.
	Units   int `json:"units,omitempty"`
	Sources int `json:"sources,omitempty"`
}

func (s *Server) personaKnowledge(w http.ResponseWriter, r *http.Request) {
	p := s.persona(w, r, cloud.ScopeKnowledge)
	if p == nil {
		return
	}
	var req KnowledgeRequest
	if !decode(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Query) == "" {
		fail(w, http.StatusBadRequest, errors.New("query is empty"))
		return
	}
	if req.Units > 64 || req.Sources > 16 {
		fail(w, http.StatusBadRequest, errors.New("at most 64 units and 16 sources"))
		return
	}
	start := time.Now()
	k, err := p.Retrieve(r.Context(), req.Query, persona.RetrieveOptions{Units: req.Units, Sources: req.Sources})
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	s.record(r.Context(), p.Spec.ID, "knowledge", llm.Usage{}, start)
	writeJSON(w, 200, map[string]any{
		"persona": p.Spec.ID, "version": p.Spec.Version,
		"system": p.SystemPrompt(k), "context": k.Text(),
		"units": toJSON(k.Units), "sources": toJSON(k.Sources),
	})
}

// ChatRequest is the body of POST /v1/personas/{id}/chat and .../jobs.
type ChatRequest struct {
	Messages []llm.Message `json:"messages"`
	// Context is extra material for the system prompt, such as what the
	// caller's own app knows about the user.
	Context   string `json:"context,omitempty"`
	MaxTokens int    `json:"max_tokens,omitempty"`
	// Stream answers as server-sent events (chat only).
	Stream bool `json:"stream,omitempty"`
	// MemoryScope adds what this server's memory holds about the user and,
	// with Remember, stores the exchange afterwards (self-hosted servers).
	MemoryScope string `json:"memory_scope,omitempty"`
	Remember    bool   `json:"remember,omitempty"`
}

// ChatResult is a finished answer: the body of a chat response, the final
// server-sent event, and an async job's result.
type ChatResult struct {
	Persona string     `json:"persona"`
	Version string     `json:"version"`
	Reply   string     `json:"reply"`
	Units   []itemJSON `json:"units"`
	Usage   llm.Usage  `json:"usage"`
}

func (req *ChatRequest) validate() error {
	if len(req.Messages) == 0 {
		return errors.New("messages is empty")
	}
	if req.Messages[len(req.Messages)-1].Role != "user" {
		return errors.New("the last message must be from the user")
	}
	return nil
}

func (s *Server) chatOptions(ctx context.Context, req *ChatRequest) (persona.ChatOptions, error) {
	opt := persona.ChatOptions{Context: req.Context, MaxTokens: req.MaxTokens}
	if req.MemoryScope != "" && s.Memory != nil {
		res, err := s.Memory.Search(ctx, req.MemoryScope, req.Messages[len(req.Messages)-1].Content, memory.SearchOptions{})
		if err != nil {
			return opt, err
		}
		if block := res.Context(1500); block != "" {
			opt.Context = strings.TrimSpace(opt.Context + "\n\n" + block)
		}
	}
	return opt, nil
}

func (s *Server) remember(ctx context.Context, req *ChatRequest, reply string) error {
	if !req.Remember || req.MemoryScope == "" || s.Memory == nil {
		return nil
	}
	last := req.Messages[len(req.Messages)-1]
	return s.Memory.Add(ctx, req.MemoryScope, []memory.Message{{Role: last.Role, Content: last.Content}, {Role: "assistant", Content: reply}})
}

func (s *Server) personaChat(w http.ResponseWriter, r *http.Request) {
	p := s.persona(w, r, cloud.ScopeChat)
	if p == nil {
		return
	}
	if s.Chat == nil {
		fail(w, http.StatusServiceUnavailable, errors.New("no chat model is configured"))
		return
	}
	var req ChatRequest
	if !decode(w, r, &req) {
		return
	}
	if err := req.validate(); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	opt, err := s.chatOptions(r.Context(), &req)
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	if req.Stream || strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
		s.streamChat(w, r, p, &req, opt)
		return
	}
	start := time.Now()
	reply, err := p.Chat(r.Context(), s.Chat, req.Messages, opt)
	if err != nil {
		fail(w, http.StatusBadGateway, err)
		return
	}
	s.record(r.Context(), p.Spec.ID, "chat", reply.Usage, start)
	if err := s.remember(r.Context(), &req, reply.Content); err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, 200, result(p, reply))
}

func result(p *persona.Persona, reply *persona.Reply) ChatResult {
	return ChatResult{Persona: p.Spec.ID, Version: p.Spec.Version, Reply: reply.Content, Units: toJSON(reply.Knowledge.Units), Usage: reply.Usage}
}

// streamChat answers as server-sent events:
//
//	event: delta   data: {"text": "..."}       repeated, in order
//	event: done    data: ChatResult            once, at the end
//	event: error   data: {"error": "..."}      instead of done, on failure
func (s *Server) streamChat(w http.ResponseWriter, r *http.Request, p *persona.Persona, req *ChatRequest, opt persona.ChatOptions) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		fail(w, http.StatusNotImplemented, errors.New("streaming is not supported on this deployment; use the async jobs endpoint"))
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no") // nginx and Caddy-style proxies: do not buffer
	w.WriteHeader(http.StatusOK)
	send := func(event string, v any) error {
		data, _ := json.Marshal(v)
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data); err != nil {
			return err
		}
		flusher.Flush()
		return nil
	}
	start := time.Now()
	reply, err := p.ChatStream(r.Context(), s.Chat, req.Messages, opt, func(delta string) error {
		return send("delta", map[string]string{"text": delta})
	})
	if err == nil {
		err = s.remember(r.Context(), req, reply.Content)
	}
	if err != nil {
		send("error", map[string]string{"error": err.Error()})
		return
	}
	s.record(r.Context(), p.Spec.ID, "stream", reply.Usage, start)
	send("done", result(p, reply))
}

func (s *Server) downloadPack(w http.ResponseWriter, r *http.Request) {
	p := s.persona(w, r, cloud.ScopeDownload)
	if p == nil {
		return
	}
	f, err := os.Open(p.Path())
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	sum, err := s.packSHA(p.Path(), f)
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	if r.Header.Get("Range") == "" || strings.HasPrefix(r.Header.Get("Range"), "bytes=0-") {
		s.record(r.Context(), p.Spec.ID, "download", llm.Usage{}, time.Now())
	}
	h := w.Header()
	h.Set("Content-Type", "application/vnd.openpersona.pack")
	h.Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", p.Spec.ID+"@"+p.Spec.Version+persona.PackExt))
	h.Set("ETag", `"`+sum+`"`)
	h.Set("X-Pack-SHA256", sum)
	h.Set("X-Persona-Id", p.Spec.ID)
	h.Set("X-Persona-Version", p.Spec.Version)
	http.ServeContent(w, r, "", st.ModTime(), f)
}

// packSHA hashes a pack once per path; packs never change in place.
func (s *Server) packSHA(path string, f io.Reader) (string, error) {
	s.shaMu.Lock()
	defer s.shaMu.Unlock()
	if sum, ok := s.shas[path]; ok {
		return sum, nil
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	if s.shas == nil {
		s.shas = make(map[string]string)
	}
	s.shas[path] = hex.EncodeToString(h.Sum(nil))
	return s.shas[path], nil
}
