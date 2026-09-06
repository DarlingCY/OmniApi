package translate

import (
	"fmt"
	"strings"

	"github.com/omniapi/omni-api/internal/model"
)

// Request preserves same-protocol payloads; cross-protocol conversion keeps
// message blocks and tool call/result identities rather than stringifying them.
func Request(from, to model.Protocol, body object, modelName string) (object, error) {
	if from == to {
		out := clone(body)
		out["model"] = modelName
		return out, nil
	}
	if !from.Valid() || !to.Valid() {
		return nil, fmt.Errorf("unsupported protocol conversion %s -> %s", from, to)
	}
	for _, k := range []string{"previous_response_id", "conversation", "file_ids", "audio", "prediction"} {
		if v := body[k]; v != nil && v != "" {
			return nil, fmt.Errorf("%s requires a provider using the same protocol", k)
		}
	}
	if n := integer(body["n"]); n > 1 {
		return nil, fmt.Errorf("cross-protocol requests support n=1")
	}
	c, err := readConversation(from, body)
	if err != nil {
		return nil, err
	}
	out := object{"model": modelName}
	copyFields(out, body, "stream", "temperature", "top_p")
	maxTokens := body["max_output_tokens"]
	if maxTokens == nil {
		maxTokens = body["max_completion_tokens"]
	}
	if maxTokens == nil {
		maxTokens = body["max_tokens"]
	}
	if maxTokens != nil {
		key := "max_tokens"
		if to == model.ProtocolOpenAIResponses {
			key = "max_output_tokens"
		}
		out[key] = maxTokens
	} else if to == model.ProtocolAnthropic {
		out["max_tokens"] = 4096
	}
	if err = writeMessages(out, c.Messages, to); err != nil {
		return nil, err
	}
	if body["tools"] != nil {
		out["tools"] = writeTools(c.Tools, to)
	}
	if choice, ok := body["tool_choice"]; ok {
		converted, err := toolChoice(from, to, choice)
		if err != nil {
			return nil, err
		}
		out["tool_choice"] = converted
	}
	parallel := body["parallel_tool_calls"]
	if from == model.ProtocolAnthropic {
		if disabled, ok := obj(body["tool_choice"])["disable_parallel_tool_use"].(bool); ok {
			parallel = !disabled
		}
	}
	if parallel != nil {
		if to == model.ProtocolAnthropic {
			choice := obj(out["tool_choice"])
			if choice == nil {
				choice = object{"type": "auto"}
				out["tool_choice"] = choice
			}
			enabled, ok := parallel.(bool)
			if !ok {
				return nil, fmt.Errorf("parallel_tool_calls must be boolean")
			}
			choice["disable_parallel_tool_use"] = !enabled
		} else {
			out["parallel_tool_calls"] = parallel
		}
	}
	stop := body["stop"]
	if stop == nil {
		stop = body["stop_sequences"]
	}
	if stop != nil {
		if to == model.ProtocolOpenAIResponses {
			return nil, fmt.Errorf("stop sequences cannot be represented by Responses")
		}
		if s, ok := stop.(string); ok {
			stop = []any{s}
		}
		key := "stop"
		if to == model.ProtocolAnthropic {
			key = "stop_sequences"
		}
		out[key] = stop
	}
	if from == model.ProtocolAnthropic {
		thinking := obj(body["thinking"])
		if typ := str(thinking["type"]); typ == "enabled" || typ == "adaptive" {
			effort := str(obj(body["output_config"])["effort"])
			if effort == "" {
				budget := integer(thinking["budget_tokens"])
				effort = "medium"
				if budget > 0 && budget < 4096 {
					effort = "low"
				}
				if budget >= 16384 {
					effort = "high"
				}
			}
			if to == model.ProtocolOpenAIChat {
				out["reasoning_effort"] = effort
			} else {
				out["reasoning"] = object{"effort": effort}
			}
		}
	} else {
		effort := first(str(body["reasoning_effort"]), str(obj(body["reasoning"])["effort"]))
		if effort != "" {
			if to == model.ProtocolAnthropic {
				if effort != "none" {
					budget := 4096
					if effort == "low" || effort == "minimal" {
						budget = 1024
					}
					if effort == "high" || effort == "xhigh" {
						budget = 16384
					}
					if max := integer(out["max_tokens"]); max > 0 && budget >= max {
						budget = max - 1
					}
					if budget < 1024 {
						return nil, fmt.Errorf("thinking requires max_tokens greater than 1024")
					}
					out["thinking"] = object{"type": "enabled", "budget_tokens": budget}
				}
			} else if to == model.ProtocolOpenAIChat {
				out["reasoning_effort"] = effort
			} else {
				out["reasoning"] = object{"effort": effort}
			}
		}
	}
	if err := outputFormat(from, to, body, out); err != nil {
		return nil, err
	}
	return out, nil
}

