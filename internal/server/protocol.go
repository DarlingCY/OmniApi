package server

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/omniapi/omni-api/internal/model"
	"github.com/omniapi/omni-api/internal/upstream"
)

const (
	kindChat      = "chat"
	kindResponses = "responses"
	kindMessages  = "messages"
)

func newRequestID() string {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "00000000000000000000000000000000"
	}
	return hex.EncodeToString(raw)
}

func stringField(body map[string]any, key string) string {
	value, _ := body[key].(string)
	return value
}

func intField(body map[string]any, key string) int {
	value, ok := body[key].(float64)
	if !ok {
		return 0
	}
	return int(value)
}

func floatField(body map[string]any, key string) *float64 {
	value, ok := body[key].(float64)
	if !ok {
		return nil
	}
	return &value
}

func toolsField(body map[string]any) []any {
	tools, _ := body["tools"].([]any)
	return tools
}

func messages(value any) []model.Message {
	entries, ok := value.([]any)
	if !ok {
		return nil
	}
	converted := make([]model.Message, 0, len(entries))
	for _, entry := range entries {
		record, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		role, _ := record["role"].(string)
		switch role {
		case "assistant", "system", "tool":
		default:
			role = "user"
		}
		content, isText := record["content"].(string)
		if !isText {
			raw, err := json.Marshal(record["content"])
			if err == nil && string(raw) != "null" {
				content = string(raw)
			}
		}
		toolCallID, _ := record["tool_call_id"].(string)
		name, _ := record["name"].(string)
		converted = append(converted, model.Message{Role: role, Content: content, ToolCallID: toolCallID, Name: name})
	}
	return converted
}

func responseMessages(value any) []model.Message {
	if text, ok := value.(string); ok {
		return []model.Message{{Role: "user", Content: text}}
	}
	return messages(value)
}

func normalize(kind string, body map[string]any) model.Request {
	stream, _ := body["stream"].(bool)
	request := model.Request{
		Raw:         body,
		Model:       stringField(body, "model"),
		Tools:       toolsField(body),
		Temperature: floatField(body, "temperature"),
		TopP:        floatField(body, "top_p"),
		Stream:      stream,
	}
	switch kind {
	case kindResponses:
		request.Protocol = model.ProtocolOpenAIResponses
		request.System = stringField(body, "instructions")
		request.Messages = responseMessages(body["input"])
		request.MaxOutputTokens = intField(body, "max_output_tokens")
	case kindMessages:
		request.Protocol = model.ProtocolAnthropic
		request.System = stringField(body, "system")
		request.Messages = messages(body["messages"])
		request.MaxOutputTokens = intField(body, "max_tokens")
	default:
		request.Protocol = model.ProtocolOpenAIChat
		source := messages(body["messages"])
		filtered := make([]model.Message, 0, len(source))
		for _, message := range source {
			if message.Role == "system" {
				if request.System == "" {
					request.System = message.Content
				}
				continue
			}
			filtered = append(filtered, message)
		}
		request.Messages = filtered
		request.MaxOutputTokens = intField(body, "max_tokens")
	}
	return request
}

func responseUsage(usage model.Usage) map[string]any {
	return map[string]any{
		"input_tokens":  usage.InputTokens,
		"output_tokens": usage.OutputTokens,
		"total_tokens":  usage.InputTokens + usage.OutputTokens,
		"input_tokens_details": map[string]any{
			"cached_tokens": usage.CachedTokens,
		},
		"output_tokens_details": map[string]any{
			"reasoning_tokens": usage.ReasoningTokens,
		},
	}
}

func messageUsage(usage model.Usage) map[string]any {
	return map[string]any{
		"input_tokens":                usage.InputTokens,
		"output_tokens":               usage.OutputTokens,
		"cache_creation_input_tokens": usage.CacheCreationTokens,
		"cache_read_input_tokens":     usage.CachedTokens,
	}
}

func chatUsage(usage model.Usage) map[string]any {
	return map[string]any{
		"prompt_tokens":     usage.InputTokens,
		"completion_tokens": usage.OutputTokens,
		"total_tokens":      usage.InputTokens + usage.OutputTokens,
		"prompt_tokens_details": map[string]any{
			"cached_tokens": usage.CachedTokens,
		},
		"completion_tokens_details": map[string]any{
			"reasoning_tokens": usage.ReasoningTokens,
		},
	}
}

func valueOrDefault(value *float64, fallback float64) float64 {
	if value != nil {
		return *value
	}
	return fallback
}

