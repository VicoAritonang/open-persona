package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"go.vicoworks.com/openpersona/cloud"
	"go.vicoworks.com/openpersona/llm"
	"go.vicoworks.com/openpersona/persona"
)

// MCP: the Model Context Protocol over streamable HTTP, stateless, with
// JSON responses (no SSE stream and no session), which is what lets it run
// on AWS Lambda as well as on a server. Each persona the API key may use
// becomes tools:
//
//	{id}_knowledge   retrieve knowledge for one focused question (scope knowledge; no LLM call)
//	{id}_ask         answer a question as the persona (scope chat; one LLM call)

// mcpVersions are the protocol revisions this server speaks, newest first.
var mcpVersions = []string{"2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"}

type rpcMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

func (s *Server) mcp(w http.ResponseWriter, r *http.Request) {
	var raw json.RawMessage
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(&raw); err != nil {
		writeJSON(w, 200, rpcResponse{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcError{-32700, "parse error"}})
		return
	}
	batch := strings.HasPrefix(strings.TrimSpace(string(raw)), "[")
	var msgs []rpcMessage
	if batch {
		if err := json.Unmarshal(raw, &msgs); err != nil {
			writeJSON(w, 200, rpcResponse{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcError{-32600, "invalid request"}})
			return
		}
	} else {
		var m rpcMessage
		if err := json.Unmarshal(raw, &m); err != nil {
			writeJSON(w, 200, rpcResponse{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcError{-32600, "invalid request"}})
			return
		}
		msgs = []rpcMessage{m}
	}
	var out []rpcResponse
	for _, m := range msgs {
		if len(m.ID) == 0 || string(m.ID) == "null" {
			continue // a notification (or a response from the client): nothing to answer
		}
		res, rerr := s.mcpCall(r.Context(), m)
		out = append(out, rpcResponse{JSONRPC: "2.0", ID: m.ID, Result: res, Error: rerr})
	}
	switch {
	case len(out) == 0:
		w.WriteHeader(http.StatusAccepted)
	case batch:
		writeJSON(w, 200, out)
	default:
		writeJSON(w, 200, out[0])
	}
}

func (s *Server) mcpCall(ctx context.Context, m rpcMessage) (any, *rpcError) {
	switch m.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		json.Unmarshal(m.Params, &p)
		version := mcpVersions[0]
		if slices.Contains(mcpVersions, p.ProtocolVersion) {
			version = p.ProtocolVersion
		}
		v := s.Version
		if v == "" {
			v = "dev"
		}
		return map[string]any{
			"protocolVersion": version,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]any{"name": "openpersona", "version": v},
			"instructions": "Each persona offers {id}_knowledge, which returns the persona's own knowledge for one focused question " +
				"(call it once per sub-question: a visa rule, a train fare, opening hours), and {id}_ask, which answers as the persona.",
		}, nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		tools, err := s.mcpTools(ctx)
		if err != nil {
			return nil, &rpcError{-32603, err.Error()}
		}
		return map[string]any{"tools": tools}, nil
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(m.Params, &p); err != nil {
			return nil, &rpcError{-32602, "invalid params"}
		}
		return s.mcpTool(ctx, p.Name, p.Arguments)
	}
	return nil, &rpcError{-32601, "method not found: " + m.Method}
}

