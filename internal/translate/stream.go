package translate

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/omniapi/omni-api/internal/model"
)

// Frame is one complete SSE event (multi-line data has already been joined).
type Frame struct{ Event, Data, ID, Comment string }

func (f Frame) Bytes() []byte {
	var b strings.Builder
	if f.Comment != "" {
		for _, line := range strings.Split(f.Comment, "\n") {
			fmt.Fprintf(&b, ":%s\n", line)
		}
	}
	if f.Event != "" {
		fmt.Fprintf(&b, "event: %s\n", f.Event)
	}
	if f.ID != "" {
		fmt.Fprintf(&b, "id: %s\n", f.ID)
	}
	if f.Data != "" {
		for _, line := range strings.Split(f.Data, "\n") {
			fmt.Fprintf(&b, "data: %s\n", line)
		}
	}
	b.WriteByte('\n')
	return []byte(b.String())
}

type streamBlock struct {
	part
	Key                  string
	Done, Started, Ended bool
	Sent                 int
	Index, ToolIndex     int
	Fallback             string
}

// Stream maintains the per-request block and tool-call state. Anthropic output
// serializes interleaved blocks, while Chat and Responses retain their indices.
type Stream struct {
	From, To                    model.Protocol
	Model, ID                   string
	Request                     object
	blocks                      []*streamBlock
	lookup                      map[string]*streamBlock
	itemKeys                    map[string]string
	usage                       model.Usage
	stop                        string
	stopSequence                any
	started, finished, terminal bool
	seq                         int
	created                     int64
	chatText, chatThinking      int
	out                         []Frame
}

func NewStream(from, to model.Protocol, modelName, id string, request object) *Stream {
	return &Stream{From: from, To: to, Model: modelName, ID: id, Request: request, lookup: map[string]*streamBlock{}, itemKeys: map[string]string{}, created: time.Now().Unix()}
}
func (s *Stream) Usage() model.Usage {
	u := s.usage
	if s.From == model.ProtocolAnthropic {
		u.InputTokens += u.CachedTokens + u.CacheCreationTokens
	}
	return u
}
func (s *Stream) Done() bool { return s.finished }
func (s *Stream) emit(event string, b object) {
	if s.To == model.ProtocolOpenAIResponses {
		s.seq++
		b["sequence_number"] = s.seq
	}
	data, _ := json.Marshal(b)
	s.out = append(s.out, Frame{Event: event, Data: string(data)})
}
func (s *Stream) block(key, kind string) *streamBlock {
	if b := s.lookup[key]; b != nil {
		return b
	}
	b := &streamBlock{Key: key, part: part{Kind: kind}, Index: len(s.blocks)}
	s.blocks = append(s.blocks, b)
	s.lookup[key] = b
	return b
}
func (s *Stream) closeBlock(b *streamBlock) {
	if b == nil {
		return
	}
	if b.Kind == "tool" && b.Arguments == "" {
		b.Arguments = first(b.Fallback, "{}")
	}
	b.Done = true
}
func (s *Stream) closeAll() {
	for _, b := range s.blocks {
		s.closeBlock(b)
	}
}

// Push accepts a complete upstream event and returns zero or more client events.
func (s *Stream) Push(f Frame) ([]Frame, error) {
	if s.finished {
		return nil, nil
	}
	s.out = nil
	if f.Data == "" {
		if s.From == s.To {
			return []Frame{f}, nil
		}
		return nil, nil
	}
	if f.Data == "[DONE]" {
		s.terminal = true
		s.closeAll()
		if s.From == s.To {
			s.finished = true
			return []Frame{f}, nil
		}
		return s.render(true)
	}
	var body object
	if err := json.Unmarshal([]byte(f.Data), &body); err != nil {
		return nil, fmt.Errorf("upstream stream contained invalid JSON")
	}
	if body["error"] != nil || str(body["type"]) == "error" || str(body["type"]) == "response.failed" || f.Event == "error" {
		e := body["error"]
		if r := obj(body["response"]); r != nil {
			e = r["error"]
		}
		msg := first(str(obj(e)["message"]), str(e), str(body["message"]), "upstream reported a stream error")
		return nil, fmt.Errorf("upstream reported a stream error: %s", msg)
	}
	s.usage.Merge(ReadUsage(body))
	if s.From == s.To {
		if body["model"] != nil {
			body["model"] = s.Model
		}
		if response := obj(body["response"]); response != nil && response["model"] != nil {
			response["model"] = s.Model
		}
		if message := obj(body["message"]); message != nil && message["model"] != nil {
			message["model"] = s.Model
		}
		typ := str(body["type"])
		s.terminal = typ == "response.completed" || typ == "response.incomplete" || typ == "message_stop"
		s.finished = s.terminal
		// Chat finish_reason precedes its usage-only chunk and [DONE].
		for _, c := range arr(body["choices"]) {
			if reason := str(obj(c)["finish_reason"]); reason != "" {
				s.stop = reason
			}
		}
		data, _ := json.Marshal(body)
		f.Data = string(data)
		return []Frame{f}, nil
	}
	var err error
	switch s.From {
	case model.ProtocolOpenAIChat:
		err = s.chat(body)
	case model.ProtocolOpenAIResponses:
		err = s.responses(body)
	case model.ProtocolAnthropic:
		err = s.anthropic(body)
	}
	if err != nil {
		return nil, err
	}
	return s.render(s.terminal)
}

