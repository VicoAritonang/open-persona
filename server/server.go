// Package server exposes the memory and persona engines over HTTP and MCP,
// so any backend, in any language, and any MCP-capable agent can use them.
// Package rpc adds the same operations as gRPC services.
//
// The API contract is versioned and lives in the api directory of the
// repository: api/openapi.yaml for HTTP and api/proto for gRPC.
//
// The same handler runs as a self-hosted server (openpersona serve) and
// inside other deployments, such as a hosted platform on AWS Lambda. What
// differs is wiring: where packs come from (Personas or Catalog), who may
// call (Auth), and where async jobs run (Jobs and Dispatch).
package server

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"go.vicoworks.com/open-persona/api"
	"go.vicoworks.com/open-persona/llm"
	"go.vicoworks.com/open-persona/memory"
	"go.vicoworks.com/open-persona/persona"
)

// Catalog supplies personas on demand, for example from a registry
// database; Packs implements it on packs already open.
type Catalog interface {
	// Persona returns the current version of id, or an error wrapping
	// ErrNotFound.
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
	return nil, fmt.Errorf("persona %q: %w", id, ErrNotFound)
}

// List implements Catalog.
func (m Packs) List(context.Context) ([]persona.Spec, error) {
	specs := make([]persona.Spec, 0, len(m))
	for _, p := range m {
		specs = append(specs, p.Spec)
	}
	return specs, nil
}

// Server wires the engines to the API. Any field may be nil; the
// operations that need it then fail with CodeUnavailable.
type Server struct {
	Memory *memory.Engine
	// Personas serves a fixed set of open packs; Catalog serves them on
	// demand. Catalog wins when both are set.
	Personas  map[string]*persona.Persona
	Catalog   Catalog
	ChatModel llm.Client // model that speaks for personas; streams if it implements llm.Streamer

	// Auth checks the bearer token of every request except /healthz and
	// /openapi.yaml. When nil, APIKey (if set) is the single accepted key;
	// with neither, the server is open (only for localhost or behind another
	// auth layer).
	Auth   Authenticator
	APIKey string

	// Jobs stores async jobs (POST /v1/personas/{id}/jobs). Dispatch starts
	// a stored job outside the request, for example by invoking a serverless
	// function asynchronously. When Dispatch is nil, jobs run in a goroutine
	// of this process, so the process must outlive them.
	Jobs     JobStore
	Dispatch func(ctx context.Context, jobID string) error
	// JobTimeout bounds one job run (default 10 minutes).
	JobTimeout time.Duration

	// AllowOrigins lists the browser origins allowed to call the API (CORS),
	// such as "https://app.example.com"; "*" allows any. Empty: no CORS
	// headers.
	AllowOrigins []string

	// Mount adds handlers next to the HTTP API, keyed by ServeMux pattern,
	// such as the gRPC services of package rpc. They get CORS but not the
	// HTTP API's authentication: they authenticate for themselves.
	Mount map[string]http.Handler

	Usage   UsageRecorder // optional: one record per billable operation
	Logf    func(format string, args ...any)
	Version string // reported to MCP clients

	shaMu sync.Mutex
	shas  map[string]string // pack path -> sha256
}

// Handler returns the HTTP API (see api/openapi.yaml), MCP, and whatever
// is in Mount.
//
//	GET  /healthz
//	GET  /openapi.yaml                        the HTTP API contract
//	POST /v1/memory/{scope}/messages          store messages
//	POST /v1/memory/{scope}/search            facts, messages and a prompt-ready context block
//	GET  /v1/personas                         personas this key may use
//	GET  /v1/personas/{id}                    one persona's manifest
//	POST /v1/personas/{id}/knowledge          retrieved knowledge and system prompt; no LLM call
//	POST /v1/personas/{id}/prompt             same as knowledge (deprecated name)
//	POST /v1/personas/{id}/chat               answer as the persona; "stream": true answers as server-sent events
//	POST /v1/personas/{id}/jobs               answer asynchronously; returns a job to poll
//	GET  /v1/jobs/{id}                        an async job and, when done, its result
//	GET  /v1/personas/{id}/pack               the pack file, for running it locally (supports Range)
//	POST /mcp                                 Model Context Protocol (streamable HTTP, JSON responses)
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	root := http.NewServeMux()
	for _, rt := range s.routes() {
		if rt.public {
			root.HandleFunc(rt.pattern, rt.handler)
		} else {
			mux.HandleFunc(rt.pattern, rt.handler)
		}
	}
	root.Handle("/", s.authenticate(mux))
	for pattern, h := range s.Mount {
		root.Handle(pattern, h)
	}
	return s.cors(root)
}

