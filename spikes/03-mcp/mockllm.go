package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"time"
)

// The mock model stands in for the Anthropic Messages API (Claude Code) and
// the OpenAI Responses API (Codex), so the agents make real MCP tool calls
// without real credentials. It is scripted:
//
//  1. a request that carries a tool result gets a text with that result;
//  2. otherwise, a request that offers a tool whose name contains
//     sandbox_info gets a call to that tool;
//  3. anything else gets a short fixed text.
//
// It never calls a real API: it only answers.

const (
	mockToolSubstring = "sandbox_info"
	mockResultPrefix  = "MOCK-RESULT: "
	mockPlainText     = "MOCK-OK"
	mockMaxBody       = 32 << 20 // agents send large system prompts
)

func newMockLLM(logf func(string, ...any)) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /anthropic/v1/messages", func(w http.ResponseWriter, r *http.Request) {
		mockAnthropic(w, r, logf)
	})
	mux.HandleFunc("POST /anthropic/v1/messages/count_tokens", func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, io.LimitReader(r.Body, mockMaxBody))
		logf("anthropic count_tokens")
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"input_tokens":10}`)
	})
	mux.HandleFunc("POST /openai/v1/responses", func(w http.ResponseWriter, r *http.Request) {
		mockResponses(w, r, logf)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		logf("unhandled %s %s", r.Method, r.URL.Path)
		http.NotFound(w, r)
	})
	return mux
}

func decodeBody(r *http.Request) (map[string]any, error) {
	var req map[string]any
	dec := json.NewDecoder(io.LimitReader(r.Body, mockMaxBody))
	if err := dec.Decode(&req); err != nil {
		return nil, fmt.Errorf("decode request: %w", err)
	}
	return req, nil
}

// findToolName returns the first "name" anywhere in v that contains sub.
func findToolName(v any, sub string) string {
	switch t := v.(type) {
	case map[string]any:
		if n, ok := t["name"].(string); ok && strings.Contains(n, sub) {
			return n
		}
		for _, x := range t {
			if n := findToolName(x, sub); n != "" {
				return n
			}
		}
	case []any:
		for _, x := range t {
			if n := findToolName(x, sub); n != "" {
				return n
			}
		}
	}
	return ""
}

// flatText flattens a tool result (a string, text blocks or nested
// content) to one line.
func flatText(v any) string {
	var parts []string
	var walk func(any)
	walk = func(v any) {
		switch t := v.(type) {
		case string:
			parts = append(parts, t)
		case []any:
			for _, x := range t {
				walk(x)
			}
		case map[string]any:
			if s, ok := t["text"].(string); ok {
				parts = append(parts, s)
				return
			}
			for _, k := range []string{"content", "output"} {
				if c, ok := t[k]; ok {
					walk(c)
					return
				}
			}
		}
	}
	walk(v)
	return strings.Join(strings.Fields(strings.Join(parts, " ")), " ")
}

func sseEvent(w http.ResponseWriter, event string, data any) {
	b, _ := json.Marshal(data)
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

// anthropicTurn decides the next content block for a Messages request.
func anthropicTurn(req map[string]any) (block map[string]any, stop string) {
	if msgs, _ := req["messages"].([]any); len(msgs) > 0 {
		if last, _ := msgs[len(msgs)-1].(map[string]any); last != nil {
			if blocks, _ := last["content"].([]any); blocks != nil {
				for _, b := range blocks {
					if m, _ := b.(map[string]any); m != nil && m["type"] == "tool_result" {
						return map[string]any{"type": "text", "text": mockResultPrefix + flatText(m["content"])}, "end_turn"
					}
				}
			}
		}
	}
	if tool := findToolName(req["tools"], mockToolSubstring); tool != "" {
		return map[string]any{"type": "tool_use", "id": "toolu_mock_1", "name": tool, "input": map[string]any{}}, "tool_use"
	}
	return map[string]any{"type": "text", "text": mockPlainText}, "end_turn"
}

func mockAnthropic(w http.ResponseWriter, r *http.Request, logf func(string, ...any)) {
	req, err := decodeBody(r)
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	stream, _ := req["stream"].(bool)
	block, stop := anthropicTurn(req)
	logf("anthropic messages model=%v stream=%v -> %s", req["model"], stream, block["type"])
	usage := map[string]any{"input_tokens": 10, "output_tokens": 5}
	if !stream {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"id": "msg_mock", "type": "message", "role": "assistant", "model": req["model"],
			"content": []any{block}, "stop_reason": stop, "stop_sequence": nil, "usage": usage,
		})
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	sseEvent(w, "message_start", map[string]any{"type": "message_start", "message": map[string]any{
		"id": "msg_mock", "type": "message", "role": "assistant", "model": req["model"],
		"content": []any{}, "stop_reason": nil, "stop_sequence": nil, "usage": usage,
	}})
	start := map[string]any{"type": block["type"]}
	var delta map[string]any
	if block["type"] == "tool_use" {
		start["id"], start["name"], start["input"] = block["id"], block["name"], map[string]any{}
		delta = map[string]any{"type": "input_json_delta", "partial_json": "{}"}
	} else {
		start["text"] = ""
		delta = map[string]any{"type": "text_delta", "text": block["text"]}
	}
	sseEvent(w, "content_block_start", map[string]any{"type": "content_block_start", "index": 0, "content_block": start})
	sseEvent(w, "content_block_delta", map[string]any{"type": "content_block_delta", "index": 0, "delta": delta})
	sseEvent(w, "content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
	sseEvent(w, "message_delta", map[string]any{"type": "message_delta",
		"delta": map[string]any{"stop_reason": stop, "stop_sequence": nil}, "usage": map[string]any{"output_tokens": 5}})
	sseEvent(w, "message_stop", map[string]any{"type": "message_stop"})
}

// responsesTurn decides the next output item for a Responses request.
// Codex offers MCP tools inside a namespace; the call must name it.
func responsesTurn(req map[string]any) map[string]any {
	if items, _ := req["input"].([]any); items != nil {
		for i := len(items) - 1; i >= 0; i-- {
			if m, _ := items[i].(map[string]any); m != nil && m["type"] == "function_call_output" {
				return map[string]any{"type": "message", "role": "assistant", "id": "msg_mock", "status": "completed",
					"content": []any{map[string]any{"type": "output_text", "text": mockResultPrefix + flatText(m["output"]), "annotations": []any{}}}}
			}
		}
	}
	if tool := findToolName(req["tools"], mockToolSubstring); tool != "" {
		item := map[string]any{"type": "function_call", "id": "fc_mock_1", "call_id": "call_mock_1", "name": tool, "arguments": "{}", "status": "completed"}
		if ts, ok := req["tools"].([]any); ok {
			for _, t := range ts {
				if m, _ := t.(map[string]any); m != nil && m["type"] == "namespace" && findToolName(m["tools"], mockToolSubstring) != "" {
					item["namespace"] = m["name"]
				}
			}
		}
		return item
	}
	return map[string]any{"type": "message", "role": "assistant", "id": "msg_mock", "status": "completed",
		"content": []any{map[string]any{"type": "output_text", "text": mockPlainText, "annotations": []any{}}}}
}

func mockResponses(w http.ResponseWriter, r *http.Request, logf func(string, ...any)) {
	req, err := decodeBody(r)
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	item := responsesTurn(req)
	stream, _ := req["stream"].(bool)
	logf("openai responses model=%v stream=%v -> %s", req["model"], stream, item["type"])
	usage := map[string]any{"input_tokens": 10, "input_tokens_details": map[string]any{"cached_tokens": 0},
		"output_tokens": 5, "output_tokens_details": map[string]any{"reasoning_tokens": 0}, "total_tokens": 15}
	resp := map[string]any{"id": "resp_mock", "object": "response", "status": "completed", "model": req["model"],
		"output": []any{item}, "usage": usage}
	if !stream {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	sseEvent(w, "response.created", map[string]any{"type": "response.created",
		"response": map[string]any{"id": "resp_mock", "object": "response", "status": "in_progress"}})
	sseEvent(w, "response.output_item.done", map[string]any{"type": "response.output_item.done", "output_index": 0, "item": item})
	sseEvent(w, "response.completed", map[string]any{"type": "response.completed", "response": resp})
}

func runMockLLM(ctx context.Context, args []string, stderr io.Writer) error {
	fs := flag.NewFlagSet("mockllm", flag.ContinueOnError)
	fs.SetOutput(stderr)
	addr := fs.String("addr", "127.0.0.1:18080", "listen address")
	if err := fs.Parse(args); err != nil {
		return err
	}
	logger := log.New(stderr, "mcpspike mockllm: ", log.LstdFlags)
	srv := &http.Server{
		Handler:           newMockLLM(logger.Printf),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      60 * time.Second,
		ErrorLog:          logger,
	}
	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	logger.Printf("listening on %s", ln.Addr())
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	select {
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
	}
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return srv.Shutdown(sctx)
}
