package kiro

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/enowdev/enowx/core/model"
)

// CodeWhisperer carries tools on the user message as
// userInputMessageContext.tools[].toolSpecification and streams tool calls back
// as toolUseEvent frames. This file builds the tool payload from the normalized
// request and holds the shared name-sanitization + schema-cleaning helpers.

var toolNameRe = regexp.MustCompile(`[^a-zA-Z0-9_]`)
var toolNameStartRe = regexp.MustCompile(`^[a-zA-Z_]`)

// buildToolSpecs converts the normalized tools into CodeWhisperer toolSpecs.
// Names are sanitized (CodeWhisperer only allows [a-zA-Z0-9_]); reverse maps the
// sanitized name back to the original so we can restore it in the response.
func buildToolSpecs(tools []model.Tool, reverse map[string]string) []any {
	if len(tools) == 0 {
		return nil
	}
	used := map[string]bool{}
	out := make([]any, 0, len(tools))
	for _, t := range tools {
		name := sanitizeToolName(t.Name, used)
		if name != t.Name {
			reverse[name] = t.Name
		}
		out = append(out, map[string]any{
			"toolSpecification": map[string]any{
				"name":        name,
				"description": t.Description,
				"inputSchema": map[string]any{"json": cleanSchema(t.Parameters)},
			},
		})
	}
	return out
}

func sanitizeToolName(name string, used map[string]bool) string {
	s := toolNameRe.ReplaceAllString(name, "_")
	if s == "" || !toolNameStartRe.MatchString(s) {
		s = "_" + s
	}
	if len(s) > 64 {
		s = s[:64]
	}
	base := s
	for i := 2; used[s]; i++ {
		s = fmt.Sprintf("%s_%d", base, i)
	}
	used[s] = true
	return s
}

// cleanSchema normalizes a JSON-schema for CodeWhisperer's inputSchema.json. It
// accepts most standard schema, but a tool with no parameters must still present
// an object schema, so default to an empty object.
func cleanSchema(raw json.RawMessage) map[string]any {
	empty := map[string]any{"type": "object", "properties": map[string]any{}}
	if len(raw) == 0 {
		return empty
	}
	var s map[string]any
	if json.Unmarshal(raw, &s) != nil {
		return empty
	}
	if _, hasType := s["type"]; !hasType {
		if _, hasProps := s["properties"]; hasProps {
			s["type"] = "object"
		} else {
			return empty
		}
	}
	// Prune required entries that don't exist in properties (CodeWhisperer 400s).
	if req, ok := s["required"].([]any); ok {
		if props, ok := s["properties"].(map[string]any); ok {
			kept := []any{}
			for _, r := range req {
				if name, ok := r.(string); ok {
					if _, exists := props[name]; exists {
						kept = append(kept, name)
					}
				}
			}
			s["required"] = kept
		}
	}
	return s
}

// --- helpers over parts (tool_use / tool_result) ---

func toolCallFields(p model.Part) (id, name, args string) {
	id, name = p.ToolCallID, p.ToolName
	if len(p.Raw) > 0 {
		var r struct {
			ID   string          `json:"id"`
			Name string          `json:"name"`
			Args json.RawMessage `json:"arguments"`
		}
		if json.Unmarshal(p.Raw, &r) == nil {
			if r.ID != "" {
				id = r.ID
			}
			if r.Name != "" {
				name = r.Name
			}
			if len(r.Args) > 0 {
				args = string(r.Args)
			}
		}
	}
	return id, name, args
}

// jsonObj parses an arguments string into a map (CodeWhisperer wants the tool
// input as a JSON object, not a string). Empty/invalid → {}.
func jsonObj(s string) map[string]any {
	if strings.TrimSpace(s) == "" {
		return map[string]any{}
	}
	var m map[string]any
	if json.Unmarshal([]byte(s), &m) != nil {
		return map[string]any{}
	}
	return m
}

// --- context plumbing for the reverse tool-name map (build → parse) ---

type reverseKey struct{}

func withReverseNames(ctx context.Context, m map[string]string) context.Context {
	return context.WithValue(ctx, reverseKey{}, m)
}

func reverseNamesFrom(ctx context.Context) map[string]string {
	if ctx == nil {
		return nil
	}
	if m, ok := ctx.Value(reverseKey{}).(map[string]string); ok {
		return m
	}
	return nil
}
