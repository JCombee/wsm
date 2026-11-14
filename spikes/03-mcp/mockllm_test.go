package main

import (
	"bufio"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newMockServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(newMockLLM(t.Logf))
	t.Cleanup(srv.Close)
	return srv
}

// mockReply is a decoded JSON reply, or the data of every SSE event when
// the reply was a stream.
type mockReply struct {
	reply  map[string]any
	events []map[string]any
}

func postJSON(t *testing.T, url string, body any) mockReply {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(url, "application/json", strings.NewReader(string(b)))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var r mockReply
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			data, ok := strings.CutPrefix(sc.Text(), "data: ")
			if !ok {
				continue
			}
			var ev map[string]any
			if err := json.Unmarshal([]byte(data), &ev); err != nil {
				t.Fatalf("event %q: %v", data, err)
			}
			r.events = append(r.events, ev)
		}
		return r
	}
	if err := json.NewDecoder(resp.Body).Decode(&r.reply); err != nil {
		t.Fatal(err)
	}
	return r
}

// anthropicBlock returns the single content block of a Messages reply,
// streamed or not, with streamed deltas applied.
func anthropicBlock(t *testing.T, r mockReply) (map[string]any, string) {
	t.Helper()
	reply, events := r.reply, r.events
	if reply != nil {
		content := reply["content"].([]any)
		if len(content) != 1 {
			t.Fatalf("content %v", content)
		}
		return content[0].(map[string]any), reply["stop_reason"].(string)
	}
	var block map[string]any
	var stop string
	var types []string
	for _, ev := range events {
		types = append(types, ev["type"].(string))
		switch ev["type"] {
		case "content_block_start":
			block = ev["content_block"].(map[string]any)
		case "content_block_delta":
			d := ev["delta"].(map[string]any)
			if d["type"] == "text_delta" {
				block["text"] = block["text"].(string) + d["text"].(string)
			}
		case "message_delta":
			stop = ev["delta"].(map[string]any)["stop_reason"].(string)
		}
	}
	want := "message_start,content_block_start,content_block_delta,content_block_stop,message_delta,message_stop"
	if got := strings.Join(types, ","); got != want {
		t.Errorf("events %s, want %s", got, want)
	}
	return block, stop
}

func TestMockAnthropicToolTurn(t *testing.T) {
	srv := newMockServer(t)
	tools := []any{
		map[string]any{"name": "Bash", "input_schema": map[string]any{}},
		map[string]any{"name": "mcp__wsm__sandbox_info", "input_schema": map[string]any{}},
	}
	for _, stream := range []bool{false, true} {
		name := "nonstream"
		if stream {
			name = "stream"
		}
		t.Run(name, func(t *testing.T) {
			// Turn 1: the model calls the tool.
			req := map[string]any{"model": "mock", "stream": stream, "tools": tools,
				"messages": []any{map[string]any{"role": "user", "content": "which sandbox?"}}}
			block, stop := anthropicBlock(t, postJSON(t, srv.URL+"/anthropic/v1/messages", req))
			if block["type"] != "tool_use" || block["name"] != "mcp__wsm__sandbox_info" || stop != "tool_use" {
				t.Fatalf("turn 1: block %v stop %s", block, stop)
			}
			// Turn 2: after the tool result, a text with that result.
			req["messages"] = append(req["messages"].([]any),
				map[string]any{"role": "assistant", "content": []any{block}},
				map[string]any{"role": "user", "content": []any{map[string]any{
					"type": "tool_result", "tool_use_id": block["id"],
					"content": []any{map[string]any{"type": "text", "text": "sandbox aaaaaaaaaaaa\n"}},
				}}})
			block, stop = anthropicBlock(t, postJSON(t, srv.URL+"/anthropic/v1/messages", req))
			if block["type"] != "text" || block["text"] != "MOCK-RESULT: sandbox aaaaaaaaaaaa" || stop != "end_turn" {
				t.Fatalf("turn 2: block %v stop %s", block, stop)
			}
			// Without the tool: a plain text.
			delete(req, "tools")
			req["messages"] = req["messages"].([]any)[:1]
			block, _ = anthropicBlock(t, postJSON(t, srv.URL+"/anthropic/v1/messages", req))
			if block["type"] != "text" || block["text"] != "MOCK-OK" {
				t.Fatalf("no tools: block %v", block)
			}
		})
	}
	ct := postJSON(t, srv.URL+"/anthropic/v1/messages/count_tokens", map[string]any{"model": "mock"})
	if ct.reply["input_tokens"] == nil {
		t.Errorf("count_tokens %v", ct.reply)
	}
}

// responsesItem returns the single output item of a Responses reply.
func responsesItem(t *testing.T, r mockReply) map[string]any {
	t.Helper()
	reply, events := r.reply, r.events
	if reply != nil {
		out := reply["output"].([]any)
		if len(out) != 1 {
			t.Fatalf("output %v", out)
		}
		return out[0].(map[string]any)
	}
	var item map[string]any
	var completed bool
	for _, ev := range events {
		switch ev["type"] {
		case "response.output_item.done":
			item = ev["item"].(map[string]any)
		case "response.completed":
			completed = true
		}
	}
	if item == nil || !completed {
		t.Fatalf("events %v", events)
	}
	return item
}

func TestMockResponsesToolTurn(t *testing.T) {
	srv := newMockServer(t)
	tools := []any{
		map[string]any{"type": "function", "name": "shell"},
		map[string]any{"type": "namespace", "name": "mcp__wsm", "tools": []any{
			map[string]any{"type": "function", "name": "sandbox_info"},
		}},
	}
	for _, stream := range []bool{true, false} {
		name := "nonstream"
		if stream {
			name = "stream"
		}
		t.Run(name, func(t *testing.T) {
			input := []any{map[string]any{"type": "message", "role": "user",
				"content": []any{map[string]any{"type": "input_text", "text": "which sandbox?"}}}}
			req := map[string]any{"model": "mock", "stream": stream, "tools": tools, "input": input}
			item := responsesItem(t, postJSON(t, srv.URL+"/openai/v1/responses", req))
			if item["type"] != "function_call" || item["name"] != "sandbox_info" || item["namespace"] != "mcp__wsm" {
				t.Fatalf("turn 1: item %v", item)
			}
			req["input"] = append(input, item, map[string]any{"type": "function_call_output",
				"call_id": item["call_id"], "output": "Wall time: 0.0 seconds\nOutput:\nsandbox aaaaaaaaaaaa"})
			item = responsesItem(t, postJSON(t, srv.URL+"/openai/v1/responses", req))
			content, _ := item["content"].([]any)
			if item["type"] != "message" || item["role"] != "assistant" || len(content) != 1 {
				t.Fatalf("turn 2: item %v", item)
			}
			text := content[0].(map[string]any)["text"].(string)
			if !strings.HasPrefix(text, "MOCK-RESULT: ") || !strings.HasSuffix(text, "sandbox aaaaaaaaaaaa") || strings.Contains(text, "\n") {
				t.Fatalf("turn 2: text %q", text)
			}
		})
	}
}
