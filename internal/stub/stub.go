// Package stub is the scripted model stub's LOCAL mode (build plan §3 C18).
//
// It speaks just enough of the Anthropic Messages API (`/v1/messages`, streamed
// and not) for a fenced Claude Code to run a scripted turn without a real model:
// it sits on loopback BEHIND the forwarder, as the "model" door, so the session
// path (fence → fwd → door, token + proof) is the real one.
//
// What it will and won't do:
//   - It verifies the session token like any door (internal/token), and then
//     serves only tokens with `profile` starting "e2e-" AND `billing` = "stub".
//     Any other profile is refused 403 (a real session can never reach it by
//     accident; invariant 18, billing pinned).
//   - A script is chosen by a marker `E2E:<name>` in the user's latest message.
//     <name> only selects a row of the fixed Catalog. The tool arguments, the
//     reply text and everything else come from the catalog, never from prompt
//     text, so a prompt can't make the stub emit arbitrary tool calls.
//   - A tool call is emitted only if the request's own `tools` list offers a tool
//     with the catalog's name (MCP tools arrive as mcp__<server>__<tool>); if it
//     is missing the stub says so in text, so the e2e fails loudly.
//   - After a tool_result comes back it ends the turn with "E2E-DONE".
//
// Local mode only: --listen must be a loopback address. The separate door
// instance (ccsp-stub, in cc-secret-proxy) is out of scope tonight.
package stub

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strings"

	"github.com/landovsky/cc-sandbox/internal/token"
)

// Script is one catalog row.
type Script struct {
	// Tool is the ccf verb to call ("" = reply with Text only).
	Tool string `json:"tool,omitempty"`
	// Args are the fixed arguments. A key whose value is "$TEXT" is filled with
	// Text only if the offered tool's schema has that property.
	Args map[string]string `json:"args,omitempty"`
	Text string            `json:"text"`
}

// Catalog is the whole behaviour of the stub. It is data on purpose: reviewing
// what the stub can make an agent do means reading these rows.
var Catalog = map[string]Script{
	"hi":         {Text: "STUB-OK"},
	"outcome":    {Tool: "outcome", Args: map[string]string{"text": "e2e outcome"}, Text: "calling outcome"},
	"question":   {Tool: "question", Args: map[string]string{"text": "e2e question?"}, Text: "calling question"},
	"checkpoint": {Tool: "checkpoint", Args: map[string]string{"text": "e2e checkpoint"}, Text: "calling checkpoint"},
	"done":       {Tool: "done", Args: map[string]string{"text": "e2e done"}, Text: "calling done"},
}

// DefaultAud is the stub door's audience: never the real model door's, so a stub token is useless there.
const DefaultAud = "ccsbx-stub"

// DoneText ends a turn after a tool result.
const DoneText = "E2E-DONE"

var marker = regexp.MustCompile(`E2E:([a-z][a-z-]{0,30})`)