func readConversation(from model.Protocol, body object) (conversation, error) {
	c := conversation{}
	if from == model.ProtocolAnthropic || from == model.ProtocolOpenAIResponses {
		sys := body["system"]
		if from == model.ProtocolOpenAIResponses {
			sys = body["instructions"]
		}
		p, err := parts(sys)
		if err != nil {
			return c, err
		}
		if len(p) > 0 {
			c.Messages = append(c.Messages, message{Role: "system", Parts: p})
		}
	}
	input := body["messages"]
	if from == model.ProtocolOpenAIResponses {
		input = body["input"]
		if s, ok := input.(string); ok {
			input = []any{object{"role": "user", "content": s}}
		}
	}
	for _, v := range arr(input) {
		m := obj(v)
		role := str(m["role"])
		if role == "" {
			role = "user"
		}
		if from == model.ProtocolOpenAIResponses {
			switch typ := str(m["type"]); typ {
			case "function_call":
				c.Messages = append(c.Messages, message{Role: "assistant", Parts: []part{{Kind: "tool", ID: str(m["call_id"]), Name: str(m["name"]), Arguments: str(m["arguments"])}}})
				continue
			case "function_call_output":
				p, err := parts(m["output"])
				if err != nil {
					return c, err
				}
				c.Messages = append(c.Messages, message{Role: "user", Parts: []part{{Kind: "result", ID: str(m["call_id"]), Content: p}}})
				continue
			case "reasoning":
				p := []part{}
				for _, v := range arr(m["summary"]) {
					p = append(p, part{Kind: "thinking", Text: str(obj(v)["text"])})
				}
				if len(p) > 0 {
					c.Messages = append(c.Messages, message{Role: "assistant", Parts: p})
				}
				continue
			case "", "message":
			default:
				return c, fmt.Errorf("Responses input item %q requires a same-protocol provider", typ)
			}
		}
		p, err := parts(m["content"])
		if err != nil {
			return c, err
		}
		if role == "tool" {
			p = []part{{Kind: "result", ID: str(m["tool_call_id"]), Content: p}}
			role = "user"
		}
		if from == model.ProtocolOpenAIChat {
			if thinking := first(str(m["reasoning_content"]), str(m["reasoning"])); thinking != "" {
				p = append([]part{{Kind: "thinking", Text: thinking}}, p...)
			}
			for _, call := range arr(m["tool_calls"]) {
				call := obj(call)
				f := obj(call["function"])
				p = append(p, part{Kind: "tool", ID: str(call["id"]), Name: str(f["name"]), Arguments: str(f["arguments"])})
			}
		}
		c.Messages = append(c.Messages, message{Role: role, Parts: p})
	}
	for _, v := range arr(body["tools"]) {
		t := obj(v)
		typ := str(t["type"])
		if typ != "" && typ != "function" && !(from == model.ProtocolAnthropic && typ == "custom") {
			return c, fmt.Errorf("tool type %q requires a same-protocol provider", typ)
		}
		if from == model.ProtocolOpenAIChat {
			if typ != "function" {
				return c, fmt.Errorf("Chat tools require type=function")
			}
			t = obj(t["function"])
		}
		name := str(t["name"])
		if strings.TrimSpace(name) == "" {
			return c, fmt.Errorf("tool name is required")
		}
		schema := t["parameters"]
		if from == model.ProtocolAnthropic {
			schema = t["input_schema"]
		}
		if schema == nil {
			schema = object{"type": "object", "properties": object{}}
		}
		c.Tools = append(c.Tools, tool{Name: name, Description: str(t["description"]), Parameters: schema, Strict: t["strict"]})
	}
	return c, nil
}