// End treats transport EOF without a protocol terminal marker as a failure.
func (s *Stream) End() ([]Frame, error) {
	if s.finished {
		return nil, nil
	}
	if s.From == model.ProtocolOpenAIChat && s.stop != "" {
		if s.From == s.To {
			s.finished = true
			return []Frame{{Data: "[DONE]"}}, nil
		}
		s.closeAll()
		return s.render(true)
	}
	return nil, fmt.Errorf("upstream stream ended without a terminal event")
}

func (s *Stream) chat(body object) error {
	for _, v := range arr(body["choices"]) {
		choice := obj(v)
		if integer(choice["index"]) != 0 {
			return fmt.Errorf("multiple streaming choices cannot be converted")
		}
		d := obj(choice["delta"])
		if text := str(d["content"]); text != "" {
			key := fmt.Sprintf("text-%d", s.chatText)
			b := s.block(key, "text")
			if b.Done {
				s.chatText++
				b = s.block(fmt.Sprintf("text-%d", s.chatText), "text")
			}
			b.Text += text
		}
		if text := first(str(d["reasoning_content"]), str(d["reasoning"])); text != "" {
			key := fmt.Sprintf("thinking-%d", s.chatThinking)
			b := s.block(key, "thinking")
			if b.Done {
				s.chatThinking++
				b = s.block(fmt.Sprintf("thinking-%d", s.chatThinking), "thinking")
			}
			b.Text += text
		}
		if text := str(d["refusal"]); text != "" {
			s.block("refusal", "refusal").Text += text
		}
		for _, v := range arr(d["tool_calls"]) {
			call := obj(v)
			for _, b := range s.blocks {
				if b.Kind != "tool" {
					s.closeBlock(b)
				}
			}
			index := integer(call["index"])
			b := s.block(fmt.Sprintf("tool-%d", index), "tool")
			b.ToolIndex = index
			b.ID += str(call["id"])
			f := obj(call["function"])
			b.Name += str(f["name"])
			b.Arguments += str(f["arguments"])
		}
		if reason := str(choice["finish_reason"]); reason != "" {
			s.stop = reason
			s.closeAll()
		}
	}
	return nil
}

func (s *Stream) anthropic(body object) error {
	key := fmt.Sprintf("block-%d", integer(body["index"]))
	switch typ := str(body["type"]); typ {
	case "content_block_start":
		p := obj(body["content_block"])
		kind := str(p["type"])
		if kind == "tool_use" {
			kind = "tool"
		}
		if kind != "text" && kind != "thinking" && kind != "tool" {
			return fmt.Errorf("upstream content block %q cannot be converted", kind)
		}
		b := s.block(key, kind)
		b.Text = first(str(p["text"]), str(p["thinking"]))
		b.ID = str(p["id"])
		b.Name = str(p["name"])
		b.Signature = str(p["signature"])
		if kind == "tool" && p["input"] != nil {
			b.Fallback = jsonText(p["input"])
		}
	case "content_block_delta":
		d := obj(body["delta"])
		b := s.lookup[key]
		if b == nil {
			return fmt.Errorf("upstream content delta has no matching block start")
		}
		switch str(d["type"]) {
		case "text_delta":
			b.Text += str(d["text"])
		case "thinking_delta":
			b.Text += str(d["thinking"])
		case "input_json_delta":
			b.Arguments += str(d["partial_json"])
		case "signature_delta":
			b.Signature += str(d["signature"])
		default:
			return fmt.Errorf("unsupported upstream content delta %q", str(d["type"]))
		}
	case "content_block_stop":
		s.closeBlock(s.lookup[key])
	case "message_delta":
		s.stop = str(obj(body["delta"])["stop_reason"])
		s.stopSequence = obj(body["delta"])["stop_sequence"]
	case "message_stop":
		s.closeAll()
		s.terminal = true
	}
	return nil
}

