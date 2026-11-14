// Package server exposes the memory and persona engines over HTTP, so any
// backend, in any language, can use them.
package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"time"

	"github.com/openpersona/openpersona/llm"
	"github.com/openpersona/openpersona/memory"
	"github.com/openpersona/openpersona/persona"
	"github.com/openpersona/openpersona/recall"
)

// Server wires the engines to HTTP handlers. Any field may be nil; the
// endpoints that need it then answer 503.
type Server struct {
	Memory   *memory.Engine
	Personas map[string]*persona.Persona
	Chat     llm.Client // model that speaks for personas
}

// Handler returns the HTTP API.
//
//	GET  /healthz
//	POST /v1/memory/{scope}/messages      store messages
//	POST /v1/memory/{scope}/search        retrieve facts, messages and a prompt-ready context block
//	GET  /v1/personas                     list loaded personas
//	POST /v1/personas/{name}/prompt       system prompt for a query (bring your own model)
//	POST /v1/personas/{name}/chat         answer as the persona, optionally with user memory
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, 200, map[string]string{"status": "ok"}) })
	mux.HandleFunc("POST /v1/memory/{scope}/messages", s.addMessages)
	mux.HandleFunc("POST /v1/memory/{scope}/search", s.searchMemory)
	mux.HandleFunc("GET /v1/personas", s.listPersonas)
	mux.HandleFunc("POST /v1/personas/{name}/prompt", s.personaPrompt)
	mux.HandleFunc("POST /v1/personas/{name}/chat", s.personaChat)
	return mux
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

func (s *Server) listPersonas(w http.ResponseWriter, _ *http.Request) {
	specs := make([]persona.Spec, 0, len(s.Personas))
	for _, p := range s.Personas {
		specs = append(specs, p.Spec)
	}
	sort.Slice(specs, func(i, j int) bool { return specs[i].Name < specs[j].Name })
	writeJSON(w, 200, map[string]any{"personas": specs})
}

func (s *Server) persona(w http.ResponseWriter, r *http.Request) *persona.Persona {
	p := s.Personas[r.PathValue("name")]
	if p == nil {
		fail(w, http.StatusNotFound, errors.New("unknown persona: "+r.PathValue("name")))
	}
	return p
}

func (s *Server) personaPrompt(w http.ResponseWriter, r *http.Request) {
	p := s.persona(w, r)
	if p == nil {
		return
	}
	var req struct {
		Query string `json:"query"`
	}
	if !decode(w, r, &req) {
		return
	}
	k, err := p.Retrieve(r.Context(), req.Query, persona.RetrieveOptions{})
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, 200, map[string]any{"system": p.SystemPrompt(k), "units": toJSON(k.Units)})
}

func (s *Server) personaChat(w http.ResponseWriter, r *http.Request) {
	p := s.persona(w, r)
	if p == nil {
		return
	}
	if s.Chat == nil {
		fail(w, http.StatusServiceUnavailable, errors.New("no chat model is configured"))
		return
	}
	var req struct {
		Messages []llm.Message `json:"messages"`
		// MemoryScope adds what is remembered about this user to the prompt
		// and, if Remember is set, stores this exchange afterwards.
		MemoryScope string `json:"memory_scope"`
		Remember    bool   `json:"remember"`
	}
	if !decode(w, r, &req) {
		return
	}
	if len(req.Messages) == 0 {
		fail(w, http.StatusBadRequest, errors.New("messages is empty"))
		return
	}
	last := req.Messages[len(req.Messages)-1]
	opt := persona.ChatOptions{}
	if req.MemoryScope != "" && s.Memory != nil {
		res, err := s.Memory.Search(r.Context(), req.MemoryScope, last.Content, memory.SearchOptions{})
		if err != nil {
			fail(w, http.StatusInternalServerError, err)
			return
		}
		opt.Context = res.Context(1500)
	}
	reply, err := p.Chat(r.Context(), s.Chat, req.Messages, opt)
	if err != nil {
		fail(w, http.StatusBadGateway, err)
		return
	}
	if req.Remember && req.MemoryScope != "" && s.Memory != nil {
		err := s.Memory.Add(r.Context(), req.MemoryScope, []memory.Message{
			{Role: last.Role, Content: last.Content},
			{Role: "assistant", Content: reply.Content},
		})
		if err != nil {
			fail(w, http.StatusInternalServerError, err)
			return
		}
	}
	writeJSON(w, 200, map[string]any{"reply": reply.Content, "units": toJSON(reply.Knowledge.Units), "usage": reply.Usage})
}
