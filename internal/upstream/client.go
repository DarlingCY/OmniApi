package upstream

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/omniapi/omni-api/internal/model"
	"github.com/omniapi/omni-api/internal/translate"
)

// Failure carries an upstream error together with its public HTTP status.
type Failure struct {
	Status  int
	Message string
	Cause   error
}

// Error implements error.
func (f *Failure) Error() string { return f.Message }

func (f *Failure) Unwrap() error { return f.Cause }

// Diagnostic preserves network failure details without the request URL, which
// may contain credentials. Public HTTP errors still use Failure.Message.
func Diagnostic(err error) string {
	var failure *Failure
	if errors.As(err, &failure) && failure.Cause != nil {
		return failure.Message + ": " + Diagnostic(failure.Cause)
	}
	var urlError *url.Error
	if errors.As(err, &urlError) {
		return urlError.Op + ": " + urlError.Err.Error()
	}
	return err.Error()
}

// Result is a completed upstream call, either buffered or streaming.
type Result struct {
	Converted bool
	Status    int
	Body      map[string]any
	Stream    *Stream
	Usage     model.Usage
}

// Client performs upstream provider calls.
type Client struct {
	httpClient *http.Client
}

// NewClient builds a client with a per-request timeout.
func NewClient(timeout time.Duration) *Client {
	return &Client{httpClient: &http.Client{Timeout: timeout}}
}

func endpoint(baseURL string, protocol model.Protocol, suffix string) (string, error) {
	trimmed := strings.TrimRight(baseURL, "/")
	if trimmed == "" {
		return "", &Failure{Status: 400, Message: "provider base URL is empty"}
	}
	parsed, err := url.Parse(trimmed)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return "", &Failure{Status: 400, Message: "provider base URL must be an http or https URL"}
	}
	return trimmed + "/" + suffix, nil
}

func applyAuth(header http.Header, protocol model.Protocol, apiKey string) {
	header.Set("content-type", "application/json")
	if apiKey == "" {
		return
	}
	if protocol == model.ProtocolAnthropic {
		header.Set("x-api-key", apiKey)
		header.Set("anthropic-version", "2023-06-01")
		return
	}
	header.Set("authorization", "Bearer "+apiKey)
}

func chatSuffix(protocol model.Protocol) string {
	switch protocol {
	case model.ProtocolOpenAIChat:
		return "chat/completions"
	case model.ProtocolOpenAIResponses:
		return "responses"
	default:
		return "messages"
	}
}

func upstreamMessages(request model.Request) []model.Message {
	if request.System == "" {
		return request.Messages
	}
	return append([]model.Message{{Role: "system", Content: request.System}}, request.Messages...)
}

func payload(provider model.Provider, upstreamModel string, request model.Request) map[string]any {
	body := map[string]any{"model": upstreamModel, "stream": request.Stream}
	if request.Tools != nil {
		body["tools"] = request.Tools
	}
	if request.Temperature != nil {
		body["temperature"] = *request.Temperature
	}
	if request.TopP != nil {
		body["top_p"] = *request.TopP
	}
	switch provider.Protocol {
	case model.ProtocolOpenAIChat:
		body["messages"] = upstreamMessages(request)
		if request.MaxOutputTokens > 0 {
			body["max_tokens"] = request.MaxOutputTokens
		}
	case model.ProtocolOpenAIResponses:
		body["input"] = upstreamMessages(request)
		if request.MaxOutputTokens > 0 {
			body["max_output_tokens"] = request.MaxOutputTokens
		}
	default:
		if request.System != "" {
			body["system"] = request.System
		}
		filtered := make([]model.Message, 0, len(request.Messages))
		for _, message := range request.Messages {
			if message.Role != "system" {
				filtered = append(filtered, message)
			}
		}
		body["messages"] = filtered
		if request.MaxOutputTokens > 0 {
			body["max_tokens"] = request.MaxOutputTokens
		} else {
			body["max_tokens"] = 4096
		}
	}
	return body
}

