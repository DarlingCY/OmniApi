package translate

import (
	"fmt"
	"time"

	"github.com/omniapi/omni-api/internal/model"
)

type completion struct {
	Parts        []part
	Stop         string
	StopSequence any
	Usage        model.Usage
}

func readCompletion(source model.Protocol, body object) (completion, error) {
	c := completion{Usage: TotalUsage(source, body)}
	if body["error"] != nil || str(body["status"]) == "failed" {
		return c, fmt.Errorf("upstream returned a failed response")
	}
	switch source {
	case model.ProtocolOpenAIChat:
		choices := arr(body["choices"])
		if len(choices) == 0 {
			return c, fmt.Errorf("upstream response has no choices")
		}
		choice := obj(choices[0])
		m := obj(choice["message"])
		p, err := parts(m["content"])
		if err != nil {
			return c, err
		}
		c.Parts = p
		if refusal := str(m["refusal"]); refusal != "" {
			c.Parts = append(c.Parts, part{Kind: "refusal", Text: refusal})
		}
		if thinking := first(str(m["reasoning_content"]), str(m["reasoning"])); thinking != "" {
			c.Parts = append([]part{{Kind: "thinking", Text: thinking}}, c.Parts...)
		}
		for _, v := range arr(m["tool_calls"]) {
			call := obj(v)
			f := obj(call["function"])
			c.Parts = append(c.Parts, part{Kind: "tool", ID: str(call["id"]), Name: str(f["name"]), Arguments: str(f["arguments"])})
		}
		c.Stop = str(choice["finish_reason"])
	case model.ProtocolAnthropic:
		p, err := parts(body["content"])
		if err != nil {
			return c, err
		}
		c.Parts = p
		c.Stop = str(body["stop_reason"])
		c.StopSequence = body["stop_sequence"]
	case model.ProtocolOpenAIResponses:
		for _, v := range arr(body["output"]) {
			item := obj(v)
			switch typ := str(item["type"]); typ {
			case "message":
				p, err := parts(item["content"])
				if err != nil {
					return c, err
				}
				c.Parts = append(c.Parts, p...)
			case "function_call":
				c.Parts = append(c.Parts, part{Kind: "tool", ID: str(item["call_id"]), Name: str(item["name"]), Arguments: str(item["arguments"])})
			case "reasoning":
				for _, summary := range arr(item["summary"]) {
					c.Parts = append(c.Parts, part{Kind: "thinking", Text: str(obj(summary)["text"])})
				}
			default:
				return c, fmt.Errorf("upstream output type %q cannot be converted to the client protocol", typ)
			}
		}
		if len(c.Parts) == 0 && str(body["output_text"]) != "" {
			c.Parts = []part{{Kind: "text", Text: str(body["output_text"])}}
		}
		if str(body["status"]) == "incomplete" {
			c.Stop = first(str(obj(body["incomplete_details"])["reason"]), "max_output_tokens")
		}
	}
	for _, p := range c.Parts {
		if p.Kind == "tool" {
			if p.Name == "" || p.ID == "" {
				return c, fmt.Errorf("upstream tool call is missing its id or name")
			}
			if _, err := toolInput(p.Arguments); err != nil {
				return c, err
			}
		}
	}
	return c, nil
}

func stopReason(target model.Protocol, reason string, hasTool bool) string {
	if reason == "length" || reason == "max_tokens" || reason == "max_output_tokens" {
		if target == model.ProtocolAnthropic {
			return "max_tokens"
		}
		return "length"
	}
	if reason == "content_filter" || reason == "refusal" {
		if target == model.ProtocolAnthropic {
			return "refusal"
		}
		return "content_filter"
	}
	if reason == "stop_sequence" && target == model.ProtocolAnthropic {
		return "stop_sequence"
	}
	if hasTool || reason == "tool_use" || reason == "tool_calls" {
		if target == model.ProtocolAnthropic {
			return "tool_use"
		}
		return "tool_calls"
	}
	if target == model.ProtocolAnthropic {
		return "end_turn"
	}
	return "stop"
}

