package kiro

import (
	"encoding/json"
	"testing"

	"github.com/enowdev/enowx/core/model"
)

func TestBuildPayloadCarriesTools(t *testing.T) {
	req := &model.Request{
		Model: "kr/auto",
		Messages: []model.Message{
			{Role: model.RoleUser, Parts: []model.Part{{Type: "text", Text: "make a file"}}},
		},
		Tools: []model.Tool{
			{Name: "write_file", Description: "write a file", Parameters: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path","missing"]}`)},
		},
	}
	body, reverse, err := buildPayload(req, "arn:x", "conv1")
	if err != nil {
		t.Fatal(err)
	}
	_ = reverse
	var p map[string]any
	if err := json.Unmarshal(body, &p); err != nil {
		t.Fatal(err)
	}
	cs := p["conversationState"].(map[string]any)
	cur := cs["currentMessage"].(map[string]any)["userInputMessage"].(map[string]any)
	ctx, ok := cur["userInputMessageContext"].(map[string]any)
	if !ok {
		t.Fatalf("no userInputMessageContext; cur=%v", cur)
	}
	tools, ok := ctx["tools"].([]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("tools not carried: %v", ctx["tools"])
	}
	spec := tools[0].(map[string]any)["toolSpecification"].(map[string]any)
	if spec["name"] != "write_file" {
		t.Fatalf("bad tool name: %v", spec["name"])
	}
	sch := spec["inputSchema"].(map[string]any)["json"].(map[string]any)
	// required "missing" must be pruned (not in properties).
	reqd := sch["required"].([]any)
	if len(reqd) != 1 || reqd[0] != "path" {
		t.Fatalf("required not pruned: %v", reqd)
	}
	t.Logf("payload OK: %s", string(body)[:200])
}

func TestToolUseEventAccumulates(t *testing.T) {
	s := newStream(nil, map[string]string{"write_file": "write_file"})
	// Simulate two fragments + stop.
	s.frameToEvent(frame{eventType: "toolUseEvent", payload: []byte(`{"toolUseId":"t1","name":"write_file","input":"{\"path\":"}`)})
	s.frameToEvent(frame{eventType: "toolUseEvent", payload: []byte(`{"toolUseId":"t1","input":"\"a.rs\"}","stop":true}`)})
	ev, ok := s.flushToolCalls()
	if !ok || len(ev.ToolCalls) != 1 {
		t.Fatalf("no tool calls flushed: %+v", ev)
	}
	tc := ev.ToolCalls[0]
	if tc.ID != "t1" || tc.Name != "write_file" || tc.ArgsDelta != `{"path":"a.rs"}` {
		t.Fatalf("bad tool call: %+v", tc)
	}
	if ev.FinishReason != "tool_calls" {
		t.Fatalf("finish reason: %s", ev.FinishReason)
	}
}
