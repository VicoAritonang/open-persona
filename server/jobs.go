package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"go.vicoworks.com/open-persona/persona"
)

// SubmitJob stores an answer to run asynchronously and starts it. Poll it
// with Job. A job answers once: Stream, MemoryScope and Remember are
// ignored. Scope: chat.
func (s *Server) SubmitJob(ctx context.Context, id string, req ChatRequest) (*Job, error) {
	if s.Jobs == nil {
		return nil, newError(CodeUnavailable, "async jobs are not configured")
	}
	if err := req.validate(); err != nil {
		return nil, err
	}
	p, err := s.Persona(ctx, id, ScopeChat)
	if err != nil {
		return nil, err
	}
	if s.ChatModel == nil {
		return nil, newError(CodeUnavailable, "no chat model is configured")
	}
	req.Stream, req.MemoryScope, req.Remember = false, "", false
	body, _ := json.Marshal(req)
	j := &Job{KeyID: PrincipalFrom(ctx).KeyID, PersonaID: p.Spec.ID, Kind: "chat", Request: body}
	if err := s.Jobs.CreateJob(ctx, j); err != nil {
		return nil, wrapError(CodeInternal, err)
	}
	if err := s.dispatch(ctx, j.ID); err != nil {
		s.Jobs.FinishJob(context.WithoutCancel(ctx), j.ID, nil, "dispatch failed: "+err.Error())
		return nil, &Error{Code: CodeUnavailable, Message: "could not start job: " + err.Error(), Err: err}
	}
	j.Request = nil
	return j, nil
}

func (s *Server) dispatch(ctx context.Context, id string) error {
	if s.Dispatch != nil {
		return s.Dispatch(ctx, id)
	}
	go func() {
		if err := s.RunJob(context.WithoutCancel(ctx), id); err != nil {
			s.logf("job %s: %v", id, err)
		}
	}()
	return nil
}

// Job returns an async job of the caller. Another key's job does not exist
// for this caller.
func (s *Server) Job(ctx context.Context, id string) (*Job, error) {
	if s.Jobs == nil {
		return nil, newError(CodeUnavailable, "async jobs are not configured")
	}
	j, err := s.Jobs.GetJob(ctx, id)
	if err == nil && (PrincipalFrom(ctx) == nil || j.KeyID != PrincipalFrom(ctx).KeyID) {
		err = ErrNotFound
	}
	if errors.Is(err, ErrNotFound) {
		return nil, newError(CodeNotFound, "unknown job")
	}
	if err != nil {
		return nil, wrapError(CodeInternal, err)
	}
	j.Request = nil
	return j, nil
}

func (s *Server) submitJob(w http.ResponseWriter, r *http.Request) {
	var req ChatRequest
	if !decode(w, r, &req) {
		return
	}
	j, err := s.SubmitJob(r.Context(), r.PathValue("name"), req)
	if err != nil {
		fail(w, err)
		return
	}
	w.Header().Set("Location", "/v1/jobs/"+j.ID)
	writeJSON(w, http.StatusAccepted, j)
}

func (s *Server) getJob(w http.ResponseWriter, r *http.Request) {
	j, err := s.Job(r.Context(), r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	if !j.Done() {
		w.Header().Set("Retry-After", "2")
	}
	writeJSON(w, http.StatusOK, j)
}

// RunJob runs a stored job to completion and records its outcome. Workers
// call it: the goroutine started by a server without Dispatch, or the
// invocation that Dispatch triggered. A job already finished, or running
// elsewhere, is left alone, so a redelivered event is harmless.
func (s *Server) RunJob(ctx context.Context, id string) error {
	if s.Jobs == nil {
		return errors.New("server: no job store")
	}
	timeout := s.JobTimeout
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	j, err := s.Jobs.ClaimJob(ctx, id, timeout+time.Minute)
	if err != nil || j == nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	res, err := s.runChat(ctx, j)
	if err != nil {
		return s.Jobs.FinishJob(context.WithoutCancel(ctx), id, nil, err.Error())
	}
	return s.Jobs.FinishJob(context.WithoutCancel(ctx), id, res, "")
}

func (s *Server) runChat(ctx context.Context, j *Job) (json.RawMessage, error) {
	var req ChatRequest
	if err := json.Unmarshal(j.Request, &req); err != nil {
		return nil, fmt.Errorf("bad job request: %w", err)
	}
	if err := req.validate(); err != nil {
		return nil, err
	}
	if s.ChatModel == nil {
		return nil, errors.New("no chat model is configured")
	}
	p, err := s.catalog().Persona(ctx, j.PersonaID)
	if err != nil {
		return nil, err
	}
	ctx = WithPrincipal(ctx, &Principal{KeyID: j.KeyID})
	start := time.Now()
	reply, err := p.Chat(ctx, s.ChatModel, req.Messages, persona.ChatOptions{Context: req.Context, MaxTokens: req.MaxTokens})
	if err != nil {
		return nil, err
	}
	s.record(ctx, p.Spec.ID, "job", reply.Usage, start)
	return json.Marshal(result(p, reply))
}