type mcpToolDef struct {
	Name        string         `json:"name"`
	Title       string         `json:"title,omitempty"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

func (s *Server) mcpTools(ctx context.Context) ([]mcpToolDef, error) {
	specs, err := s.catalog().List(ctx)
	if err != nil {
		return nil, err
	}
	pr := principalOf(ctx)
	slices.SortFunc(specs, func(a, b persona.Spec) int { return strings.Compare(a.ID, b.ID) })
	var tools []mcpToolDef
	for _, sp := range specs {
		if pr.Can(cloud.ScopeKnowledge, sp.ID) {
			tools = append(tools, mcpToolDef{
				Name: sp.ID + "_knowledge", Title: sp.Name + ": knowledge",
				Description: sp.KnowledgeToolDescription(),
				InputSchema: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"query": map[string]any{"type": "string", "description": "One focused question, in the user's language"},
						"units": map[string]any{"type": "integer", "description": "How many knowledge units to return (default: the persona's own budget; at most 32)"},
					},
					"required": []string{"query"},
				},
			})
		}
		if pr.Can(cloud.ScopeChat, sp.ID) && s.Chat != nil {
			tools = append(tools, mcpToolDef{
				Name: sp.ID + "_ask", Title: sp.Name + ": ask",
				Description: fmt.Sprintf("Ask %s (%s) and get a finished answer grounded in its knowledge. Slower than %s_knowledge (one model call).", sp.Name, sp.Role, sp.ID),
				InputSchema: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"question": map[string]any{"type": "string"},
						"context":  map[string]any{"type": "string", "description": "Optional facts about the user or task to take into account"},
					},
					"required": []string{"question"},
				},
			})
		}
	}
	return tools, nil
}

func toolText(text string, isErr bool) map[string]any {
	return map[string]any{"content": []map[string]any{{"type": "text", "text": text}}, "isError": isErr}
}

func (s *Server) mcpTool(ctx context.Context, name string, args json.RawMessage) (any, *rpcError) {
	id, op, ok := cutLast(name, "_")
	pr := principalOf(ctx)
	if !ok || (op != "knowledge" && op != "ask") || !canAny(pr, id) {
		return nil, &rpcError{-32602, "unknown tool: " + name}
	}
	scope := cloud.ScopeKnowledge
	if op == "ask" {
		scope = cloud.ScopeChat
	}
	if !pr.Can(scope, id) {
		return toolText(fmt.Sprintf("this API key lacks the %q scope for %s", scope, id), true), nil
	}
	p, err := s.catalog().Persona(ctx, id)
	if errors.Is(err, cloud.ErrNotFound) {
		return nil, &rpcError{-32602, "unknown tool: " + name}
	}
	if err != nil {
		return toolText("persona unavailable, retry: "+err.Error(), true), nil
	}
	var a struct {
		Query    string `json:"query"`
		Units    int    `json:"units"`
		Question string `json:"question"`
		Context  string `json:"context"`
	}
	if len(args) > 0 {
		if err := json.Unmarshal(args, &a); err != nil {
			return nil, &rpcError{-32602, "invalid arguments: " + err.Error()}
		}
	}
	start := time.Now()
	if op == "knowledge" {
		if strings.TrimSpace(a.Query) == "" {
			return toolText("query is required", true), nil
		}
		units := min(a.Units, 32) // 0: the pack's default budget
		k, err := p.Retrieve(ctx, a.Query, persona.RetrieveOptions{Units: units, Sources: -1})
		if err != nil {
			return toolText(err.Error(), true), nil
		}
		s.record(ctx, id, "mcp_knowledge", llm.Usage{}, start)
		if k.Empty() {
			return toolText("Nothing in "+p.Spec.Name+"'s knowledge matches this question. Search elsewhere or ask the user.", false), nil
		}
		return toolText(strings.TrimSpace(k.Text()), false), nil
	}
	if s.Chat == nil {
		return toolText("no chat model is configured on this server", true), nil
	}
	if strings.TrimSpace(a.Question) == "" {
		return toolText("question is required", true), nil
	}
	reply, err := p.Chat(ctx, s.Chat, []llm.Message{{Role: "user", Content: a.Question}}, persona.ChatOptions{Context: a.Context})
	if err != nil {
		return toolText(err.Error(), true), nil
	}
	s.record(ctx, id, "mcp_ask", reply.Usage, start)
	return toolText(reply.Content, false), nil
}

// cutLast splits s around the last sep.
func cutLast(s, sep string) (before, after string, found bool) {
	if i := strings.LastIndex(s, sep); i >= 0 {
		return s[:i], s[i+len(sep):], true
	}
	return s, "", false
}