func (s *Stream) responseKey(body object) string {
	if id := str(body["item_id"]); id != "" {
		if key := s.itemKeys[id]; key != "" {
			return key
		}
		if b := s.lookup[id]; b != nil {
			return b.Key
		}
	}
	return fmt.Sprintf("output-%d", integer(body["output_index"]))
}
func (s *Stream) responseItem(body, item object, done bool) error {
	key := s.responseKey(body)
	if id := str(item["id"]); id != "" {
		s.itemKeys[id] = key
	}
	kind := str(item["type"])
	if kind == "message" {
		for index, v := range arr(item["content"]) {
			p := obj(v)
			partKey := fmt.Sprintf("%s-text-%d", key, index)
			pKind := "text"
			if str(p["type"]) == "refusal" {
				pKind = "refusal"
			}
			if str(p["type"]) != "output_text" && str(p["type"]) != "refusal" {
				return fmt.Errorf("unsupported output content %q", str(p["type"]))
			}
			b := s.block(partKey, pKind)
			if b.Text == "" {
				b.Text = first(str(p["text"]), str(p["refusal"]))
			}
			if done {
				s.closeBlock(b)
			}
		}
		if done {
			for _, b := range s.blocks {
				if strings.HasPrefix(b.Key, key+"-text-") {
					s.closeBlock(b)
				}
			}
		}
		return nil
	}
	if kind == "reasoning" {
		for index, v := range arr(item["summary"]) {
			b := s.block(fmt.Sprintf("%s-thinking-%d", key, index), "thinking")
			if b.Text == "" {
				b.Text = str(obj(v)["text"])
			}
			if done {
				s.closeBlock(b)
			}
		}
		if done {
			for _, b := range s.blocks {
				if strings.HasPrefix(b.Key, key+"-thinking-") {
					s.closeBlock(b)
				}
			}
		}
		return nil
	}
	if kind != "function_call" {
		return fmt.Errorf("upstream output type %q cannot be converted", kind)
	}
	b := s.block(key, "tool")
	b.ID = first(str(item["call_id"]), b.ID)
	b.Name = first(str(item["name"]), b.Name)
	if id := str(item["id"]); id != "" {
		s.lookup[id] = b
	}
	if args := str(item["arguments"]); args != "" {
		if !strings.HasPrefix(args, b.Arguments) {
			return fmt.Errorf("upstream final tool arguments conflict with streamed arguments")
		}
		b.Arguments = args
	}
	if done {
		s.closeBlock(b)
	}
	return nil
}
func (s *Stream) responses(body object) error {
	typ := str(body["type"])
	key := s.responseKey(body)
	switch typ {
	case "response.output_item.added", "response.output_item.done":
		return s.responseItem(body, obj(body["item"]), typ == "response.output_item.done")
	case "response.output_text.delta", "response.refusal.delta":
		kind := "text"
		if typ == "response.refusal.delta" {
			kind = "refusal"
		}
		b := s.block(fmt.Sprintf("%s-text-%d", key, integer(body["content_index"])), kind)
		b.Text += str(body["delta"])
	case "response.output_text.done", "response.refusal.done":
		kind := "text"
		if typ == "response.refusal.done" {
			kind = "refusal"
		}
		b := s.block(fmt.Sprintf("%s-text-%d", key, integer(body["content_index"])), kind)
		if b.Text == "" {
			b.Text = first(str(body["text"]), str(body["refusal"]))
		}
	case "response.reasoning_summary_text.delta":
		b := s.block(fmt.Sprintf("%s-thinking-%d", key, integer(body["summary_index"])), "thinking")
		b.Text += str(body["delta"])
	case "response.function_call_arguments.delta":
		s.block(key, "tool").Arguments += str(body["delta"])
	case "response.function_call_arguments.done":
		b := s.block(key, "tool")
		if args := str(body["arguments"]); args != "" {
			if !strings.HasPrefix(args, b.Arguments) {
				return fmt.Errorf("upstream final tool arguments conflict with streamed arguments")
			}
			b.Arguments = args
		}
	case "response.completed", "response.incomplete":
		r := obj(body["response"])
		for index, v := range arr(r["output"]) {
			if err := s.responseItem(object{"output_index": index}, obj(v), true); err != nil {
				return err
			}
		}
		if typ == "response.incomplete" {
			s.stop = first(str(obj(r["incomplete_details"])["reason"]), "max_output_tokens")
		}
		s.closeAll()
		s.terminal = true
	}
	return nil
}

