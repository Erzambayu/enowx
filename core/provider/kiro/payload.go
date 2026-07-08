package kiro

import (
	"encoding/json"
	"strings"

	"github.com/enowdev/enowx/core/model"
	"github.com/google/uuid"
)

// buildPayload turns the normalized request into the CodeWhisperer body. The
// last user turn becomes currentMessage; earlier turns become history. System
// turns are prepended to the first user content. Tools are attached to the
// current user message's userInputMessageContext; assistant tool calls and tool
// results are threaded through history so multi-turn tool use works.
//
// Returns the body plus a reverse map (sanitized→original tool name) so the
// response decoder can restore the client's tool names.
func buildPayload(req *model.Request, profileARN, conversationID string) ([]byte, map[string]string, error) {
	if conversationID == "" {
		conversationID = uuid.NewString()
	}
	reverse := map[string]string{}
	toolSpecs := buildToolSpecs(req.Tools, reverse)

	var system strings.Builder
	type turn struct {
		role  string
		parts []model.Part
	}
	var turns []turn
	for _, m := range req.Messages {
		if m.Role == model.RoleSystem {
			if t := partsText(m.Parts); t != "" {
				system.WriteString(t)
				system.WriteString("\n\n")
			}
			continue
		}
		turns = append(turns, turn{role: string(m.Role), parts: m.Parts})
	}

	sysPrefix := system.String()

	history := make([]map[string]any, 0, len(turns))
	var current map[string]any
	for i, t := range turns {
		isLast := i == len(turns)-1
		prefix := ""
		if i == 0 && t.role == string(model.RoleUser) {
			prefix = sysPrefix
		}

		switch t.role {
		case string(model.RoleAssistant):
			msg := assistantMessage(t.parts)
			if isLast {
				// An assistant turn can't be the current message; keep it in history
				// and leave an empty current user message for CodeWhisperer.
				history = append(history, map[string]any{"assistantResponseMessage": msg})
				current = userInput("", req.Model, nil, nil)
			} else {
				history = append(history, map[string]any{"assistantResponseMessage": msg})
			}
		case string(model.RoleTool):
			results := toolResults(t.parts)
			// Bedrock requires toolConfig (the tool specs) on any message that
			// carries toolResult blocks — so advertise tools on the current turn
			// too, not just plain user turns.
			var specs []any
			if isLast {
				specs = toolSpecs
			}
			um := userInput("", req.Model, specs, results)
			if isLast {
				current = um
			} else {
				history = append(history, map[string]any{"userInputMessage": um})
			}
		default: // user
			content := prefix + partsText(t.parts)
			var specs []any
			if isLast {
				specs = toolSpecs // advertise tools on the live turn
			}
			um := userInput(content, req.Model, specs, nil)
			if isLast {
				current = um
			} else {
				history = append(history, map[string]any{"userInputMessage": um})
			}
		}
	}
	if current == nil {
		current = userInput("", req.Model, toolSpecs, nil)
	}

	payload := map[string]any{
		"conversationState": map[string]any{
			"conversationId":  conversationID,
			"chatTriggerType": "MANUAL",
			"currentMessage":  map[string]any{"userInputMessage": current},
			"history":         history,
		},
	}
	if profileARN != "" {
		payload["profileArn"] = profileARN
	}
	b, err := json.Marshal(payload)
	return b, reverse, err
}

// userInput builds a userInputMessage. tools + toolResults ride in
// userInputMessageContext when present.
func userInput(content, modelID string, tools, toolResults []any) map[string]any {
	m := map[string]any{
		"content": content,
		"modelId": modelID,
		"origin":  "AI_EDITOR",
	}
	ctx := map[string]any{}
	if len(tools) > 0 {
		ctx["tools"] = tools
	}
	if len(toolResults) > 0 {
		ctx["toolResults"] = toolResults
	}
	if len(ctx) > 0 {
		m["userInputMessageContext"] = ctx
	}
	return m
}

// assistantMessage encodes an assistant turn: its text plus any tool calls as
// CodeWhisperer toolUses.
func assistantMessage(parts []model.Part) map[string]any {
	msg := map[string]any{"content": partsText(parts)}
	var uses []any
	for _, p := range parts {
		if p.Type == "tool_use" || p.Type == "tool_call" {
			id, name, args := toolCallFields(p)
			if s, ok := sanitizedToolName(name); ok {
				name = s
			}
			uses = append(uses, map[string]any{
				"toolUseId": id,
				"name":      name,
				"input":     jsonObj(args),
			})
		}
	}
	if len(uses) > 0 {
		msg["toolUses"] = uses
	}
	return msg
}

// toolResults encodes tool-result parts into CodeWhisperer toolResults.
func toolResults(parts []model.Part) []any {
	var out []any
	for _, p := range parts {
		if p.ToolCallID == "" && p.Type != "tool_result" {
			continue
		}
		out = append(out, map[string]any{
			"toolUseId": p.ToolCallID,
			"status":    "success",
			"content":   []any{map[string]any{"text": p.Text}},
		})
	}
	return out
}

func partsText(parts []model.Part) string {
	var b strings.Builder
	for _, p := range parts {
		if p.Text != "" && p.Type != "tool_use" && p.Type != "tool_call" {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

// sanitizedToolName re-sanitizes an assistant tool name to match what we
// advertised (kept minimal; the reverse map handles the response side).
func sanitizedToolName(name string) (string, bool) {
	s := toolNameRe.ReplaceAllString(name, "_")
	if s == "" || !toolNameStartRe.MatchString(s) {
		s = "_" + s
	}
	if len(s) > 64 {
		s = s[:64]
	}
	return s, s != name
}
