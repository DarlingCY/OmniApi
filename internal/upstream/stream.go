package upstream

import (
	"bufio"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/omniapi/omni-api/internal/model"
	"github.com/omniapi/omni-api/internal/translate"
)

// Stream yields decoded text deltas from an upstream SSE response.
// The first delta is read eagerly so provider failover can still happen
// before any bytes reach the downstream client.
type Stream struct {
	translation *translate.Stream
	frames      []translate.Frame
	response    *http.Response
	protocol    model.Protocol
	lines       *bufio.Scanner
	first       string
	pending     bool
	done        bool
	err         error
	usage       model.Usage
}

func newStream(protocol model.Protocol, response *http.Response) (*Stream, error) {
	stream := &Stream{response: response, protocol: protocol}
	first, ok := stream.read()
	if !ok {
		stream.Close()
		if stream.Err() != nil {
			return nil, stream.Err()
		}
		return nil, &Failure{Status: 502, Message: "upstream stream ended before the first event"}
	}
	stream.first = first
	stream.pending = true
	return stream, nil
}

// Next returns the next text delta, or false when the stream is complete.
func (s *Stream) Next() (string, bool) {
	if s.pending {
		s.pending = false
		return s.first, true
	}
	return s.read()
}

// Close releases the upstream connection.
func (s *Stream) Close() {
	if s.response != nil {
		s.response.Body.Close()
	}
}

// Usage returns the final counters reported by the upstream stream.
func (s *Stream) Usage() model.Usage {
	if s.translation != nil {
		return s.translation.Usage()
	}
	return s.usage
}

// Err distinguishes a failed upstream stream from normal completion.
func (s *Stream) Err() error { return s.err }

func (s *Stream) read() (string, bool) {
	if s.done {
		return "", false
	}
	if s.lines == nil {
		s.lines = scanner(s.response)
	}
	for s.lines.Scan() {
		line := strings.TrimSpace(s.lines.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			s.done = true
			return "", false
		}
		if data == "" {
			continue
		}
		var event map[string]any
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			s.err = &Failure{Status: 502, Message: "upstream stream contained invalid JSON"}
			s.done = true
			return "", false
		}
		if event["error"] != nil || event["type"] == "error" || event["type"] == "response.failed" {
			errorData := []byte(data)
			if response, ok := event["response"].(map[string]any); ok {
				errorData, _ = json.Marshal(response)
			}
			s.err = &Failure{Status: 502, Message: "upstream reported a stream error", Cause: errors.New(publicError(errorData, 502, "upstream reported a stream error"))}
			s.done = true
			return "", false
		}
		if eventUsage := streamUsage(data); !eventUsage.Empty() {
			s.usage.Merge(eventUsage)
		}
		if event["type"] == "response.completed" || event["type"] == "message_stop" {
			s.done = true
			return "", false
		}
		if delta := textDelta(s.protocol, data); delta != "" {
			return delta, true
		}
	}
	if err := s.lines.Err(); err != nil {
		s.err = &Failure{Status: 502, Message: "upstream stream read failed", Cause: err}
	}
	s.done = true
	return "", false
}

func streamUsage(data string) model.Usage {
	var event map[string]any
	if err := json.Unmarshal([]byte(data), &event); err != nil {
		return model.Usage{}
	}
	return usage(event)
}

func textDelta(protocol model.Protocol, data string) string {
	var event map[string]any
	if err := json.Unmarshal([]byte(data), &event); err != nil {
		return ""
	}
	switch protocol {
	case model.ProtocolOpenAIChat:
		choices, ok := event["choices"].([]any)
		if !ok || len(choices) == 0 {
			return ""
		}
		choice, ok := choices[0].(map[string]any)
		if !ok {
			return ""
		}
		delta, ok := choice["delta"].(map[string]any)
		if !ok {
			return ""
		}
		content, _ := delta["content"].(string)
		return content
	case model.ProtocolOpenAIResponses:
		if event["type"] != "response.output_text.delta" {
			return ""
		}
		delta, _ := event["delta"].(string)
		return delta
	default:
		if event["type"] != "content_block_delta" {
			return ""
		}
		delta, ok := event["delta"].(map[string]any)
		if !ok || delta["type"] != "text_delta" {
			return ""
		}
		text, _ := delta["text"].(string)
		return text
	}
}