func (s *Stream) start() {
	if s.started {
		return
	}
	s.started = true
	switch s.To {
	case model.ProtocolAnthropic:
		s.emit("message_start", object{"type": "message_start", "message": object{"id": "msg_" + s.ID, "type": "message", "role": "assistant", "model": s.Model, "content": []any{}, "stop_reason": nil, "stop_sequence": nil, "usage": Usage(s.To, s.Usage())}})
	case model.ProtocolOpenAIResponses:
		for _, typ := range []string{"response.created", "response.in_progress"} {
			s.emit(typ, object{"type": typ, "response": object{"id": "resp_" + s.ID, "object": "response", "model": s.Model, "status": "in_progress", "created_at": s.created, "output": []any{}, "error": nil}})
		}
	default:
		s.chatChunk(object{"role": "assistant"}, nil, nil)
	}
}
func (s *Stream) chatChunk(delta object, finish any, usage any) {
	b := object{"id": "chatcmpl_" + s.ID, "object": "chat.completion.chunk", "created": s.created, "model": s.Model, "choices": []any{object{"index": 0, "delta": delta, "finish_reason": finish}}}
	if usage != nil {
		b["usage"] = usage
	}
	s.emit("", b)
}
func (s *Stream) itemID(b *streamBlock) string { return fmt.Sprintf("resp_%s_%d", s.ID, b.Index) }
func (s *Stream) responseItemBody(b *streamBlock, done bool) object {
	status := "in_progress"
	if done {
		status = "completed"
	}
	id := s.itemID(b)
	switch b.Kind {
	case "tool":
		args := ""
		if done {
			args = b.Arguments
		}
		return object{"id": id, "type": "function_call", "status": status, "call_id": b.ID, "name": b.Name, "arguments": args}
	case "thinking":
		summary := []any{}
		if done {
			summary = append(summary, object{"type": "summary_text", "text": b.Text})
		}
		return object{"id": id, "type": "reasoning", "summary": summary}
	default:
		content := []any{}
		if done {
			p := object{"type": "output_text", "text": b.Text, "annotations": []any{}}
			if b.Kind == "refusal" {
				p = object{"type": "refusal", "refusal": b.Text}
			}
			content = append(content, p)
		}
		return object{"id": id, "type": "message", "status": status, "role": "assistant", "content": content}
	}
}
func (s *Stream) startBlock(b *streamBlock) {
	b.Started = true
	switch s.To {
	case model.ProtocolAnthropic:
		p := object{"type": "text", "text": ""}
		if b.Kind == "thinking" {
			p = object{"type": "thinking", "thinking": "", "signature": ""}
		}
		if b.Kind == "tool" {
			p = object{"type": "tool_use", "id": b.ID, "name": b.Name, "input": object{}}
		}
		s.emit("content_block_start", object{"type": "content_block_start", "index": b.Index, "content_block": p})
	case model.ProtocolOpenAIResponses:
		s.emit("response.output_item.added", object{"type": "response.output_item.added", "output_index": b.Index, "item": s.responseItemBody(b, false)})
		if b.Kind == "text" || b.Kind == "refusal" {
			p := object{"type": "output_text", "text": "", "annotations": []any{}}
			if b.Kind == "refusal" {
				p = object{"type": "refusal", "refusal": ""}
			}
			s.emit("response.content_part.added", object{"type": "response.content_part.added", "output_index": b.Index, "item_id": s.itemID(b), "content_index": 0, "part": p})
		}
		if b.Kind == "thinking" {
			s.emit("response.reasoning_summary_part.added", object{"type": "response.reasoning_summary_part.added", "output_index": b.Index, "item_id": s.itemID(b), "summary_index": 0, "part": object{"type": "summary_text", "text": ""}})
		}
	default:
		if b.Kind == "tool" {
			s.chatChunk(object{"tool_calls": []any{object{"index": b.ToolIndex, "id": b.ID, "type": "function", "function": object{"name": b.Name, "arguments": ""}}}}, nil, nil)
		}
	}
}
func (s *Stream) delta(b *streamBlock, text string) {
	switch s.To {
	case model.ProtocolAnthropic:
		d := object{"type": "text_delta", "text": text}
		if b.Kind == "tool" {
			d = object{"type": "input_json_delta", "partial_json": text}
		}
		if b.Kind == "thinking" {
			d = object{"type": "thinking_delta", "thinking": text}
		}
		s.emit("content_block_delta", object{"type": "content_block_delta", "index": b.Index, "delta": d})
	case model.ProtocolOpenAIResponses:
		typ := "response.output_text.delta"
		data := object{"output_index": b.Index, "item_id": s.itemID(b), "delta": text}
		switch b.Kind {
		case "tool":
			typ = "response.function_call_arguments.delta"
		case "thinking":
			typ = "response.reasoning_summary_text.delta"
			data["summary_index"] = 0
		case "refusal":
			typ = "response.refusal.delta"
			data["content_index"] = 0
		default:
			data["content_index"] = 0
		}
		data["type"] = typ
		s.emit(typ, data)
	default:
		d := object{"content": text}
		if b.Kind == "thinking" {
			d = object{"reasoning_content": text}
		}
		if b.Kind == "refusal" {
			d = object{"refusal": text}
		}
		if b.Kind == "tool" {
			d = object{"tool_calls": []any{object{"index": b.ToolIndex, "function": object{"arguments": text}}}}
		}
		s.chatChunk(d, nil, nil)
	}
}
func (s *Stream) endBlock(b *streamBlock) {
	b.Ended = true
	switch s.To {
	case model.ProtocolAnthropic:
		s.emit("content_block_stop", object{"type": "content_block_stop", "index": b.Index})
	case model.ProtocolOpenAIResponses:
		data := object{"output_index": b.Index, "item_id": s.itemID(b)}
		switch b.Kind {
		case "tool":
			data["type"] = "response.function_call_arguments.done"
			data["arguments"] = b.Arguments
			s.emit(str(data["type"]), data)
		case "thinking":
			data["type"] = "response.reasoning_summary_text.done"
			data["summary_index"] = 0
			data["text"] = b.Text
			s.emit(str(data["type"]), data)
			s.emit("response.reasoning_summary_part.done", object{"type": "response.reasoning_summary_part.done", "output_index": b.Index, "item_id": s.itemID(b), "summary_index": 0, "part": object{"type": "summary_text", "text": b.Text}})
		default:
			typ := "response.output_text.done"
			data["text"] = b.Text
			p := object{"type": "output_text", "text": b.Text, "annotations": []any{}}
			if b.Kind == "refusal" {
				typ = "response.refusal.done"
				delete(data, "text")
				data["refusal"] = b.Text
				p = object{"type": "refusal", "refusal": b.Text}
			}
			data["type"] = typ
			data["content_index"] = 0
			s.emit(typ, data)
			s.emit("response.content_part.done", object{"type": "response.content_part.done", "output_index": b.Index, "item_id": s.itemID(b), "content_index": 0, "part": p})
		}
		s.emit("response.output_item.done", object{"type": "response.output_item.done", "output_index": b.Index, "item": s.responseItemBody(b, true)})
	}
}

