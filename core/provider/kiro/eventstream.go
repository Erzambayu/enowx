package kiro

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"io"
	"net/http"
	"strings"

	"github.com/enowdev/enowx/core/model"
)

// stream decodes the AWS binary event-stream response into model events. Frames
// are length-prefixed with CRC-checked prelude and message; each carries a JSON
// payload tagged by the ":event-type" header.
type stream struct {
	resp    *http.Response
	buf     []byte
	done    bool
	reverse map[string]string // sanitized→original tool name

	// tool-call accumulation: CodeWhisperer streams a tool's input as JSON
	// fragments across several toolUseEvent frames, keyed by toolUseId.
	toolIdx  int
	toolSeq  []string          // toolUseId in first-seen order
	toolName map[string]string // toolUseId → name
	toolArgs map[string]string // toolUseId → accumulated input JSON
	toolIx      map[string]int    // toolUseId → stream index
	sawTool     bool
	pendingDone bool // a Done event is owed after a tool-calls finish event
}

func newStream(resp *http.Response, reverse map[string]string) *stream {
	return &stream{
		resp:     resp,
		reverse:  reverse,
		toolName: map[string]string{},
		toolArgs: map[string]string{},
		toolIx:   map[string]int{},
	}
}

func (s *stream) Close() error { return s.resp.Body.Close() }

func (s *stream) Recv() (model.Event, error) {
	for {
		if ev, ok, err := s.next(); err != nil {
			return model.Event{}, err
		} else if ok {
			return ev, nil
		}
		// need more bytes
		tmp := make([]byte, 16*1024)
		n, err := s.resp.Body.Read(tmp)
		if n > 0 {
			s.buf = append(s.buf, tmp[:n]...)
			continue
		}
		if err == io.EOF {
			if !s.done {
				s.done = true
				// Flush any accumulated tool calls as a final finish event before
				// signalling done, so the client sees FinishReason=tool_calls.
				if ev, ok := s.flushToolCalls(); ok {
					// Re-queue the Done for the next Recv by leaving s.done set and
					// returning the finish event now.
					s.pendingDone = true
					return ev, nil
				}
				return model.Event{Type: model.EventDone}, nil
			}
			if s.pendingDone {
				s.pendingDone = false
				return model.Event{Type: model.EventDone}, nil
			}
			return model.Event{}, io.EOF
		}
		if err != nil {
			return model.Event{}, err
		}
	}
}

// next pulls one decodable frame from the buffer, returning a meaningful event.
// ok=false means "not enough bytes yet" (caller should read more).
func (s *stream) next() (model.Event, bool, error) {
	for {
		frame, consumed, err := readFrame(s.buf)
		if err != nil {
			return model.Event{}, false, err
		}
		if consumed == 0 {
			return model.Event{}, false, nil
		}
		s.buf = s.buf[consumed:]

		ev, meaningful := s.frameToEvent(frame)
		if meaningful {
			return ev, true, nil
		}
		// skip non-content frames and try the next one
	}
}

type frame struct {
	eventType string
	msgType   string
	payload   []byte
}

func readFrame(buf []byte) (frame, int, error) {
	if len(buf) < 12 {
		return frame{}, 0, nil
	}
	total := int(binary.BigEndian.Uint32(buf[0:4]))
	headersLen := int(binary.BigEndian.Uint32(buf[4:8]))
	if total < 16 || headersLen < 0 || headersLen > total-16 {
		return frame{}, 0, fmt.Errorf("kiro: invalid frame total=%d headers=%d", total, headersLen)
	}
	if len(buf) < total {
		return frame{}, 0, nil
	}
	if got, want := binary.BigEndian.Uint32(buf[8:12]), crc32.ChecksumIEEE(buf[0:8]); got != want {
		return frame{}, 0, fmt.Errorf("kiro: prelude crc")
	}
	if got, want := binary.BigEndian.Uint32(buf[total-4:total]), crc32.ChecksumIEEE(buf[0:total-4]); got != want {
		return frame{}, 0, fmt.Errorf("kiro: message crc")
	}
	headers := parseHeaders(buf[12 : 12+headersLen])
	payload := append([]byte(nil), buf[12+headersLen:total-4]...)
	return frame{
		eventType: headers[":event-type"],
		msgType:   headers[":message-type"],
		payload:   payload,
	}, total, nil
}