type request struct {
	Model    string `json:"model"`
	Stream   bool   `json:"stream"`
	Messages []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"messages"`
	Tools []struct {
		Name        string `json:"name"`
		InputSchema struct {
			Properties map[string]json.RawMessage `json:"properties"`
		} `json:"input_schema"`
	} `json:"tools"`
}

type block struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// lastUser returns the user text of the latest message and whether it carries
// a tool_result (the turn after a tool call).
func (r *request) lastUser() (text string, isToolResult bool) {
	if len(r.Messages) == 0 {
		return "", false
	}
	// Claude Code (2.1.311, seen in the e2e suite) ends the messages array with a {"role":"system"} entry after the user's turn;
	// the turn the stub answers is the last USER message, so trailing system entries are skipped. An assistant message last
	// (a prefill) is still no user turn.
	i := len(r.Messages) - 1
	for i >= 0 && r.Messages[i].Role == "system" {
		i--
	}
	if i < 0 || r.Messages[i].Role != "user" {
		return "", false
	}
	m := r.Messages[i]
	var s string
	if json.Unmarshal(m.Content, &s) == nil {
		return s, false
	}
	var bs []block
	if json.Unmarshal(m.Content, &bs) != nil {
		return "", false
	}
	var sb strings.Builder
	for _, b := range bs {
		switch b.Type {
		case "tool_result":
			isToolResult = true
		case "text":
			sb.WriteString(b.Text + "\n")
		}
	}
	return sb.String(), isToolResult
}

// Handler is the stub door. It must be wrapped by the token verifier
// (Gate does that); it checks the e2e gate itself.
func Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok\n") })
	mux.HandleFunc("/v1/messages/count_tokens", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"input_tokens": 10})
	})
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"data": []any{}, "has_more": false})
	})
	mux.HandleFunc("/v1/messages", messages)
	return mux
}

// Gate wraps h so only verified e2e/stub sessions get through.
func Gate(v *token.Verifier, h http.Handler, logf func(string, ...any)) http.Handler {
	return v.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c := token.ClaimsFrom(r)
		if !strings.HasPrefix(c.Profile, "e2e-") || c.Billing != "stub" {
			if logf != nil {
				logf("stub refused sid=%s profile=%q billing=%q", c.SID(), c.Profile, c.Billing)
			}
			writeJSON(w, 403, map[string]any{"type": "error", "error": map[string]string{
				"type": "permission_error", "message": "the scripted stub serves only e2e-* profiles with billing=stub"}})
			return
		}
		h.ServeHTTP(w, r)
	}), logf)
}

func messages(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, 405, map[string]any{"type": "error"})
		return
	}
	var req request
	if err := json.NewDecoder(io.LimitReader(r.Body, 32<<20)).Decode(&req); err != nil {
		writeJSON(w, 400, map[string]any{"type": "error", "error": map[string]string{"type": "invalid_request_error", "message": "bad json"}})
		return
	}
	text, toolResult := req.lastUser()
	var content []map[string]any
	stop := "end_turn"
	switch {
	case toolResult:
		content = append(content, textBlock(DoneText))
	default:
		name := "hi"
		if m := marker.FindStringSubmatch(text); m != nil {
			name = m[1]
		}
		sc, ok := Catalog[name]
		switch {
		case !ok:
			content = append(content, textBlock("E2E: no script "+name+" in the catalog ("+strings.Join(catalogNames(), ", ")+")"))
		case sc.Tool == "":
			content = append(content, textBlock(sc.Text))
		default:
			tool, props := findTool(&req, sc.Tool)
			if tool == "" {
				content = append(content, textBlock("E2E: tool "+sc.Tool+" is not offered to this session"))
				break
			}
			args := map[string]string{}
			for k, v := range sc.Args {
				if _, has := props[k]; has || len(props) == 0 {
					args[k] = v
				}
			}
			content = append(content, textBlock(sc.Text), map[string]any{
				"type": "tool_use", "id": "toolu_stub_" + sc.Tool, "name": tool, "input": args})
			stop = "tool_use"
		}
	}
	msg := map[string]any{"id": "msg_stub", "type": "message", "role": "assistant", "model": orDefault(req.Model, "stub"),
		"content": []any{}, "stop_reason": nil, "stop_sequence": nil,
		"usage": map[string]int{"input_tokens": 10, "output_tokens": 1}}
	if !req.Stream {
		msg["content"], msg["stop_reason"] = content, stop
		writeJSON(w, 200, msg)
		return
	}
	stream(w, msg, content, stop)
}

// findTool returns the offered tool matching verb. Claude Code names an MCP tool mcp__<server>__<tool>, and the ccf server
// (dotfiles ccf/ccf-mcp) names its tools ccf_<verb>, so the real name is mcp__ccf__ccf_<verb>; the shorter forms stay
// for scripts and tests that offer them.
func findTool(req *request, verb string) (string, map[string]json.RawMessage) {
	for _, t := range req.Tools {
		if t.Name == "mcp__ccf__ccf_"+verb || t.Name == "mcp__ccf__"+verb || t.Name == "ccf_"+verb { // exact: a same-suffix tool of another server must not match
			return t.Name, t.InputSchema.Properties
		}
	}
	return "", nil
}

func catalogNames() []string {
	var n []string
	for k := range Catalog {
		n = append(n, k)
	}
	sort.Strings(n)
	return n
}

func textBlock(s string) map[string]any { return map[string]any{"type": "text", "text": s} }

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func sse(w http.ResponseWriter, ev string, data any) {
	b, _ := json.Marshal(data)
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev, b)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

func stream(w http.ResponseWriter, msg map[string]any, content []map[string]any, stop string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(200)
	sse(w, "message_start", map[string]any{"type": "message_start", "message": msg})
	for i, b := range content {
		if b["type"] == "text" {
			sse(w, "content_block_start", map[string]any{"type": "content_block_start", "index": i, "content_block": map[string]any{"type": "text", "text": ""}})
			sse(w, "content_block_delta", map[string]any{"type": "content_block_delta", "index": i, "delta": map[string]any{"type": "text_delta", "text": b["text"]}})
		} else {
			sse(w, "content_block_start", map[string]any{"type": "content_block_start", "index": i, "content_block": map[string]any{
				"type": "tool_use", "id": b["id"], "name": b["name"], "input": map[string]any{}}})
			in, _ := json.Marshal(b["input"])
			sse(w, "content_block_delta", map[string]any{"type": "content_block_delta", "index": i, "delta": map[string]any{"type": "input_json_delta", "partial_json": string(in)}})
		}
		sse(w, "content_block_stop", map[string]any{"type": "content_block_stop", "index": i})
	}
	sse(w, "message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": stop, "stop_sequence": nil}, "usage": map[string]int{"output_tokens": 3}})
	sse(w, "message_stop", map[string]any{"type": "message_stop"})
}
