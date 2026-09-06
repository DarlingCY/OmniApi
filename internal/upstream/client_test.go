package upstream

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/omniapi/omni-api/internal/model"
)

func anthropicProvider(baseURL string) model.Provider {
	return model.Provider{
		ID:       "anthropic",
		Name:     "Anthropic",
		BaseURL:  baseURL + "/v1",
		Protocol: model.ProtocolAnthropic,
		APIKey:   "secret",
		Enabled:  true,
		Models:   []model.ProviderModel{{ID: "claude", UpstreamModel: "claude-test"}},
	}
}

func TestAnthropicPathAndHeaders(t *testing.T) {
	captured := struct {
		path    string
		headers http.Header
		body    map[string]any
	}{}
	upstreamServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		captured.path = request.URL.Path
		captured.headers = request.Header.Clone()
		raw, _ := io.ReadAll(request.Body)
		_ = json.Unmarshal(raw, &captured.body)
		writer.Header().Set("content-type", "application/json")
		_, _ = writer.Write([]byte(`{"content":[{"type":"text","text":"ok"}]}`))
	}))
	defer upstreamServer.Close()

	provider := anthropicProvider(upstreamServer.URL)
	providerModel := provider.Models[0]
	request := model.Request{Model: "omni", Messages: []model.Message{{Role: "user", Content: "hello"}}}

	result, err := NewClient(5*time.Second).Call(context.Background(), provider, providerModel, request)
	if err != nil {
		t.Fatalf("call failed: %v", err)
	}
	if Text(result.Body) != "ok" {
		t.Fatalf("expected assistant text ok, got %q", Text(result.Body))
	}
	if captured.path != "/v1/messages" {
		t.Fatalf("expected /v1/messages, got %s", captured.path)
	}
	if captured.headers.Get("x-api-key") != "secret" {
		t.Fatalf("expected x-api-key header, got %q", captured.headers.Get("x-api-key"))
	}
	if captured.headers.Get("anthropic-version") != "2023-06-01" {
		t.Fatalf("expected anthropic-version header, got %q", captured.headers.Get("anthropic-version"))
	}
	if captured.headers.Get("authorization") != "" {
		t.Fatalf("expected no authorization header, got %q", captured.headers.Get("authorization"))
	}
	if captured.body["model"] != "claude-test" {
		t.Fatalf("expected upstream model claude-test, got %v", captured.body["model"])
	}
	if captured.body["max_tokens"] != float64(4096) {
		t.Fatalf("expected default max_tokens 4096, got %v", captured.body["max_tokens"])
	}
}

func TestEmptyStreamFailsBeforeFirstEvent(t *testing.T) {
	upstreamServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("content-type", "text/event-stream")
		writer.WriteHeader(http.StatusOK)
	}))
	defer upstreamServer.Close()

	provider := anthropicProvider(upstreamServer.URL)
	providerModel := provider.Models[0]
	request := model.Request{Model: "omni", Messages: []model.Message{{Role: "user", Content: "hello"}}, Stream: true}

	_, err := NewClient(5*time.Second).Call(context.Background(), provider, providerModel, request)
	failure, ok := err.(*Failure)
	if !ok {
		t.Fatalf("expected an upstream failure, got %v", err)
	}
	if failure.Status != 502 || failure.Message != "upstream stream ended before the first event" {
		t.Fatalf("unexpected failure: %d %s", failure.Status, failure.Message)
	}
}

func TestUsageParsesCacheCreationAndReasoningTokens(t *testing.T) {
	cases := []struct {
		name string
		body string
		want model.Usage
	}{
		{"anthropic", `{"usage":{"input_tokens":120,"output_tokens":20,"cache_creation_input_tokens":30,"cache_read_input_tokens":40}}`, model.Usage{InputTokens: 120, OutputTokens: 20, CachedTokens: 40, CacheCreationTokens: 30}},
		{"openai", `{"usage":{"prompt_tokens":120,"completion_tokens":20,"completion_tokens_details":{"reasoning_tokens":9},"output_tokens_details":{"reasoning_tokens":12}}}`, model.Usage{InputTokens: 120, OutputTokens: 20, ReasoningTokens: 12}},
		{"gemini", `{"usageMetadata":{"promptTokenCount":120,"candidatesTokenCount":20,"totalTokenCount":140,"cachedContentTokenCount":40,"thoughtsTokenCount":9}}`, model.Usage{InputTokens: 120, OutputTokens: 20, CachedTokens: 40, ReasoningTokens: 9}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			var body map[string]any
			if err := json.Unmarshal([]byte(testCase.body), &body); err != nil {
				t.Fatal(err)
			}
			if got := usage(body); got != testCase.want {
				t.Fatalf("usage mismatch: got %#v want %#v", got, testCase.want)
			}
		})
	}
}