func writeTools(tools []tool, target model.Protocol) []any {
	out := []any{}
	for _, t := range tools {
		b := object{"name": t.Name, "description": t.Description}
		if target == model.ProtocolAnthropic {
			b["input_schema"] = t.Parameters
		} else {
			b["parameters"] = t.Parameters
		}
		if t.Strict != nil {
			b["strict"] = t.Strict
		}
		if target == model.ProtocolOpenAIChat {
			b = object{"type": "function", "function": b}
		} else if target == model.ProtocolOpenAIResponses {
			b["type"] = "function"
		}
		out = append(out, b)
	}
	return out
}

func toolChoice(from, to model.Protocol, v any) (any, error) {
	mode, name := str(v), ""
	if b := obj(v); b != nil {
		mode = str(b["type"])
		name = str(b["name"])
		if f := obj(b["function"]); f != nil {
			name = str(f["name"])
		}
	}
	if mode == "any" {
		mode = "required"
	}
	if mode == "tool" || mode == "function" {
		mode = "function"
		if name == "" {
			return nil, fmt.Errorf("tool_choice name is required")
		}
	}
	if mode != "auto" && mode != "none" && mode != "required" && mode != "function" {
		return nil, fmt.Errorf("unsupported tool_choice %q", mode)
	}
	if to == model.ProtocolAnthropic {
		if mode == "function" {
			return object{"type": "tool", "name": name}, nil
		}
		if mode == "required" {
			mode = "any"
		}
		return object{"type": mode}, nil
	}
	if mode == "function" {
		if to == model.ProtocolOpenAIChat {
			return object{"type": "function", "function": object{"name": name}}, nil
		}
		return object{"type": "function", "name": name}, nil
	}
	return mode, nil
}

