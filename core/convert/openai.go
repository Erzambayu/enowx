package convert

import (
	"encoding/json"
	"fmt"

	"github.com/enowdev/enowx/core/model"
)

type oaiChat struct {
	Model       string    `json:"model"`
	Stream      bool      `json:"stream"`
	MaxTokens   int       `json:"max_tokens"`
	Temperature *float64  `json:"temperature"`
	Messages    []oaiMsg  `json:"messages"`
	Tools       []oaiTool `json:"tools"`
}

type oaiMsg struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	ToolCallID string          `json:"tool_call_id"`
	Name       string          `json:"name"`
	ToolCalls  []oaiToolCall   `json:"tool_calls"`
}

type oaiToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type oaiTool struct {
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"function"`
}

func fromOpenAIChat(body []byte) (*model.Request, error) {
	var in oaiChat
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, fmt.Errorf("decode openai chat: %w", err)
	}
	if in.Model == "" {
		return nil, fmt.Errorf("missing model")
	}
	req := &model.Request{
		Source:      model.APIOpenAIChat,
		Model:       in.Model,
		Stream:      in.Stream,
		MaxTokens:   in.MaxTokens,
		Temperature: in.Temperature,
		Raw:         body,
	}
	for _, m := range in.Messages {
		parts := oaiContentToParts(m.Content, m.ToolCallID)
		// An assistant message may carry tool_calls alongside (or instead of)
		// text content; turn each into a tool_use part so downstream providers
		// can reconstruct the call in history.
		for _, tc := range m.ToolCalls {
			raw, _ := json.Marshal(map[string]any{
				"id": tc.ID, "name": tc.Function.Name, "arguments": tc.Function.Arguments,
			})
			parts = append(parts, model.Part{
				Type:       "tool_use",
				ToolCallID: tc.ID,
				ToolName:   tc.Function.Name,
				Raw:        raw,
			})
		}
		req.Messages = append(req.Messages, model.Message{
			Role:  model.Role(m.Role),
			Parts: parts,
		})
	}
	for _, t := range in.Tools {
		req.Tools = append(req.Tools, model.Tool{
			Name:        t.Function.Name,
			Description: t.Function.Description,
			Parameters:  t.Function.Parameters,
		})
	}
	return req, nil
}

// OpenAIBody returns a body suitable for an OpenAI chat/completions upstream:
// the original raw body when the request already came in as OpenAI chat,
// otherwise a re-encoded body (so e.g. an Anthropic request reaches an
// OpenAI-compatible provider in a format it understands).
func OpenAIBody(req *model.Request) []byte {
	if req.Source == model.APIOpenAIChat && len(req.Raw) > 0 {
		return req.Raw
	}
	return ToOpenAIChat(req)
}

// ToOpenAIChat serializes a normalized request into an OpenAI chat/completions
// body. Used by OpenAI-compatible providers (codebuddy, openaicompat) when the
// request did not originate as OpenAI (e.g. an Anthropic request), so upstream
// receives a format it understands instead of the raw inbound body.
func ToOpenAIChat(req *model.Request) []byte {
	out := map[string]any{
		"model":  req.Model,
		"stream": req.Stream,
	}
	// Ask the upstream to include a usage block in the final SSE chunk so token
	// counts are available when streaming (needed for accurate accounting).
	if req.Stream {
		out["stream_options"] = map[string]any{"include_usage": true}
	}
	if req.MaxTokens > 0 {
		out["max_tokens"] = req.MaxTokens
	}
	if req.Temperature != nil {
		out["temperature"] = *req.Temperature
	}
	msgs := make([]map[string]any, 0, len(req.Messages))
	for _, m := range req.Messages {
		msgs = append(msgs, map[string]any{"role": string(m.Role), "content": partsToOpenAIContent(m.Parts)})
	}
	out["messages"] = msgs
	if len(req.Tools) > 0 {
		tools := make([]map[string]any, 0, len(req.Tools))
		for _, t := range req.Tools {
			tools = append(tools, map[string]any{
				"type": "function",
				"function": map[string]any{
					"name":        t.Name,
					"description": t.Description,
					"parameters":  t.Parameters,
				},
			})
		}
		out["tools"] = tools
	}
	b, _ := json.Marshal(out)
	return b
}

// partsToOpenAIContent returns a plain string when the message is text-only,
// otherwise the array-of-parts form (needed for images).
func partsToOpenAIContent(parts []model.Part) any {
	hasImage := false
	for _, p := range parts {
		if p.Type == "image" {
			hasImage = true
			break
		}
	}
	if !hasImage {
		text := ""
		for _, p := range parts {
			text += p.Text
		}
		return text
	}
	arr := make([]map[string]any, 0, len(parts))
	for _, p := range parts {
		if p.Type == "image" {
			arr = append(arr, map[string]any{"type": "image_url", "image_url": map[string]any{"url": p.ImageURL}})
		} else {
			arr = append(arr, map[string]any{"type": "text", "text": p.Text})
		}
	}
	return arr
}

// oaiContentToParts handles both string content and the array-of-parts form.
func oaiContentToParts(raw json.RawMessage, toolCallID string) []model.Part {
	if len(raw) == 0 {
		return nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return []model.Part{{Type: "text", Text: s, ToolCallID: toolCallID}}
	}
	var arr []struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		ImageURL struct {
			URL string `json:"url"`
		} `json:"image_url"`
	}
	if err := json.Unmarshal(raw, &arr); err != nil {
		return nil
	}
	parts := make([]model.Part, 0, len(arr))
	for _, p := range arr {
		switch p.Type {
		case "image_url":
			parts = append(parts, model.Part{Type: "image", ImageURL: p.ImageURL.URL})
		default:
			parts = append(parts, model.Part{Type: "text", Text: p.Text})
		}
	}
	return parts
}