type route struct {
	pattern string // "METHOD /path", as in api/openapi.yaml
	handler http.HandlerFunc
	public  bool // no API key needed
}

// routes is the HTTP API. A test checks it against api/openapi.yaml.
func (s *Server) routes() []route {
	return []route{
		{"GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
		}, true},
		{"GET /openapi.yaml", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/yaml")
			w.Write(api.OpenAPI)
		}, true},
		{"POST /v1/memory/{scope}/messages", s.addMessages, false},
		{"POST /v1/memory/{scope}/search", s.searchMemory, false},
		{"GET /v1/personas", s.listPersonas, false},
		{"GET /v1/personas/{name}", s.personaInfo, false},
		{"POST /v1/personas/{name}/knowledge", s.personaKnowledge, false},
		{"POST /v1/personas/{name}/prompt", s.personaKnowledge, false},
		{"POST /v1/personas/{name}/chat", s.personaChat, false},
		{"POST /v1/personas/{name}/jobs", s.submitJob, false},
		{"GET /v1/jobs/{id}", s.getJob, false},
		{"GET /v1/personas/{name}/pack", s.downloadPack, false},
		{"POST /mcp", s.mcp, false},
		{"GET /mcp", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Allow", "POST")
			fail(w, &Error{Code: CodeUnimplemented, Message: "this MCP server answers POST with JSON and offers no SSE stream"})
		}, false},
	}
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
			h.Set("Access-Control-Expose-Headers", "Location, Retry-After, X-Pack-SHA256, X-Persona-Id, X-Persona-Version, Content-Range, "+
				"Grpc-Status, Grpc-Message, Grpc-Status-Details-Bin")
			if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
				h.Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
				h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Accept, Range, X-API-Key, Mcp-Protocol-Version, Mcp-Session-Id, "+
					"Connect-Protocol-Version, Connect-Timeout-Ms, Grpc-Timeout, X-Grpc-Web, X-User-Agent")
				h.Set("Access-Control-Max-Age", "600")
				w.WriteHeader(http.StatusNoContent)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// TokenFrom extracts the API key from request headers:
// "Authorization: Bearer <key>", or "X-API-Key: <key>".
func TokenFrom(h http.Header) string {
	token := strings.TrimSpace(strings.TrimPrefix(h.Get("Authorization"), "Bearer "))
	if token == "" {
		token = strings.TrimSpace(h.Get("X-API-Key"))
	}
	return token
}

// Authenticate resolves an API key to its principal with the configured
// authenticator. It fails with CodeUnauthenticated, or CodeUnavailable when
// the authenticator itself is down.
func (s *Server) Authenticate(ctx context.Context, token string) (*Principal, error) {
	var auth Authenticator = OpenAccess{}
	switch {
	case s.Auth != nil:
		auth = s.Auth
	case s.APIKey != "":
		auth = StaticKey(s.APIKey)
	}
	p, err := auth.Authenticate(ctx, token)
	switch {
	case err == nil:
		return p, nil
	case errors.Is(err, ErrUnauthorized):
		return nil, wrapError(CodeUnauthenticated, err)
	}
	s.logf("auth: %v", err)
	return nil, &Error{Code: CodeUnavailable, Message: "authentication is unavailable, retry", Err: err}
}

func (s *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, err := s.Authenticate(r.Context(), TokenFrom(r.Header))
		if err != nil {
			fail(w, err)
			return
		}
		next.ServeHTTP(w, r.WithContext(WithPrincipal(r.Context(), p)))
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

func (s *Server) record(ctx context.Context, personaID, op string, u llm.Usage, start time.Time) {
	if s.Usage == nil {
		return
	}
	p := PrincipalFrom(ctx)
	if p == nil {
		return
	}
	err := s.Usage.RecordUsage(context.WithoutCancel(ctx), Usage{KeyID: p.KeyID, PersonaID: personaID, Op: op,
		PromptTokens: u.PromptTokens, CompletionTokens: u.CompletionTokens, Calls: u.Calls, Duration: time.Since(start)})
	if err != nil {
		s.logf("usage: %v", err)
	}
}