func writeMessages(out object, messages []message, target model.Protocol) error {
	items, systems := []any{}, []any{}
	appendAnthropic := func(role string, blocks []any) {
		if len(blocks) == 0 {
			return
		}
		if len(items) > 0 && str(obj(items[len(items)-1])["role"]) == role {
			last := obj(items[len(items)-1])
			last["content"] = append(arr(last["content"]), blocks...)
			return
		}
		items = append(items, object{"role": role, "content": blocks})
	}
	for _, m := range messages {
		if target == model.ProtocolAnthropic && (m.Role == "system" || m.Role == "developer") {
			p, err := contentParts(m.Parts, target, m.Role)
			if err != nil {
				return err
			}
			systems = append(systems, p...)
			continue
		}
		pending := []part{}
		flush := func() error {
			if len(pending) == 0 {
				return nil
			}
			p, err := contentParts(pending, target, m.Role)
			if err != nil {
				return err
			}
			if target == model.ProtocolAnthropic {
				appendAnthropic(m.Role, p)
			} else {
				items = append(items, object{"role": m.Role, "content": p})
			}
			pending = nil
			return nil
		}
		var chatMessage object
		if target == model.ProtocolOpenAIChat {
			chatMessage = object{"role": m.Role, "content": nil}
		}
		for _, p := range m.Parts {
			switch p.Kind {
			case "text", "image", "refusal":
				pending = append(pending, p)
			case "thinking":
				if target == model.ProtocolOpenAIChat {
					chatMessage["reasoning_content"] = str(chatMessage["reasoning_content"]) + p.Text
				} else if target == model.ProtocolAnthropic {
					// Foreign signatures cannot authorize Anthropic thinking blocks.
					// Keep the readable history as assistant text instead.
					pending = append(pending, part{Kind: "text", Text: p.Text})
				} else {
					if err := flush(); err != nil {
						return err
					}
					items = append(items, object{"type": "reasoning", "summary": []any{object{"type": "summary_text", "text": p.Text}}})
				}
			case "tool":
				if p.ID == "" || p.Name == "" {
					return fmt.Errorf("tool calls require an id and name")
				}
				if target == model.ProtocolOpenAIChat {
					calls := arr(chatMessage["tool_calls"])
					chatMessage["tool_calls"] = append(calls, object{"id": p.ID, "type": "function", "function": object{"name": p.Name, "arguments": p.Arguments}})
				} else {
					if err := flush(); err != nil {
						return err
					}
					if target == model.ProtocolOpenAIResponses {
						items = append(items, object{"type": "function_call", "call_id": p.ID, "name": p.Name, "arguments": p.Arguments})
					} else {
						input, err := toolInput(p.Arguments)
						if err != nil {
							return err
						}
						appendAnthropic("assistant", []any{object{"type": "tool_use", "id": p.ID, "name": p.Name, "input": input}})
					}
				}
			case "result":
				if p.ID == "" {
					return fmt.Errorf("tool results require a tool call id")
				}
				if err := flush(); err != nil {
					return err
				}
				blocks, err := contentParts(p.Content, target, "user")
				if err != nil {
					return err
				}
				if target == model.ProtocolAnthropic {
					appendAnthropic("user", []any{object{"type": "tool_result", "tool_use_id": p.ID, "content": blocks, "is_error": p.IsError}})
				} else {
					var content any = blocks
					onlyText := true
					for _, b := range p.Content {
						if b.Kind != "text" {
							onlyText = false
						}
					}
					if onlyText {
						content = textParts(p.Content)
						if p.IsError {
							content = "Tool error: " + str(content)
						}
					} else if p.IsError {
						blocks = append([]any{object{"type": map[bool]string{true: "input_text", false: "text"}[target == model.ProtocolOpenAIResponses], "text": "Tool error:"}}, blocks...)
						content = blocks
					}
					if target == model.ProtocolOpenAIChat {
						items = append(items, object{"role": "tool", "tool_call_id": p.ID, "content": content})
					} else {
						items = append(items, object{"type": "function_call_output", "call_id": p.ID, "output": content})
					}
				}
			}
		}
		if target == model.ProtocolOpenAIChat {
			if len(pending) > 0 {
				p, err := contentParts(pending, target, m.Role)
				if err != nil {
					return err
				}
				chatMessage["content"] = p
			}
			if chatMessage["content"] != nil || chatMessage["tool_calls"] != nil || chatMessage["reasoning_content"] != nil || len(m.Parts) == 0 {
				items = append(items, chatMessage)
			}
		} else if err := flush(); err != nil {
			return err
		}
	}
	key := "messages"
	if target == model.ProtocolOpenAIResponses {
		key = "input"
	}
	out[key] = items
	if len(systems) > 0 {
		out["system"] = systems
	}
	return nil
}

func outputFormat(from, to model.Protocol, body, out object) error {
	format := obj(body["response_format"])
	if from == model.ProtocolOpenAIResponses {
		format = obj(obj(body["text"])["format"])
	}
	if from == model.ProtocolAnthropic {
		format = obj(obj(body["output_config"])["format"])
	}
	if format == nil {
		return nil
	}
	typ := str(format["type"])
	if typ == "text" {
		return nil
	}
	if to == model.ProtocolOpenAIChat {
		if from == model.ProtocolAnthropic {
			out["response_format"] = object{"type": "json_schema", "json_schema": object{"name": "response", "schema": format["schema"], "strict": true}}
		} else if typ == "json_schema" {
			f := clone(format)
			delete(f, "type")
			out["response_format"] = object{"type": typ, "json_schema": f}
		} else {
			out["response_format"] = format
		}
		return nil
	}
	if from == model.ProtocolOpenAIChat && typ == "json_schema" {
		format = clone(obj(format["json_schema"]))
		format["type"] = typ
	}
	if to == model.ProtocolOpenAIResponses {
		f := clone(format)
		if f["name"] == nil && typ == "json_schema" {
			f["name"] = "response"
		}
		out["text"] = object{"format": f}
		return nil
	}
	if typ != "json_schema" {
		return fmt.Errorf("response format %q cannot be converted to Anthropic", typ)
	}
	out["output_config"] = object{"format": object{"type": "json_schema", "schema": format["schema"]}}
	return nil
}
