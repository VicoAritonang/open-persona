package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"go.vicoworks.com/open-persona/llm"
	"go.vicoworks.com/open-persona/memory"
	"go.vicoworks.com/open-persona/persona"
)

// The HTTP API: thin handlers over the operations in core.go. The
// contract is api/openapi.yaml.

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// ErrorBody is the body of every error response of the HTTP API.
type ErrorBody struct {
	Error string `json:"error"`
	Code  Code   `json:"code"`
}

func fail(w http.ResponseWriter, err error) {
	e := AsError(err)
	if e.Code == CodeUnavailable {
		w.Header().Set("Retry-After", "2")
	}
	writeJSON(w, e.Code.HTTPStatus(), ErrorBody{Error: e.Message, Code: e.Code})
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<20)).Decode(v); err != nil {
		fail(w, newError(CodeInvalidArgument, "invalid JSON body: %v", err))
		return false
	}
	return true
}

func (s *Server) addMessages(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Messages []memory.Message `json:"messages"`
	}
	if !decode(w, r, &req) {
		return
	}
	if err := s.AddMessages(r.Context(), r.PathValue("scope"), req.Messages); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]int{"stored": len(req.Messages)})
}

func (s *Server) searchMemory(w http.ResponseWriter, r *http.Request) {
	var req MemorySearchRequest
	if !decode(w, r, &req) {
		return
	}
	res, err := s.SearchMemory(r.Context(), r.PathValue("scope"), req)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) listPersonas(w http.ResponseWriter, r *http.Request) {
	specs, err := s.ListPersonas(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"personas": specs})
}

func (s *Server) personaInfo(w http.ResponseWriter, r *http.Request) {
	m, err := s.Manifest(r.Context(), r.PathValue("name"))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, m)
}

func (s *Server) personaKnowledge(w http.ResponseWriter, r *http.Request) {
	var req KnowledgeRequest
	if !decode(w, r, &req) {
		return
	}
	k, err := s.Knowledge(r.Context(), r.PathValue("name"), req)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, k)
}

func (s *Server) personaChat(w http.ResponseWriter, r *http.Request) {
	var req ChatRequest
	if !decode(w, r, &req) {
		return
	}
	if req.Stream || strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
		s.streamChat(w, r, req)
		return
	}
	res, err := s.Chat(r.Context(), r.PathValue("name"), req, nil)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// streamChat answers as server-sent events:
//
//	event: delta   data: {"text": "..."}               repeated, in order
//	event: done    data: ChatResult                    once, at the end
//	event: error   data: {"error": "...", "code": ...} instead of done, on failure
//
// Errors found before the first byte (bad request, unknown persona) are
// ordinary JSON error responses.
func (s *Server) streamChat(w http.ResponseWriter, r *http.Request, req ChatRequest) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		fail(w, newError(CodeUnimplemented, "streaming is not supported on this deployment; use the async jobs endpoint"))
		return
	}
	started := false
	send := func(event string, v any) error {
		if !started {
			h := w.Header()
			h.Set("Content-Type", "text/event-stream")
			h.Set("Cache-Control", "no-cache")
			h.Set("X-Accel-Buffering", "no") // nginx and Caddy-style proxies: do not buffer
			w.WriteHeader(http.StatusOK)
			started = true
		}
		data, _ := json.Marshal(v)
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data); err != nil {
			return err
		}
		flusher.Flush()
		return nil
	}
	res, err := s.Chat(r.Context(), r.PathValue("name"), req, func(delta string) error {
		return send("delta", map[string]string{"text": delta})
	})
	switch {
	case err != nil && !started:
		fail(w, err)
	case err != nil:
		e := AsError(err)
		send("error", ErrorBody{Error: e.Message, Code: e.Code})
	default:
		send("done", res)
	}
}

func (s *Server) downloadPack(w http.ResponseWriter, r *http.Request) {
	p, err := s.Persona(r.Context(), r.PathValue("name"), ScopeDownload)
	if err != nil {
		fail(w, err)
		return
	}
	f, err := os.Open(p.Path())
	if err != nil {
		fail(w, err)
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		fail(w, err)
		return
	}
	sum, err := s.packSHA(p.Path(), f)
	if err != nil {
		fail(w, err)
		return
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		fail(w, err)
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
