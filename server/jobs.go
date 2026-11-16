package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"go.vicoworks.com/openpersona/cloud"
	"go.vicoworks.com/openpersona/persona"
)

func (s *Server) submitJob(w http.ResponseWriter, r *http.Request) {
	if s.Jobs == nil {
		fail(w, http.StatusServiceUnavailable, errors.New("async jobs are not configured"))
		return
	}
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
	req.Stream, req.MemoryScope, req.Remember = false, "", false // a job answers once, without server memory
	body, _ := json.Marshal(req)
	j := &cloud.Job{KeyID: principalOf(r.Context()).KeyID, PersonaID: p.Spec.ID, Kind: "chat", Request: body}
	if err := s.Jobs.CreateJob(r.Context(), j); err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	if err := s.dispatch(r.Context(), j.ID); err != nil {
		s.Jobs.FinishJob(context.WithoutCancel(r.Context()), j.ID, nil, "dispatch failed: "+err.Error())
		fail(w, http.StatusServiceUnavailable, fmt.Errorf("could not start job: %w", err))
		return
	}
	j.Request = nil
	w.Header().Set("Location", "/v1/jobs/"+j.ID)
	writeJSON(w, http.StatusAccepted, j)
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

func (s *Server) getJob(w http.ResponseWriter, r *http.Request) {
	if s.Jobs == nil {
		fail(w, http.StatusServiceUnavailable, errors.New("async jobs are not configured"))
		return
	}
	j, err := s.Jobs.GetJob(r.Context(), r.PathValue("id"))
	if err == nil && j.KeyID != principalOf(r.Context()).KeyID {
		err = cloud.ErrNotFound // another key's job does not exist for this caller
	}
	if errors.Is(err, cloud.ErrNotFound) {
		fail(w, http.StatusNotFound, errors.New("unknown job"))
		return
	}
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	j.Request = nil
	if !j.Done() {
		w.Header().Set("Retry-After", "2")
	}
	writeJSON(w, 200, j)
}

// RunJob runs a stored job to completion and records its outcome. Workers
// call it: the goroutine started by a server without Dispatch, or the
// Lambda invocation that Dispatch triggered. A job already finished, or
// running elsewhere, is left alone, so a redelivered event is harmless.
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

func (s *Server) runChat(ctx context.Context, j *cloud.Job) (json.RawMessage, error) {
	var req ChatRequest
	if err := json.Unmarshal(j.Request, &req); err != nil {
		return nil, fmt.Errorf("bad job request: %w", err)
	}
	if err := req.validate(); err != nil {
		return nil, err
	}
	if s.Chat == nil {
		return nil, errors.New("no chat model is configured")
	}
	p, err := s.catalog().Persona(ctx, j.PersonaID)
	if err != nil {
		return nil, err
	}
	ctx = context.WithValue(ctx, principalKey{}, &cloud.Principal{KeyID: j.KeyID})
	start := time.Now()
	reply, err := p.Chat(ctx, s.Chat, req.Messages, persona.ChatOptions{Context: req.Context, MaxTokens: req.MaxTokens})
	if err != nil {
		return nil, err
	}
	s.record(ctx, p.Spec.ID, "job", reply.Usage, start)
	return json.Marshal(result(p, reply))
}