func publicError(body []byte, status int, fallback string) string {
	var envelope struct {
		Error json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(body, &envelope); err == nil && len(envelope.Error) > 0 {
		var text string
		if json.Unmarshal(envelope.Error, &text) == nil && text != "" {
			return text
		}
		var detail struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(envelope.Error, &detail) == nil && detail.Message != "" {
			return detail.Message
		}
	}
	if fallback != "" {
		return fallback
	}
	return fmt.Sprintf("upstream returned %d", status)
}

// Call sends a chat request to a provider and adapts the response shape.
func (c *Client) Call(ctx context.Context, provider model.Provider, providerModel model.ProviderModel, request model.Request) (*Result, error) {
	return c.call(ctx, provider, providerModel, request, nil)
}

// CallWithResponse is like Call, and invokes onResponse after upstream headers arrive.
func (c *Client) CallWithResponse(ctx context.Context, provider model.Provider, providerModel model.ProviderModel, request model.Request, onResponse func(*http.Response)) (*Result, error) {
	return c.call(ctx, provider, providerModel, request, onResponse)
}

func (c *Client) call(ctx context.Context, provider model.Provider, providerModel model.ProviderModel, request model.Request, onResponse func(*http.Response)) (*Result, error) {
	upstreamModel := strings.TrimSpace(providerModel.UpstreamModel)
	if upstreamModel == "" {
		return nil, &Failure{Status: 400, Message: "provider model has no upstream model name"}
	}
	target, err := endpoint(provider.BaseURL, provider.Protocol, chatSuffix(provider.Protocol))
	if err != nil {
		return nil, err
	}
	bodyToSend := payload(provider, upstreamModel, request)
	if request.Raw != nil {
		bodyToSend, err = translate.Request(request.Protocol, provider.Protocol, request.Raw, upstreamModel)
		if err != nil {
			return nil, &Failure{Status: 400, Message: "protocol conversion: " + err.Error()}
		}
	}
	encoded, err := json.Marshal(bodyToSend)
	if err != nil {
		return nil, &Failure{Status: 500, Message: "request could not be encoded"}
	}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(encoded))
	if err != nil {
		return nil, &Failure{Status: 502, Message: "upstream request could not be created"}
	}
	applyAuth(httpRequest.Header, provider.Protocol, provider.APIKey)
	if request.Stream {
		httpRequest.Header.Set("accept", "text/event-stream")
	}
	response, err := c.httpClient.Do(httpRequest)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return nil, &Failure{Status: 499, Message: "client closed the request", Cause: err}
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, &Failure{Status: 504, Message: "upstream request timed out", Cause: err}
		}
		return nil, &Failure{Status: 502, Message: "upstream request failed", Cause: err}
	}
	if onResponse != nil {
		onResponse(response)
	}
	if response.StatusCode < 200 || response.StatusCode > 299 {
		defer response.Body.Close()
		raw, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
		return nil, &Failure{Status: response.StatusCode, Message: publicError(raw, response.StatusCode, response.Status)}
	}
	if request.Stream {
		if request.Raw != nil {
			stream, err := newConvertedStream(provider.Protocol, request, response)
			if err != nil {
				return nil, err
			}
			return &Result{Status: response.StatusCode, Stream: stream, Converted: true}, nil
		}
		stream, err := newStream(provider.Protocol, response)
		if err != nil {
			return nil, err
		}
		return &Result{Status: response.StatusCode, Stream: stream}, nil
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, 8<<20))
	if err != nil {
		return nil, &Failure{Status: 502, Message: "upstream response could not be read", Cause: err}
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, &Failure{Status: 502, Message: "upstream response was not valid JSON"}
	}
	if request.Raw != nil {
		resultUsage := translate.TotalUsage(provider.Protocol, body)
		converted, err := translate.Response(provider.Protocol, request.Protocol, body, request.Model, request.RequestID, request.Raw)
		if err != nil {
			return nil, &Failure{Status: 502, Message: "upstream protocol conversion: " + err.Error()}
		}
		return &Result{Status: response.StatusCode, Body: converted, Usage: resultUsage, Converted: true}, nil
	}
	return &Result{Status: response.StatusCode, Body: body, Usage: usage(body)}, nil
}