func TestStreamUsageKeepsHighestDetailedCounters(t *testing.T) {
	var got model.Usage
	got.Merge(streamUsage(`{"usage":{"input_tokens":100,"output_tokens":10,"cache_creation_input_tokens":20,"completion_tokens_details":{"reasoning_tokens":3}}}`))
	got.Merge(streamUsage(`{"usage":{"input_tokens":90,"output_tokens":12,"cache_creation_input_tokens":15,"output_tokens_details":{"reasoning_tokens":5}}}`))
	want := model.Usage{InputTokens: 100, OutputTokens: 12, CacheCreationTokens: 20, ReasoningTokens: 5}
	if got != want {
		t.Fatalf("stream usage mismatch: got %#v want %#v", got, want)
	}
}

func TestStreamDecodesDeltasPerProtocol(t *testing.T) {
	cases := []struct {
		protocol model.Protocol
		payload  string
	}{
		{model.ProtocolOpenAIChat, "data: {\"choices\":[{\"delta\":{\"content\":\"he\"}}]}\n\ndata: {\"choices\":[{\"delta\":{\"content\":\"llo\"}}]}\n\ndata: [DONE]\n\n"},
		{model.ProtocolOpenAIResponses, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"he\"}\n\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"llo\"}\n\n"},
		{model.ProtocolAnthropic, "data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"he\"}}\r\n\r\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"llo\"}}\r\n\r\n"},
	}
	for _, testCase := range cases {
		upstreamServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			writer.Header().Set("content-type", "text/event-stream")
			_, _ = writer.Write([]byte(testCase.payload))
		}))
		provider := anthropicProvider(upstreamServer.URL)
		provider.Protocol = testCase.protocol
		providerModel := provider.Models[0]
		request := model.Request{Model: "omni", Messages: []model.Message{{Role: "user", Content: "hi"}}, Stream: true}

		result, err := NewClient(5*time.Second).Call(context.Background(), provider, providerModel, request)
		if err != nil {
			upstreamServer.Close()
			t.Fatalf("%s stream failed: %v", testCase.protocol, err)
		}
		collected := ""
		for {
			delta, ok := result.Stream.Next()
			if !ok {
				break
			}
			collected += delta
		}
		result.Stream.Close()
		upstreamServer.Close()
		if collected != "hello" {
			t.Fatalf("%s expected hello, got %q", testCase.protocol, collected)
		}
	}
}

func TestDiscoverModelsNormalizesResults(t *testing.T) {
	captured := struct {
		path    string
		headers http.Header
	}{}
	upstreamServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		captured.path = request.URL.Path
		captured.headers = request.Header.Clone()
		writer.Header().Set("content-type", "application/json")
		_, _ = writer.Write([]byte(`{"data":[{"id":"zeta"},{"id":"alpha"},{"id":"zeta"},{"id":""},{}]}`))
	}))
	defer upstreamServer.Close()

	models, err := NewClient(5*time.Second).DiscoverModels(context.Background(), model.ProtocolOpenAIChat, upstreamServer.URL+"/v1", "secret")
	if err != nil {
		t.Fatalf("discovery failed: %v", err)
	}
	if len(models) != 2 || models[0] != "alpha" || models[1] != "zeta" {
		t.Fatalf("expected [alpha zeta], got %v", models)
	}
	if captured.path != "/v1/models" {
		t.Fatalf("expected /v1/models, got %s", captured.path)
	}
	if captured.headers.Get("authorization") != "Bearer secret" {
		t.Fatalf("expected bearer auth, got %q", captured.headers.Get("authorization"))
	}
}

func TestDiscoverModelsRelaysUpstreamError(t *testing.T) {
	upstreamServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("content-type", "application/json")
		writer.WriteHeader(http.StatusForbidden)
		_, _ = writer.Write([]byte(`{"error":{"message":"Model listing is disabled"}}`))
	}))
	defer upstreamServer.Close()

	_, err := NewClient(5*time.Second).DiscoverModels(context.Background(), model.ProtocolAnthropic, upstreamServer.URL+"/v1", "secret")
	failure, ok := err.(*Failure)
	if !ok {
		t.Fatalf("expected an upstream failure, got %v", err)
	}
	if failure.Status != 403 || failure.Message != "Model listing is disabled" {
		t.Fatalf("unexpected failure: %d %s", failure.Status, failure.Message)
	}
}
