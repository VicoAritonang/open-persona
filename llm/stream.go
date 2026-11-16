package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Streamer is a Client that can also deliver a completion as it is
// generated. onDelta receives each piece of text in order; returning an
// error from it stops the stream. The Response holds the whole text and,
// when the provider reports it, the usage.
type Streamer interface {
	Client
	Stream(ctx context.Context, req Request, onDelta func(string) error) (Response, error)
}

// Stream implements Streamer over the OpenAI-compatible SSE protocol.
func (c *OpenAI) Stream(ctx context.Context, req Request, onDelta func(string) error) (Response, error) {
	body := map[string]any{
		"model": c.Model, "messages": req.Messages, "stream": true,
		"stream_options": map[string]bool{"include_usage": true},
	}
	if req.JSON {
		body["response_format"] = map[string]string{"type": "json_object"}
	}
	if req.Temperature != nil {
		body["temperature"] = *req.Temperature
	}
	if req.MaxTokens > 0 {
		body["max_tokens"] = req.MaxTokens
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return Response{}, err
	}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.BaseURL, "/")+"/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return Response{}, err
	}
	hreq.Header.Set("Content-Type", "application/json")
	hreq.Header.Set("Accept", "text/event-stream")
	hreq.Header.Set("Authorization", "Bearer "+c.APIKey)
	hc := c.HTTP
	if hc == nil {
		hc = streamHTTP
	}
	resp, err := hc.Do(hreq)
	if err != nil {
		return Response{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return Response{}, fmt.Errorf("llm: %s: HTTP %d: %s", c.Model, resp.StatusCode, snippet(data))
	}
	return readSSE(resp.Body, c.Model, onDelta)
}

// streamHTTP has no overall timeout: a long answer may stream for minutes.
// The caller's context bounds it.
var streamHTTP = &http.Client{}

func readSSE(r io.Reader, model string, onDelta func(string) error) (Response, error) {
	out := Response{Model: model, Usage: Usage{Calls: 1}}
	var text strings.Builder
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			break
		}
		var ev struct {
			Model   string `json:"model"`
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
			Usage *struct {
				PromptTokens     int64 `json:"prompt_tokens"`
				CompletionTokens int64 `json:"completion_tokens"`
			} `json:"usage"`
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			continue // keep-alive comments and unknown events
		}
		if ev.Error != nil {
			return out, fmt.Errorf("llm: %s: %s", model, ev.Error.Message)
		}
		if ev.Model != "" {
			out.Model = ev.Model
		}
		if ev.Usage != nil {
			out.Usage.PromptTokens, out.Usage.CompletionTokens = ev.Usage.PromptTokens, ev.Usage.CompletionTokens
		}
		for _, ch := range ev.Choices {
			if ch.Delta.Content == "" {
				continue
			}
			text.WriteString(ch.Delta.Content)
			if onDelta != nil {
				if err := onDelta(ch.Delta.Content); err != nil {
					out.Content = text.String()
					return out, err
				}
			}
		}
	}
	out.Content = text.String()
	if err := sc.Err(); err != nil {
		return out, err
	}
	if out.Content == "" {
		return out, errors.New("llm: " + model + ": empty stream")
	}
	return out, nil
}

// StreamOrComplete streams when c supports it and otherwise completes the
// request and delivers the whole answer as one delta.
func StreamOrComplete(ctx context.Context, c Client, req Request, onDelta func(string) error) (Response, error) {
	if s, ok := c.(Streamer); ok {
		return s.Stream(ctx, req, onDelta)
	}
	resp, err := c.Complete(ctx, req)
	if err == nil && onDelta != nil {
		err = onDelta(resp.Content)
	}
	return resp, err
}

// Stream implements Streamer, streaming when the wrapped client can.
func (m *Meter) Stream(ctx context.Context, req Request, onDelta func(string) error) (Response, error) {
	resp, err := StreamOrComplete(ctx, m.Client, req, onDelta)
	if err == nil {
		m.prompt.Add(resp.Usage.PromptTokens)
		m.completion.Add(resp.Usage.CompletionTokens)
		m.calls.Add(1)
	}
	return resp, err
}