func responsesObject(requestID, modelName, content string, usage model.Usage, request model.Request, createdAt time.Time) map[string]any {
	responseID := "resp_" + requestID
	tools := request.Tools
	if tools == nil {
		tools = []any{}
	}
	return map[string]any{
		"id":         responseID,
		"object":     "response",
		"created_at": createdAt.Unix(),
		"model":      modelName,
		"status":     "completed",
		"background": false,
		"output": []any{map[string]any{
			"id": responseID + "_msg", "type": "message", "status": "completed", "role": "assistant",
			"content": []any{map[string]any{"type": "output_text", "text": content, "annotations": []any{}}},
		}},
		"output_text":          content,
		"usage":                responseUsage(usage),
		"error":                nil,
		"incomplete_details":   nil,
		"instructions":         request.System,
		"metadata":             map[string]any{},
		"tools":                tools,
		"tool_choice":          "auto",
		"temperature":          valueOrDefault(request.Temperature, 1),
		"top_p":                valueOrDefault(request.TopP, 1),
		"text":                 map[string]any{"format": map[string]any{"type": "text"}},
		"reasoning":            map[string]any{"effort": nil, "summary": nil},
		"max_output_tokens":    nil,
		"parallel_tool_calls":  true,
		"previous_response_id": nil,
		"conversation":         nil,
		"store":                false,
		"service_tier":         "standard",
		"safety_identifier":    nil,
		"truncation":           "disabled",
	}
}

func encode(kind, modelName string, result *upstream.Result, requestID string, startedAt time.Time, request model.Request) any {
	if result.Converted {
		return result.Body
	}
	content := upstream.Text(result.Body)
	switch kind {
	case kindResponses:
		return responsesObject(requestID, modelName, content, result.Usage, request, startedAt)
	case kindMessages:
		return map[string]any{
			"id":            "msg_" + requestID,
			"type":          "message",
			"role":          "assistant",
			"model":         modelName,
			"content":       []any{map[string]any{"type": "text", "text": content}},
			"stop_reason":   "end_turn",
			"stop_sequence": nil,
			"usage":         messageUsage(result.Usage),
		}
	default:
		return map[string]any{
			"id":      "chatcmpl_" + requestID,
			"object":  "chat.completion",
			"created": startedAt.Unix(),
			"model":   modelName,
			"choices": []any{map[string]any{
				"index":         0,
				"message":       map[string]any{"role": "assistant", "content": content},
				"finish_reason": "stop",
			}},
			"usage": chatUsage(result.Usage),
		}
	}
}