func (s *Stream) render(final bool) ([]Frame, error) {
	s.start()
	toolIndex := 0
	for _, b := range s.blocks {
		if b.Kind == "tool" {
			b.ToolIndex = toolIndex
			toolIndex++
			if b.Name == "" || b.ID == "" {
				if final {
					return nil, fmt.Errorf("upstream tool call is missing its id or name")
				}
				if s.To == model.ProtocolAnthropic {
					break
				}
				continue
			}
			if b.Done {
				if _, err := toolInput(b.Arguments); err != nil {
					return nil, err
				}
			}
		}
		if b.Ended {
			continue
		}
		if !b.Started {
			s.startBlock(b)
		}
		value := b.Text
		if b.Kind == "tool" {
			value = b.Arguments
		}
		if len(value) > b.Sent {
			s.delta(b, value[b.Sent:])
			b.Sent = len(value)
		}
		if b.Done {
			s.endBlock(b)
		} else if s.To == model.ProtocolAnthropic {
			break
		}
	}
	if final {
		c := completion{Stop: s.stop, StopSequence: s.stopSequence, Usage: s.Usage()}
		for _, b := range s.blocks {
			c.Parts = append(c.Parts, b.part)
		}
		hasTool := toolIndex > 0
		reason := stopReason(s.To, s.stop, hasTool)
		switch s.To {
		case model.ProtocolAnthropic:
			s.emit("message_delta", object{"type": "message_delta", "delta": object{"stop_reason": reason, "stop_sequence": s.stopSequence}, "usage": Usage(s.To, s.Usage())})
			s.emit("message_stop", object{"type": "message_stop"})
		case model.ProtocolOpenAIResponses:
			body, err := encodeCompletion(s.To, c, s.Model, s.ID, s.Request, s.created)
			if err != nil {
				return nil, err
			}
			typ := "response.completed"
			if str(body["status"]) == "incomplete" {
				typ = "response.incomplete"
			}
			s.emit(typ, object{"type": typ, "response": body})
		default:
			s.chatChunk(object{}, reason, Usage(s.To, s.Usage()))
			s.out = append(s.out, Frame{Data: "[DONE]"})
		}
		s.finished = true
	}
	return s.out, nil
}