func parseHeaders(b []byte) map[string]string {
	h := map[string]string{}
	for len(b) > 0 {
		nameLen := int(b[0])
		b = b[1:]
		if len(b) < nameLen+1 {
			break
		}
		name := string(b[:nameLen])
		b = b[nameLen:]
		valueType := b[0]
		b = b[1:]
		if valueType != 7 || len(b) < 2 { // 7 = string
			break
		}
		valueLen := int(binary.BigEndian.Uint16(b[0:2]))
		b = b[2:]
		if len(b) < valueLen {
			break
		}
		h[name] = string(b[:valueLen])
		b = b[valueLen:]
	}
	return h
}

func (s *stream) frameToEvent(f frame) (model.Event, bool) {
	if f.msgType == "exception" {
		var ex struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(f.payload, &ex)
		msg := ex.Message
		if msg == "" {
			msg = string(f.payload)
		}
		return model.Event{Type: model.EventError, Err: msg}, true
	}
	switch f.eventType {
	case "assistantResponseEvent":
		var v struct {
			Content string `json:"content"`
			ModelID string `json:"modelId"`
		}
		if json.Unmarshal(f.payload, &v) == nil && v.Content != "" {
			return model.Event{Type: model.EventDelta, Text: v.Content, Model: v.ModelID}, true
		}
	case "toolUseEvent":
		// CodeWhisperer streams a tool call as one or more toolUseEvent frames:
		// the first carries toolUseId + name, subsequent frames append `input`
		// fragments, and the last has stop=true. We accumulate here and flush the
		// complete calls at end-of-stream.
		var v struct {
			ToolUseID string `json:"toolUseId"`
			Name      string `json:"name"`
			Input     string `json:"input"`
			Stop      bool   `json:"stop"`
		}
		if json.Unmarshal(f.payload, &v) == nil && v.ToolUseID != "" {
			if _, seen := s.toolIx[v.ToolUseID]; !seen {
				s.toolIx[v.ToolUseID] = s.toolIdx
				s.toolIdx++
				s.toolSeq = append(s.toolSeq, v.ToolUseID)
				s.sawTool = true
			}
			if v.Name != "" {
				s.toolName[v.ToolUseID] = v.Name
			}
			s.toolArgs[v.ToolUseID] += v.Input
			// Accumulate silently; nothing meaningful to emit per-fragment.
		}
	case "metadataEvent":
		var v struct {
			TokenUsage struct {
				InputTokens  int64 `json:"inputTokens"`
				OutputTokens int64 `json:"outputTokens"`
			} `json:"tokenUsage"`
		}
		if json.Unmarshal(f.payload, &v) == nil {
			return model.Event{Type: model.EventDelta, Usage: &model.Usage{
				PromptTokens:     v.TokenUsage.InputTokens,
				CompletionTokens: v.TokenUsage.OutputTokens,
			}}, true
		}
	}
	return model.Event{}, false
}

// flushToolCalls builds a single finish event carrying every accumulated tool
// call. Returns ok=false if no tool calls were seen.
func (s *stream) flushToolCalls() (model.Event, bool) {
	if !s.sawTool || len(s.toolSeq) == 0 {
		return model.Event{}, false
	}
	calls := make([]model.ToolCallDelta, 0, len(s.toolSeq))
	for _, id := range s.toolSeq {
		name := s.toolName[id]
		if orig, ok := s.reverse[name]; ok {
			name = orig
		}
		args := s.toolArgs[id]
		if strings.TrimSpace(args) == "" {
			args = "{}"
		}
		calls = append(calls, model.ToolCallDelta{
			Index: s.toolIx[id], ID: id, Name: name, ArgsDelta: args,
		})
	}
	return model.Event{Type: model.EventDelta, ToolCalls: calls, FinishReason: "tool_calls"}, true
}
