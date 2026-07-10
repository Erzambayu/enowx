// Package sse encodes a normalized model.Stream into OpenAI-style SSE on the
// wire. Other wire formats get their own encoder here later.
package sse

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/enowdev/enowx/core/model"
)

// WriteOpenAI streams events as `data: {chat.completion.chunk}` lines.
func WriteOpenAI(w http.ResponseWriter, s model.Stream) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "stream unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	defer s.Close()

	for {
		ev, err := s.Recv()
		if err == io.EOF || (err == nil && ev.Type == model.EventDone) {
			fmt.Fprint(w, "data: [DONE]\n\n")
			fl.Flush()
			return
		}
		if err != nil || ev.Type == model.EventError {
			return
		}
		emit(w, fl, chunk(ev))
	}
}

func emit(w io.Writer, fl http.Flusher, v any) {
	b, _ := json.Marshal(v)
	fmt.Fprintf(w, "data: %s\n\n", b)
	fl.Flush()
}

func chunk(ev model.Event) map[string]any {
	delta := map[string]any{"content": ev.Text}
	if ev.Reasoning != "" {
		delta["reasoning_content"] = ev.Reasoning
	}
	if len(ev.ToolCalls) > 0 {
		calls := make([]map[string]any, 0, len(ev.ToolCalls))
		for _, t := range ev.ToolCalls {
			c := map[string]any{
				"index":    t.Index,
				"function": map[string]any{"name": t.Name, "arguments": t.ArgsDelta},
			}
			if t.ID != "" {
				c["id"] = t.ID
				c["type"] = "function"
			}
			calls = append(calls, c)
		}
		delta["tool_calls"] = calls
	}
	choice := map[string]any{"index": 0, "delta": delta}
	if ev.FinishReason != "" {
		choice["finish_reason"] = ev.FinishReason
	}
	return map[string]any{
		"object":  "chat.completion.chunk",
		"model":   ev.Model,
		"choices": []map[string]any{choice},
	}
}
