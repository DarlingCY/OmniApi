// Package translate adapts OpenAI Chat, OpenAI Responses and Anthropic Messages.
// Protocol-pair request/response boundaries and stateful streaming follow the
// design of CLIProxyAPI; the implementation here uses the Go standard library.
package translate

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/omniapi/omni-api/internal/model"
)

type object = map[string]any

func obj(v any) object { o, _ := v.(map[string]any); return o }
func arr(v any) []any  { a, _ := v.([]any); return a }
func str(v any) string { s, _ := v.(string); return s }
func integer(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	}
	return 0
}
func clone(o object) object {
	out := object{}
	for k, v := range o {
		out[k] = v
	}
	return out
}
func jsonText(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	b, _ := json.Marshal(v)
	return string(b)
}
func first(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
func copyFields(to, from object, keys ...string) {
	for _, key := range keys {
		if v, ok := from[key]; ok {
			to[key] = v
		}
	}
}

type part struct {
	Kind, Text, URL, Detail, ID, Name, Arguments, Signature string
	IsError                                                 bool
	Content                                                 []part
}
type message struct {
	Role  string
	Parts []part
}
type tool struct {
	Name, Description string
	Parameters        any
	Strict            any
}
type conversation struct {
	Messages []message
	Tools    []tool
}

func parts(value any) ([]part, error) {
	if s, ok := value.(string); ok {
		return []part{{Kind: "text", Text: s}}, nil
	}
	if value == nil {
		return nil, nil
	}
	a, ok := value.([]any)
	if !ok {
		return nil, fmt.Errorf("message content must be text or an array")
	}
	out := []part{}
	for _, item := range a {
		b := obj(item)
		switch typ := str(b["type"]); typ {
		case "text", "input_text", "output_text":
			out = append(out, part{Kind: "text", Text: str(b["text"])})
		case "refusal":
			out = append(out, part{Kind: "refusal", Text: str(b["refusal"])})
		case "image_url", "input_image":
			u, detail := str(b["image_url"]), str(b["detail"])
			if image := obj(b["image_url"]); image != nil {
				u, detail = str(image["url"]), str(image["detail"])
			}
			if u == "" {
				return nil, fmt.Errorf("cross-protocol images require an image URL or data URL")
			}
			out = append(out, part{Kind: "image", URL: u, Detail: detail})
		case "image":
			source := obj(b["source"])
			u := str(source["url"])
			if str(source["type"]) == "base64" {
				u = "data:" + str(source["media_type"]) + ";base64," + str(source["data"])
			}
			if u == "" {
				return nil, fmt.Errorf("image source is missing")
			}
			out = append(out, part{Kind: "image", URL: u})
		case "tool_use":
			out = append(out, part{Kind: "tool", ID: str(b["id"]), Name: str(b["name"]), Arguments: jsonText(b["input"])})
		case "tool_result":
			content, err := parts(b["content"])
			if err != nil {
				return nil, err
			}
			failed, _ := b["is_error"].(bool)
			out = append(out, part{Kind: "result", ID: str(b["tool_use_id"]), Content: content, IsError: failed})
		case "thinking":
			out = append(out, part{Kind: "thinking", Text: str(b["thinking"]), Signature: str(b["signature"])})
		default:
			return nil, fmt.Errorf("content type %q cannot be converted between these protocols", typ)
		}
	}
	return out, nil
}

func textParts(p []part) string {
	var b strings.Builder
	for _, p := range p {
		if p.Kind == "text" || p.Kind == "refusal" {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

func contentParts(p []part, target model.Protocol, role string) ([]any, error) {
	out := []any{}
	for _, b := range p {
		switch b.Kind {
		case "text", "refusal":
			typ := "text"
			if target == model.ProtocolOpenAIResponses {
				typ = "input_text"
				if role == "assistant" {
					typ = "output_text"
				}
			}
			out = append(out, object{"type": typ, "text": b.Text})
		case "image":
			switch target {
			case model.ProtocolOpenAIChat:
				i := object{"url": b.URL}
				if b.Detail != "" {
					i["detail"] = b.Detail
				}
				out = append(out, object{"type": "image_url", "image_url": i})
			case model.ProtocolOpenAIResponses:
				i := object{"type": "input_image", "image_url": b.URL}
				if b.Detail != "" {
					i["detail"] = b.Detail
				}
				out = append(out, i)
			default:
				source := object{"type": "url", "url": b.URL}
				if strings.HasPrefix(b.URL, "data:") {
					head, data, ok := strings.Cut(strings.TrimPrefix(b.URL, "data:"), ";base64,")
					if !ok {
						return nil, fmt.Errorf("Anthropic images require base64 data URLs")
					}
					source = object{"type": "base64", "media_type": head, "data": data}
				}
				out = append(out, object{"type": "image", "source": source})
			}
		default:
			return nil, fmt.Errorf("content type %q is not valid in a plain message", b.Kind)
		}
	}
	return out, nil
}

func toolInput(arguments string) (any, error) {
	if arguments == "" {
		arguments = "{}"
	}
	var input object
	if err := json.Unmarshal([]byte(arguments), &input); err != nil || input == nil {
		return nil, fmt.Errorf("tool arguments must be a JSON object")
	}
	return input, nil
}

func ReadUsage(body object) model.Usage {
	if r := obj(body["response"]); r != nil {
		body = r
	}
	if r := obj(body["message"]); r != nil {
		body = r
	}
	u := obj(body["usage"])
	n := func(keys ...string) int {
		for _, k := range keys {
			if v, ok := u[k]; ok {
				return integer(v)
			}
		}
		return 0
	}
	result := model.Usage{InputTokens: n("prompt_tokens", "input_tokens"), OutputTokens: n("completion_tokens", "output_tokens"), CachedTokens: n("cache_read_input_tokens", "cached_tokens"), CacheCreationTokens: n("cache_creation_input_tokens")}
	for _, k := range []string{"prompt_tokens_details", "input_tokens_details"} {
		if v := integer(obj(u[k])["cached_tokens"]); v > result.CachedTokens {
			result.CachedTokens = v
		}
	}
	for _, k := range []string{"completion_tokens_details", "output_tokens_details"} {
		if v := integer(obj(u[k])["reasoning_tokens"]); v > result.ReasoningTokens {
			result.ReasoningTokens = v
		}
	}
	return result
}

func Usage(target model.Protocol, u model.Usage) object {
	switch target {
	case model.ProtocolOpenAIChat:
		return object{"prompt_tokens": u.InputTokens, "completion_tokens": u.OutputTokens, "total_tokens": u.InputTokens + u.OutputTokens, "prompt_tokens_details": object{"cached_tokens": u.CachedTokens}, "completion_tokens_details": object{"reasoning_tokens": u.ReasoningTokens}}
	case model.ProtocolOpenAIResponses:
		return object{"input_tokens": u.InputTokens, "output_tokens": u.OutputTokens, "total_tokens": u.InputTokens + u.OutputTokens, "input_tokens_details": object{"cached_tokens": u.CachedTokens}, "output_tokens_details": object{"reasoning_tokens": u.ReasoningTokens}}
	default:
		return object{"input_tokens": max(0, u.InputTokens-u.CachedTokens-u.CacheCreationTokens), "output_tokens": u.OutputTokens, "cache_read_input_tokens": u.CachedTokens, "cache_creation_input_tokens": u.CacheCreationTokens}
	}
}

func TotalUsage(source model.Protocol, body object) model.Usage {
	u := ReadUsage(body)
	if source == model.ProtocolAnthropic {
		u.InputTokens += u.CachedTokens + u.CacheCreationTokens
	}
	return u
}