func usage(body map[string]any) model.Usage {
	record := body
	if response, ok := body["response"].(map[string]any); ok {
		record = response
	}
	usageBody, ok := record["usage"].(map[string]any)
	if !ok {
		usageBody, ok = record["usageMetadata"].(map[string]any)
		if !ok {
			return model.Usage{}
		}
	}
	input := number(usageBody, "prompt_tokens", "input_tokens", "promptTokenCount")
	output := number(usageBody, "completion_tokens", "output_tokens", "candidatesTokenCount")
	cached := number(usageBody, "cache_read_input_tokens", "cached_tokens", "cachedContentTokenCount")
	cacheCreation := number(usageBody, "cache_creation_input_tokens")
	reasoning := number(usageBody, "thoughtsTokenCount")
	if details, ok := usageBody["prompt_tokens_details"].(map[string]any); ok {
		if value := number(details, "cached_tokens"); value > cached {
			cached = value
		}
	}
	if details, ok := usageBody["input_tokens_details"].(map[string]any); ok {
		if value := number(details, "cached_tokens"); value > cached {
			cached = value
		}
	}
	if details, ok := usageBody["completion_tokens_details"].(map[string]any); ok {
		if value := number(details, "reasoning_tokens"); value > reasoning {
			reasoning = value
		}
	}
	if details, ok := usageBody["output_tokens_details"].(map[string]any); ok {
		if value := number(details, "reasoning_tokens"); value > reasoning {
			reasoning = value
		}
	}
	return model.Usage{InputTokens: input, OutputTokens: output, CachedTokens: cached, CacheCreationTokens: cacheCreation, ReasoningTokens: reasoning}
}

func number(body map[string]any, keys ...string) int {
	for _, key := range keys {
		if value, ok := body[key].(float64); ok {
			return int(value)
		}
	}
	return 0
}

// DiscoverModels lists the models a provider exposes at its models endpoint.
func (c *Client) DiscoverModels(ctx context.Context, protocol model.Protocol, baseURL, apiKey string) ([]string, error) {
	target, err := endpoint(baseURL, protocol, "models")
	if err != nil {
		return nil, err
	}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, &Failure{Status: 502, Message: "model discovery request could not be created"}
	}
	applyAuth(httpRequest.Header, protocol, apiKey)
	response, err := c.httpClient.Do(httpRequest)
	if err != nil {
		return nil, &Failure{Status: 502, Message: "model discovery request failed"}
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	if err != nil {
		return nil, &Failure{Status: 502, Message: "model discovery response could not be read"}
	}
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return nil, &Failure{Status: response.StatusCode, Message: publicError(raw, response.StatusCode, response.Status)}
	}
	var envelope struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, &Failure{Status: 502, Message: "model discovery response was not valid JSON"}
	}
	unique := make(map[string]struct{}, len(envelope.Data))
	models := make([]string, 0, len(envelope.Data))
	for _, entry := range envelope.Data {
		name := strings.TrimSpace(entry.ID)
		if name == "" {
			continue
		}
		if _, seen := unique[name]; seen {
			continue
		}
		unique[name] = struct{}{}
		models = append(models, name)
	}
	sort.Strings(models)
	return models, nil
}

// Text extracts assistant text from any supported non-streaming response.
func Text(body map[string]any) string {
	if choices, ok := body["choices"].([]any); ok && len(choices) > 0 {
		if choice, ok := choices[0].(map[string]any); ok {
			if message, ok := choice["message"].(map[string]any); ok {
				if content, ok := message["content"].(string); ok {
					return content
				}
			}
		}
	}
	if text, ok := body["output_text"].(string); ok {
		return text
	}
	if output, ok := body["output"].([]any); ok && len(output) > 0 {
		if first, ok := output[0].(map[string]any); ok {
			if content, ok := first["content"].([]any); ok && len(content) > 0 {
				if block, ok := content[0].(map[string]any); ok {
					if text, ok := block["text"].(string); ok {
						return text
					}
				}
			}
		}
	}
	if content, ok := body["content"].([]any); ok && len(content) > 0 {
		if block, ok := content[0].(map[string]any); ok {
			if text, ok := block["text"].(string); ok {
				return text
			}
		}
	}
	return ""
}

func scanner(response *http.Response) *bufio.Scanner {
	streamScanner := bufio.NewScanner(response.Body)
	streamScanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	return streamScanner
}
