package server

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/omniapi/omni-api/internal/upstream"
)

func writeConvertedStream(w http.ResponseWriter, kind string, source *upstream.Stream, requestID string, started time.Time) (int, int) {
	defer source.Close()
	w.Header().Set("content-type", "text/event-stream; charset=utf-8")
	w.Header().Set("cache-control", "no-cache")
	w.Header().Set("x-accel-buffering", "no")
	w.Header().Set("x-request-id", requestID)
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	flush := func() {
		if flusher != nil {
			flusher.Flush()
		}
	}
	firstToken, seen := 0, false
	for {
		frame, ok := source.NextFrame()
		if !ok {
			break
		}
		if !seen && isContentFrame(frame.Data) {
			firstToken = elapsedMs(started)
			seen = true
		}
		if _, err := w.Write(frame.Bytes()); err != nil {
			source.SetError(&upstream.Failure{Status: 499, Message: "client response write failed", Cause: err})
			return firstToken, elapsedMs(started)
		}
		flush()
	}
	if err := source.Err(); err != nil {
		errorBody := map[string]any{"type": "upstream_error", "code": "upstream_stream_error", "message": err.Error()}
		if kind == kindResponses {
			writeEvent(w, "response.failed", map[string]any{"type": "response.failed", "response": map[string]any{"id": "resp_" + requestID, "object": "response", "status": "failed", "error": errorBody}})
		} else {
			writeEvent(w, "error", map[string]any{"type": "error", "error": errorBody})
		}
		flush()
	}
	return firstToken, elapsedMs(started)
}

func isContentFrame(data string) bool {
	var b map[string]any
	if json.Unmarshal([]byte(data), &b) != nil {
		return false
	}
	switch b["type"] {
	case "response.output_text.delta", "response.function_call_arguments.delta", "response.reasoning_summary_text.delta", "content_block_delta":
		return true
	case "content_block_start":
		p, _ := b["content_block"].(map[string]any)
		return p["type"] == "tool_use"
	}
	if choices, ok := b["choices"].([]any); ok {
		for _, v := range choices {
			c, _ := v.(map[string]any)
			d, _ := c["delta"].(map[string]any)
			if d["content"] != nil || d["tool_calls"] != nil || d["reasoning_content"] != nil {
				return true
			}
		}
	}
	return false
}
