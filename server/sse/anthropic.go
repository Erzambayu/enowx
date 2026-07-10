package sse

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/enowdev/enowx/core/model"
)

// WriteAnthropic streams events in the Anthropic Messages SSE shape.
//
// The stream is a sequence of typed events; the tricky part is that a normalized
// model.Event can carry text, reasoning ("thinking"), or tool calls — often in
// separate events whose .Text is empty. The encoder must forward all of them,
// each as its own content block, or a reasoning-heavy model (which streams
// reasoning-only events before any text) looks like it "stops after one line".
func WriteAnthropic(w http.ResponseWriter, s model.Stream, modelID string) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "stream unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	defer s.Close()

	emitEvent(w, fl, "message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id":      "msg_enx",
			"type":    "message",
			"role":    "assistant",
			"model":   modelID,
			"content": []any{},
		},
	})

	// A tiny state machine over content blocks. Anthropic wants each block
	// bracketed by content_block_start / content_block_stop with a monotonic
	// index; text, thinking, and each tool call are separate blocks.
	enc := &anthropicEncoder{w: w, fl: fl, curBlock: -1}
	stopReason := "end_turn"

	for {
		ev, err := s.Recv()
		if err == io.EOF || (err == nil && ev.Type == model.EventDone) {
			break
		}
		if err != nil || ev.Type == model.EventError {
			// Surface the truncation so the client doesn't treat it as a clean
			// finish. Close any open block first.
			enc.closeBlock()
			msg := "upstream stream error"
			if ev.Err != "" {
				msg = ev.Err
			} else if err != nil {
				msg = err.Error()
			}
			emitEvent(w, fl, "error", map[string]any{
				"type":  "error",
				"error": map[string]any{"type": "api_error", "message": msg},
			})
			return
		}

		if ev.Reasoning != "" {
			enc.thinkingDelta(ev.Reasoning)
		}
		if ev.Text != "" {
			enc.textDelta(ev.Text)
		}
		for _, tc := range ev.ToolCalls {
			enc.toolCall(tc)
		}
		if ev.FinishReason == "tool_calls" {
			stopReason = "tool_use"
		} else if ev.FinishReason != "" && ev.FinishReason != "stop" {
			stopReason = ev.FinishReason
		}
	}

	enc.closeBlock()
	emitEvent(w, fl, "message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": stopReason},
	})
	emitEvent(w, fl, "message_stop", map[string]any{"type": "message_stop"})
}

// anthropicEncoder tracks the currently-open content block so text/thinking/tool
// deltas each get their own correctly-indexed block.
type anthropicEncoder struct {
	w        io.Writer
	fl       http.Flusher
	curBlock int    // index of the open block, -1 if none
	curKind  string // "text" | "thinking" | "tool_use"
	nextIdx  int
}

func (e *anthropicEncoder) openBlock(kind string, block map[string]any) {
	e.closeBlock()
	e.curBlock = e.nextIdx
	e.nextIdx++
	e.curKind = kind
	emitEvent(e.w, e.fl, "content_block_start", map[string]any{
		"type":          "content_block_start",
		"index":         e.curBlock,
		"content_block": block,
	})
}

func (e *anthropicEncoder) closeBlock() {
	if e.curBlock < 0 {
		return
	}
	emitEvent(e.w, e.fl, "content_block_stop", map[string]any{
		"type": "content_block_stop", "index": e.curBlock,
	})
	e.curBlock = -1
	e.curKind = ""
}

func (e *anthropicEncoder) textDelta(text string) {
	if e.curKind != "text" {
		e.openBlock("text", map[string]any{"type": "text", "text": ""})
	}
	emitEvent(e.w, e.fl, "content_block_delta", map[string]any{
		"type":  "content_block_delta",
		"index": e.curBlock,
		"delta": map[string]any{"type": "text_delta", "text": text},
	})
}

func (e *anthropicEncoder) thinkingDelta(text string) {
	if e.curKind != "thinking" {
		e.openBlock("thinking", map[string]any{"type": "thinking", "thinking": ""})
	}
	emitEvent(e.w, e.fl, "content_block_delta", map[string]any{
		"type":  "content_block_delta",
		"index": e.curBlock,
		"delta": map[string]any{"type": "thinking_delta", "thinking": text},
	})
}

// toolCall opens a tool_use block and streams its arguments as input_json_delta.
// enx delivers a tool call's args as one chunk (ArgsDelta), so each call is a
// self-contained block.
func (e *anthropicEncoder) toolCall(tc model.ToolCallDelta) {
	e.openBlock("tool_use", map[string]any{
		"type": "tool_use", "id": tc.ID, "name": tc.Name, "input": map[string]any{},
	})
	args := tc.ArgsDelta
	if args == "" {
		args = "{}"
	}
	emitEvent(e.w, e.fl, "content_block_delta", map[string]any{
		"type":  "content_block_delta",
		"index": e.curBlock,
		"delta": map[string]any{"type": "input_json_delta", "partial_json": args},
	})
	e.closeBlock()
}

func emitEvent(w io.Writer, fl http.Flusher, event string, v any) {
	b, _ := json.Marshal(v)
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
	fl.Flush()
}