func writeStream(writer http.ResponseWriter, kind, modelName string, source *upstream.Stream, requestID string, startedAt time.Time, request model.Request) (int, int) {
	if source.Converted() {
		return writeConvertedStream(writer, kind, source, requestID, startedAt)
	}
	defer source.Close()
	header := writer.Header()
	header.Set("content-type", "text/event-stream; charset=utf-8")
	header.Set("cache-control", "no-cache")
	header.Set("connection", "keep-alive")
	header.Set("x-request-id", requestID)
	writer.WriteHeader(http.StatusOK)
	flusher, _ := writer.(http.Flusher)
	flush := func() {
		if flusher != nil {
			flusher.Flush()
		}
	}

	responseID := "resp_" + requestID
	messageID := responseID + "_msg"
	sequenceNumber := 0
	nextSequence := func() int {
		sequenceNumber++
		return sequenceNumber
	}
	if kind == kindResponses {
		writeEvent(writer, "response.created", map[string]any{
			"type": "response.created", "sequence_number": nextSequence(),
			"response": map[string]any{
				"id": responseID, "object": "response", "created_at": startedAt.Unix(),
				"status": "in_progress", "model": modelName, "output": []any{}, "error": nil,
			},
		})
		writeEvent(writer, "response.in_progress", map[string]any{
			"type": "response.in_progress", "sequence_number": nextSequence(),
			"response": map[string]any{
				"id": responseID, "object": "response", "created_at": startedAt.Unix(),
				"status": "in_progress", "model": modelName, "output": []any{},
			},
		})
		writeEvent(writer, "response.output_item.added", map[string]any{
			"type": "response.output_item.added", "sequence_number": nextSequence(), "output_index": 0,
			"item": map[string]any{"id": messageID, "type": "message", "status": "in_progress", "role": "assistant", "content": []any{}},
		})
		writeEvent(writer, "response.content_part.added", map[string]any{
			"type": "response.content_part.added", "sequence_number": nextSequence(), "item_id": messageID,
			"output_index": 0, "content_index": 0,
			"part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}},
		})
		flush()
	} else if kind == kindMessages {
		writeEvent(writer, "message_start", map[string]any{
			"type": "message_start",
			"message": map[string]any{
				"id": "msg_" + requestID, "type": "message", "role": "assistant",
				"model": modelName, "content": []any{}, "stop_reason": nil, "stop_sequence": nil,
				"usage": messageUsage(model.Usage{}),
			},
		})
		writeEvent(writer, "content_block_start", map[string]any{
			"type": "content_block_start", "index": 0,
			"content_block": map[string]any{"type": "text", "text": ""},
		})
		flush()
	}

	firstTokenMs := 0
	content := strings.Builder{}
	for {
		delta, ok := source.Next()
		if !ok {
			break
		}
		if firstTokenMs == 0 {
			firstTokenMs = int(time.Since(startedAt).Milliseconds())
		}
		content.WriteString(delta)
		switch kind {
		case kindResponses:
			writeEvent(writer, "response.output_text.delta", map[string]any{
				"type": "response.output_text.delta", "sequence_number": nextSequence(), "item_id": messageID,
				"output_index": 0, "content_index": 0, "delta": delta,
			})
		case kindMessages:
			writeEvent(writer, "content_block_delta", map[string]any{
				"type": "content_block_delta", "index": 0,
				"delta": map[string]any{"type": "text_delta", "text": delta},
			})
		default:
			writeEvent(writer, "", map[string]any{
				"id": "chatcmpl_" + requestID, "object": "chat.completion.chunk",
				"created": startedAt.Unix(), "model": modelName,
				"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"role": "assistant", "content": delta}, "finish_reason": nil}},
			})
		}
		flush()
	}

	if err := source.Err(); err != nil {
		errorBody := map[string]any{"type": "upstream_error", "code": "upstream_stream_error", "message": err.Error()}
		if kind == kindResponses {
			writeEvent(writer, "response.failed", map[string]any{
				"type": "response.failed", "sequence_number": nextSequence(),
				"response": map[string]any{"id": responseID, "object": "response", "status": "failed", "error": errorBody},
			})
		} else {
			writeEvent(writer, "error", map[string]any{"type": "error", "error": errorBody})
		}
		flush()
		return firstTokenMs, int(time.Since(startedAt).Milliseconds())
	}
	switch kind {
	case kindResponses:
		usage := source.Usage()
		writeEvent(writer, "response.output_text.done", map[string]any{
			"type": "response.output_text.done", "sequence_number": nextSequence(), "item_id": messageID,
			"output_index": 0, "content_index": 0, "text": content.String(),
		})
		writeEvent(writer, "response.content_part.done", map[string]any{
			"type": "response.content_part.done", "sequence_number": nextSequence(), "item_id": messageID,
			"output_index": 0, "content_index": 0,
			"part": map[string]any{"type": "output_text", "text": content.String(), "annotations": []any{}},
		})
		writeEvent(writer, "response.output_item.done", map[string]any{
			"type": "response.output_item.done", "sequence_number": nextSequence(), "output_index": 0,
			"item": map[string]any{
				"id": messageID, "type": "message", "status": "completed", "role": "assistant",
				"content": []any{map[string]any{"type": "output_text", "text": content.String(), "annotations": []any{}}},
			},
		})
		writeEvent(writer, "response.completed", map[string]any{
			"type": "response.completed", "sequence_number": nextSequence(),
			"response": responsesObject(requestID, modelName, content.String(), usage, request, startedAt),
		})
	case kindMessages:
		usage := source.Usage()
		writeEvent(writer, "content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
		writeEvent(writer, "message_delta", map[string]any{
			"type": "message_delta", "delta": map[string]any{"stop_reason": "end_turn", "stop_sequence": nil},
			"usage": messageUsage(usage),
		})
		writeEvent(writer, "message_stop", map[string]any{"type": "message_stop"})
	default:
		writeEvent(writer, "", map[string]any{
			"id": "chatcmpl_" + requestID, "object": "chat.completion.chunk",
			"created": startedAt.Unix(), "model": modelName,
			"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}},
			"usage":   chatUsage(source.Usage()),
		})
		fmt.Fprint(writer, "data: [DONE]\n\n")
	}
	flush()
	return firstTokenMs, int(time.Since(startedAt).Milliseconds())
}

func writeEvent(writer http.ResponseWriter, name string, payload any) {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return
	}
	builder := strings.Builder{}
	if name != "" {
		builder.WriteString("event: ")
		builder.WriteString(name)
		builder.WriteString("\n")
	}
	builder.WriteString("data: ")
	builder.Write(encoded)
	builder.WriteString("\n\n")
	fmt.Fprint(writer, builder.String())
}