func Response(from, to model.Protocol, body object, modelName, requestID string, request object) (object, error) {
	if from == to {
		out := clone(body)
		out["model"] = modelName
		return out, nil
	}
	c, err := readCompletion(from, body)
	if err != nil {
		return nil, err
	}
	return encodeCompletion(to, c, modelName, requestID, request, time.Now().Unix())
}

func encodeCompletion(target model.Protocol, c completion, modelName, id string, request object, created int64) (object, error) {
	hasTool := false
	for _, p := range c.Parts {
		hasTool = hasTool || p.Kind == "tool"
	}
	stop := stopReason(target, c.Stop, hasTool)
	switch target {
	case model.ProtocolOpenAIChat:
		m := object{"role": "assistant", "content": textParts(c.Parts)}
		calls := []any{}
		thinking := ""
		for _, p := range c.Parts {
			if p.Kind == "tool" {
				calls = append(calls, object{"id": p.ID, "type": "function", "function": object{"name": p.Name, "arguments": p.Arguments}})
			}
			if p.Kind == "thinking" {
				thinking += p.Text
			}
		}
		if len(calls) > 0 {
			m["tool_calls"] = calls
			if m["content"] == "" {
				m["content"] = nil
			}
		}
		if thinking != "" {
			m["reasoning_content"] = thinking
		}
		return object{"id": "chatcmpl_" + id, "object": "chat.completion", "created": created, "model": modelName, "choices": []any{object{"index": 0, "message": m, "finish_reason": stop}}, "usage": Usage(target, c.Usage)}, nil
	case model.ProtocolAnthropic:
		content := []any{}
		for _, p := range c.Parts {
			switch p.Kind {
			case "tool":
				input, err := toolInput(p.Arguments)
				if err != nil {
					return nil, err
				}
				content = append(content, object{"type": "tool_use", "id": p.ID, "name": p.Name, "input": input})
			case "thinking":
				content = append(content, object{"type": "thinking", "thinking": p.Text, "signature": ""})
			default:
				content = append(content, object{"type": "text", "text": p.Text})
			}
		}
		return object{"id": "msg_" + id, "type": "message", "role": "assistant", "model": modelName, "content": content, "stop_reason": stop, "stop_sequence": c.StopSequence, "usage": Usage(target, c.Usage)}, nil
	default:
		output := []any{}
		status := "completed"
		var incomplete any
		if stop == "length" || stop == "content_filter" {
			status = "incomplete"
			reason := "max_output_tokens"
			if stop == "content_filter" {
				reason = "content_filter"
			}
			incomplete = object{"reason": reason}
		}
		for index, p := range c.Parts {
			itemID := fmt.Sprintf("resp_%s_%d", id, index)
			switch p.Kind {
			case "tool":
				output = append(output, object{"id": itemID, "type": "function_call", "call_id": p.ID, "name": p.Name, "arguments": p.Arguments, "status": status})
			case "thinking":
				output = append(output, object{"id": itemID, "type": "reasoning", "summary": []any{object{"type": "summary_text", "text": p.Text}}})
			default:
				block := object{"type": "output_text", "text": p.Text, "annotations": []any{}}
				if p.Kind == "refusal" {
					block = object{"type": "refusal", "refusal": p.Text}
				}
				output = append(output, object{"id": itemID, "type": "message", "role": "assistant", "status": status, "content": []any{block}})
			}
		}
		out := object{"id": "resp_" + id, "object": "response", "created_at": created, "model": modelName, "status": status, "output": output, "output_text": textParts(c.Parts), "usage": Usage(target, c.Usage), "error": nil, "incomplete_details": incomplete}
		copyFields(out, request, "tools", "tool_choice", "instructions", "temperature", "top_p", "max_output_tokens", "parallel_tool_calls", "metadata")
		return out, nil
	}
}
