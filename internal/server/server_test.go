package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/omniapi/omni-api/internal/model"
	"github.com/omniapi/omni-api/internal/secret"
	"github.com/omniapi/omni-api/internal/store"
	"github.com/omniapi/omni-api/internal/upstream"
)

func newTestServer(t *testing.T, config model.Config) (*Server, *store.Store) {
	t.Helper()
	directory := t.TempDir()
	codec, err := secret.NewAESCodec(directory, "test-passphrase")
	if err != nil {
		t.Fatalf("codec setup failed: %v", err)
	}
	configStore := store.New(filepath.Join(directory, "omni-api.db"), codec)
	if err := configStore.Replace(config); err != nil {
		t.Fatalf("config setup failed: %v", err)
	}
	return New(Options{Store: configStore}), configStore
}

func call(handler http.Handler, method, target, token, body string) *httptest.ResponseRecorder {
	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(body)
	}
	request := httptest.NewRequest(method, target, reader)
	if token != "" {
		request.Header.Set("authorization", "Bearer "+token)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func callWithHeader(handler http.Handler, method, target, header, token, body string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, target, strings.NewReader(body))
	request.Header.Set(header, token)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func TestModelSyncReusesSavedKeyAndRelaysUpstreamError(t *testing.T) {
	captured := http.Header{}
	upstreamServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		captured = request.Header.Clone()
		writer.Header().Set("content-type", "application/json")
		writer.WriteHeader(http.StatusForbidden)
		_, _ = writer.Write([]byte(`{"error":{"message":"Model listing is disabled"}}`))
	}))
	defer upstreamServer.Close()

	handler, _ := newTestServer(t, model.Config{
		Settings: model.Settings{AdminToken: "admin-token"},
		Providers: []model.Provider{{
			ID: "saved", Name: "Saved", BaseURL: upstreamServer.URL + "/v1",
			Protocol: model.ProtocolOpenAIResponses, APIKey: "stored-key", Enabled: true,
		}},
	})

	payload := `{"providerId":"saved","protocol":"openai-responses","baseUrl":"` + upstreamServer.URL + `/v1","apiKey":""}`
	response := call(handler, http.MethodPost, "/api/v1/providers/models:sync", "admin-token", payload)
	if response.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d (%s)", response.Code, response.Body.String())
	}
	if captured.Get("authorization") != "Bearer stored-key" {
		t.Fatalf("expected the stored key to be reused, got %q", captured.Get("authorization"))
	}
	if !strings.Contains(response.Body.String(), "Model listing is disabled") {
		t.Fatalf("expected the upstream message to be relayed, got %s", response.Body.String())
	}
}

func TestAdminEndpointsRequireTokenOnceConfigured(t *testing.T) {
	handler, _ := newTestServer(t, model.Config{Settings: model.Settings{AdminToken: "admin-token"}})
	if response := call(handler, http.MethodGet, "/api/v1/providers", "", ""); response.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without a token, got %d", response.Code)
	}
	if response := call(handler, http.MethodGet, "/api/v1/providers", "admin-token", ""); response.Code != http.StatusOK {
		t.Fatalf("expected 200 with the admin token, got %d", response.Code)
	}
}

func TestAdminEndpointsAreOpenBeforeBootstrap(t *testing.T) {
	handler, _ := newTestServer(t, model.Config{})
	response := call(handler, http.MethodGet, "/api/v1/settings", "", "")
	if response.Code != http.StatusOK {
		t.Fatalf("expected 200 during bootstrap, got %d", response.Code)
	}
	var settings model.PublicSettings
	if err := json.Unmarshal(response.Body.Bytes(), &settings); err != nil {
		t.Fatalf("settings response was not valid JSON: %v", err)
	}
	if settings.HasAdminToken {
		t.Fatalf("expected admin token to be reported as unset, got %+v", settings)
	}
}

func TestSettingsUpdateKeepsOmittedToken(t *testing.T) {
	handler, configStore := newTestServer(t, model.Config{
		Settings: model.Settings{AdminToken: "admin-token", ProxyToken: "proxy-token"},
	})
	response := call(handler, http.MethodPut, "/api/v1/settings", "admin-token", `{"adminToken":"rotated"}`)
	if response.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (%s)", response.Code, response.Body.String())
	}
	settings := configStore.Get().Settings
	if settings.AdminToken != "rotated" {
		t.Fatalf("expected the admin token to rotate, got %q", settings.AdminToken)
	}
	if settings.ProxyToken != "proxy-token" {
		t.Fatalf("expected the proxy token to survive, got %q", settings.ProxyToken)
	}
}

func TestAccessKeyLifecycleControlsProxyAuthorization(t *testing.T) {
	handler, _ := newTestServer(t, model.Config{Settings: model.Settings{AdminToken: "admin-token"}})
	created := call(handler, http.MethodPost, "/api/v1/access-keys", "admin-token", `{"name":"CI Pipeline"}`)
	if created.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d (%s)", created.Code, created.Body.String())
	}
	var accessKey model.AccessKey
	if err := json.Unmarshal(created.Body.Bytes(), &accessKey); err != nil {
		t.Fatalf("create response was not valid JSON: %v", err)
	}
	if !strings.HasPrefix(accessKey.Secret, "oa_") || accessKey.Prefix == "" {
		t.Fatalf("expected a generated secret and prefix, got %+v", accessKey)
	}
	if response := call(handler, http.MethodGet, "/v1/models", accessKey.Secret, ""); response.Code != http.StatusOK {
		t.Fatalf("expected generated key to authorize, got %d", response.Code)
	}

	listed := call(handler, http.MethodGet, "/api/v1/access-keys", "admin-token", "")
	var keys []model.AccessKey
	if err := json.Unmarshal(listed.Body.Bytes(), &keys); err != nil || len(keys) != 1 {
		t.Fatalf("unexpected key list: %s", listed.Body.String())
	}
	if keys[0].Secret != "" {
		t.Fatalf("expected list response to hide secret, got %q", keys[0].Secret)
	}
	masked := string([]rune(accessKey.Secret)[0]) + "••••••••" + string([]rune(accessKey.Secret)[len([]rune(accessKey.Secret))-1])
	if keys[0].Prefix != masked {
		t.Fatalf("expected list response to mask all but the first and last character, got %q", keys[0].Prefix)
	}
	viewed := call(handler, http.MethodGet, "/api/v1/access-keys/"+accessKey.ID, "admin-token", "")
	if viewed.Code != http.StatusOK {
		t.Fatalf("expected 200 viewing key, got %d (%s)", viewed.Code, viewed.Body.String())
	}
	var viewedAccessKey model.AccessKey
	if err := json.Unmarshal(viewed.Body.Bytes(), &viewedAccessKey); err != nil || viewedAccessKey.Secret != accessKey.Secret {
		t.Fatalf("expected detail response to include secret, got %s", viewed.Body.String())
	}
	if unauthorized := call(handler, http.MethodGet, "/api/v1/access-keys/"+accessKey.ID, "wrong-token", ""); unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 viewing key without admin access, got %d", unauthorized.Code)
	}

	updated := call(handler, http.MethodPut, "/api/v1/access-keys/"+accessKey.ID, "admin-token", `{"name":"CI Pipeline","enabled":false}`)
	if updated.Code != http.StatusNoContent {
		t.Fatalf("expected 204 disabling key, got %d", updated.Code)
	}
	if response := call(handler, http.MethodGet, "/v1/models", accessKey.Secret, ""); response.Code != http.StatusUnauthorized {
		t.Fatalf("expected disabled key to be rejected, got %d", response.Code)
	}

	deleted := call(handler, http.MethodDelete, "/api/v1/access-keys/"+accessKey.ID, "admin-token", "")
	if deleted.Code != http.StatusNoContent {
		t.Fatalf("expected 204 deleting key, got %d", deleted.Code)
	}
}

func TestCreateAccessKeyAcceptsManualSecretAndRejectsDuplicate(t *testing.T) {
	handler, _ := newTestServer(t, model.Config{Settings: model.Settings{AdminToken: "admin-token"}})
	created := call(handler, http.MethodPost, "/api/v1/access-keys", "admin-token", `{"name":"Manual","secret":"my-custom-secret"}`)
	if created.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d (%s)", created.Code, created.Body.String())
	}
	var accessKey model.AccessKey
	if err := json.Unmarshal(created.Body.Bytes(), &accessKey); err != nil {
		t.Fatalf("create response was not valid JSON: %v", err)
	}
	if accessKey.Secret != "my-custom-secret" || accessKey.Prefix != "my-custom-" {
		t.Fatalf("expected manual secret to be preserved, got %+v", accessKey)
	}
	if response := call(handler, http.MethodGet, "/v1/models", "my-custom-secret", ""); response.Code != http.StatusOK {
		t.Fatalf("expected manual key to authorize, got %d", response.Code)
	}
	duplicate := call(handler, http.MethodPost, "/api/v1/access-keys", "admin-token", `{"name":"Duplicate","secret":"my-custom-secret"}`)
	if duplicate.Code != http.StatusConflict {
		t.Fatalf("expected 409 for duplicate secret, got %d (%s)", duplicate.Code, duplicate.Body.String())
	}
}

func TestProviderUpdateKeepsStoredKeyAndHidesIt(t *testing.T) {
	handler, configStore := newTestServer(t, model.Config{
		Settings: model.Settings{AdminToken: "admin-token"},
		Providers: []model.Provider{{
			ID: "openai", Name: "OpenAI", BaseURL: "https://api.example.test/v1",
			Protocol: model.ProtocolOpenAIChat, APIKey: "stored-key", Enabled: true,
			Models: []model.ProviderModel{{ID: "m1", UpstreamModel: "gpt-test"}},
		}},
	})
	payload := `{"id":"openai","name":"OpenAI Renamed","baseUrl":"https://api.example.test/v1","protocol":"openai-chat","enabled":true,"models":[{"id":"m1","upstreamModel":"gpt-test"}]}`
	response := call(handler, http.MethodPut, "/api/v1/providers/openai", "admin-token", payload)
	if response.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (%s)", response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), "stored-key") {
		t.Fatalf("the admin API must never return an api key: %s", response.Body.String())
	}
	stored := configStore.Get().Providers
	if len(stored) != 1 || stored[0].APIKey != "stored-key" || stored[0].Name != "OpenAI Renamed" {
		t.Fatalf("expected the key preserved and the name updated, got %+v", stored)
	}
}

func TestProviderUpdatePersistsTrimmedModelAlias(t *testing.T) {
	handler, configStore := newTestServer(t, model.Config{
		Settings: model.Settings{AdminToken: "admin-token"},
		Providers: []model.Provider{{
			ID: "openai", Name: "OpenAI", BaseURL: "https://api.example.test/v1",
			Protocol: model.ProtocolOpenAIChat, APIKey: "stored-key", Enabled: true,
			Models: []model.ProviderModel{{ID: "m1", UpstreamModel: "gpt-test"}},
		}},
	})
	payload := `{"id":"openai","name":"OpenAI","baseUrl":"https://api.example.test/v1","protocol":"openai-chat","enabled":true,"models":[{"id":"m1","upstreamModel":" gpt-test ","alias":"  fast  "}]}`
	response := call(handler, http.MethodPut, "/api/v1/providers/openai", "admin-token", payload)
	if response.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (%s)", response.Code, response.Body.String())
	}
	stored := configStore.Get().Providers[0].Models
	if len(stored) != 1 || stored[0].Alias != "fast" || stored[0].UpstreamModel != "gpt-test" {
		t.Fatalf("expected a trimmed alias and upstream model, got %+v", stored)
	}
	if !strings.Contains(response.Body.String(), `"alias":"fast"`) {
		t.Fatalf("expected the alias returned to the console, got %s", response.Body.String())
	}
}

func TestProxyRequestAppearsInMonitoring(t *testing.T) {
	upstreamServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("content-type", "application/json")
		_, _ = writer.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"pong"}}],"usage":{"prompt_tokens":120,"completion_tokens":8,"prompt_tokens_details":{"cached_tokens":100},"cache_creation_input_tokens":12,"completion_tokens_details":{"reasoning_tokens":6}}}`))
	}))
	defer upstreamServer.Close()

	handler, _ := newTestServer(t, model.Config{
		Settings: model.Settings{AdminToken: "admin-token", ProxyToken: "proxy-token"},
		Providers: []model.Provider{{
			ID: "openai", Name: "OpenAI", BaseURL: upstreamServer.URL + "/v1",
			Protocol: model.ProtocolOpenAIChat, Enabled: true,
			Models: []model.ProviderModel{{ID: "model", UpstreamModel: "gpt-upstream", Alias: "gpt-public"}},
		}},
	})
	proxied := call(handler, http.MethodPost, "/v1/chat/completions", "proxy-token", `{"model":"gpt-public","messages":[]}`)
	if proxied.Code != http.StatusOK {
		t.Fatalf("expected proxy success, got %d (%s)", proxied.Code, proxied.Body.String())
	}
	monitored := call(handler, http.MethodGet, "/api/v1/request-logs", "admin-token", "")
	if monitored.Code != http.StatusOK {
		t.Fatalf("expected monitoring success, got %d (%s)", monitored.Code, monitored.Body.String())
	}
	var payload struct {
		Data  []model.RequestLog `json:"data"`
		Total int                `json:"total"`
	}
	if err := json.Unmarshal(monitored.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode monitoring response failed: %v", err)
	}
	if payload.Total != 1 || len(payload.Data) != 1 {
		t.Fatalf("expected one request log, got %#v", payload)
	}
	entry := payload.Data[0]
	if entry.ExposedModel != "gpt-public" || entry.UpstreamModel != "gpt-upstream" || entry.ProviderName != "OpenAI" || entry.Status != 200 {
		t.Fatalf("unexpected request log: %#v", entry)
	}
	if entry.Usage.InputTokens != 120 || entry.Usage.OutputTokens != 8 || entry.Usage.CachedTokens != 100 || entry.Usage.CacheCreationTokens != 12 || entry.Usage.ReasoningTokens != 6 {
		t.Fatalf("unexpected usage: %#v", entry.Usage)
	}
}

func TestRequestLogFiltersRejectInvalidTime(t *testing.T) {
	handler, _ := newTestServer(t, model.Config{Settings: model.Settings{AdminToken: "admin-token"}})
	response := call(handler, http.MethodGet, "/api/v1/request-logs?startedAfter=invalid", "admin-token", "")
	if response.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid monitoring time, got %d (%s)", response.Code, response.Body.String())
	}
}

func TestEncodedResponsesIncludeProtocolUsage(t *testing.T) {
	result := &upstream.Result{Usage: model.Usage{
		InputTokens: 120, OutputTokens: 8, CachedTokens: 100,
		CacheCreationTokens: 12, ReasoningTokens: 6,
	}}
	request := model.Request{System: "Be concise", Tools: []any{}, Temperature: floatField(map[string]any{"value": 0.5}, "value")}
	startedAt := time.Unix(1234567890, 0)

	responses, err := json.Marshal(encode(kindResponses, "test-model", result, "request-id", startedAt, request))
	if err != nil {
		t.Fatalf("encode responses payload failed: %v", err)
	}
	for _, marker := range []string{`"id":"resp_request-id"`, `"created_at":1234567890`, `"status":"completed"`, `"id":"resp_request-id_msg"`, `"annotations":[]`, `"input_tokens":120`, `"output_tokens":8`, `"reasoning_tokens":6`, `"instructions":"Be concise"`, `"temperature":0.5`, `"error":null`} {
		if !strings.Contains(string(responses), marker) {
			t.Fatalf("expected %s in Responses payload, got %s", marker, responses)
		}
	}

	messages, err := json.Marshal(encode(kindMessages, "test-model", result, "request-id", startedAt, request))
	if err != nil {
		t.Fatalf("encode Messages payload failed: %v", err)
	}
	for _, marker := range []string{`"input_tokens":120`, `"cache_creation_input_tokens":12`, `"cache_read_input_tokens":100`} {
		if !strings.Contains(string(messages), marker) {
			t.Fatalf("expected %s in Messages payload, got %s", marker, messages)
		}
	}

	chat, err := json.Marshal(encode(kindChat, "test-model", result, "request-id", startedAt, request))
	if err != nil {
		t.Fatalf("encode Chat payload failed: %v", err)
	}
	for _, marker := range []string{`"prompt_tokens":120`, `"completion_tokens":8`, `"cached_tokens":100`} {
		if !strings.Contains(string(chat), marker) {
			t.Fatalf("expected %s in Chat payload, got %s", marker, chat)
		}
	}
}

func TestNormalizeResponsesTextInputAndInstructions(t *testing.T) {
	request := normalize(kindResponses, map[string]any{
		"model": "test-model", "instructions": "Be concise", "input": "Hello",
	})
	if request.System != "Be concise" || len(request.Messages) != 1 || request.Messages[0].Content != "Hello" {
		t.Fatalf("unexpected normalized Responses request: %#v", request)
	}
}

func TestProxyRoutesRequireProxyTokenAndResolveExposedModel(t *testing.T) {
	upstreamServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("content-type", "application/json")
		_, _ = writer.Write([]byte(`{"id":"msg_test","type":"message","role":"assistant","content":[{"type":"text","text":"pong"}],"stop_reason":"end_turn"}`))
	}))
	defer upstreamServer.Close()

	handler, _ := newTestServer(t, model.Config{
		Settings: model.Settings{AdminToken: "admin-token", ProxyToken: "proxy-token"},
		Providers: []model.Provider{{
			ID: "anthropic", Name: "Anthropic", BaseURL: upstreamServer.URL + "/v1",
			Protocol: model.ProtocolAnthropic, APIKey: "secret", Enabled: true,
			Models: []model.ProviderModel{{ID: "claude", UpstreamModel: "claude-test", Alias: "chat"}},
		}},
	})

	if response := call(handler, http.MethodPost, "/v1/chat/completions", "", `{"model":"chat","messages":[]}`); response.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without the proxy token, got %d", response.Code)
	}
	for _, header := range []string{"x-api-key", "api-key"} {
		response := callWithHeader(handler, http.MethodPost, "/v1/messages", header, "proxy-token", `{"model":"chat","messages":[]}`)
		if response.Code != http.StatusOK {
			t.Fatalf("%s expected 200, got %d (%s)", header, response.Code, response.Body.String())
		}
	}

	missing := call(handler, http.MethodPost, "/v1/chat/completions", "proxy-token", `{"model":"nope","messages":[]}`)
	if missing.Code != http.StatusNotFound || !strings.Contains(missing.Body.String(), "model_not_found") {
		t.Fatalf("expected model_not_found, got %d (%s)", missing.Code, missing.Body.String())
	}

	body := `{"model":"chat","messages":[{"role":"user","content":"ping"}]}`
	for path, marker := range map[string]string{
		"/v1/chat/completions": `"object":"chat.completion"`,
		"/v1/responses":        `"output_text":"pong"`,
		"/v1/messages":         `"type":"message"`,
	} {
		response := call(handler, http.MethodPost, path, "proxy-token", body)
		if response.Code != http.StatusOK {
			t.Fatalf("%s expected 200, got %d (%s)", path, response.Code, response.Body.String())
		}
		if !strings.Contains(response.Body.String(), marker) {
			t.Fatalf("%s expected %s in the response, got %s", path, marker, response.Body.String())
		}
		if response.Header().Get("x-request-id") == "" {
			t.Fatalf("%s expected an x-request-id header", path)
		}
	}
}

func TestAliasIsTheOnlyExposedModelIdentifier(t *testing.T) {
	upstreamServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("content-type", "application/json")
		_, _ = writer.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"pong"}}]}`))
	}))
	defer upstreamServer.Close()

	handler, _ := newTestServer(t, model.Config{
		Settings: model.Settings{ProxyToken: "proxy-token"},
		Providers: []model.Provider{{
			ID: "openai", Name: "OpenAI", BaseURL: upstreamServer.URL + "/v1",
			Protocol: model.ProtocolOpenAIChat, Enabled: true,
			Models: []model.ProviderModel{
				{ID: "aliased", UpstreamModel: "gpt-hidden", Alias: "public-alias"},
				{ID: "plain", UpstreamModel: "gpt-plain"},
			},
		}},
	})

	alias := call(handler, http.MethodPost, "/v1/chat/completions", "proxy-token", `{"model":"public-alias","messages":[]}`)
	if alias.Code != http.StatusOK {
		t.Fatalf("expected alias request to succeed, got %d (%s)", alias.Code, alias.Body.String())
	}
	original := call(handler, http.MethodPost, "/v1/chat/completions", "proxy-token", `{"model":"gpt-hidden","messages":[]}`)
	if original.Code != http.StatusNotFound {
		t.Fatalf("expected the aliased upstream model to be hidden, got %d (%s)", original.Code, original.Body.String())
	}
	plain := call(handler, http.MethodPost, "/v1/chat/completions", "proxy-token", `{"model":"gpt-plain","messages":[]}`)
	if plain.Code != http.StatusOK {
		t.Fatalf("expected the unaliased upstream model to succeed, got %d (%s)", plain.Code, plain.Body.String())
	}
}

func TestModelListReturnsOnlyExposedIdentifiers(t *testing.T) {
	handler, _ := newTestServer(t, model.Config{
		Settings: model.Settings{ProxyToken: "proxy-token"},
		Providers: []model.Provider{
			{ID: "enabled", Name: "Enabled", BaseURL: "https://enabled.test/v1", Protocol: model.ProtocolOpenAIChat, Enabled: true, Models: []model.ProviderModel{
				{ID: "aliased", UpstreamModel: "hidden-id", Alias: "public-alias"},
				{ID: "plain", UpstreamModel: "plain-id"},
			}},
			{ID: "mirror", Name: "Mirror", BaseURL: "https://mirror.test/v1", Protocol: model.ProtocolOpenAIChat, Enabled: true, Models: []model.ProviderModel{
				{ID: "duplicate", UpstreamModel: "other", Alias: "public-alias"},
			}},
			{ID: "disabled", Name: "Disabled", BaseURL: "https://disabled.test/v1", Protocol: model.ProtocolOpenAIChat, Enabled: false, Models: []model.ProviderModel{
				{ID: "offline", UpstreamModel: "offline-id"},
			}},
		},
	})

	unauthorized := call(handler, http.MethodGet, "/v1/models", "", "")
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without proxy token, got %d", unauthorized.Code)
	}
	response := call(handler, http.MethodGet, "/v1/models", "proxy-token", "")
	if response.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (%s)", response.Code, response.Body.String())
	}
	payload := response.Body.String()
	for _, exposed := range []string{"public-alias", "plain-id"} {
		if !strings.Contains(payload, `"id":"`+exposed+`"`) {
			t.Fatalf("expected %q in model list, got %s", exposed, payload)
		}
	}
	if strings.Contains(payload, `"id":"hidden-id"`) {
		t.Fatalf("aliased model id must not be exposed: %s", payload)
	}
	if strings.Contains(payload, `"id":"offline-id"`) {
		t.Fatalf("disabled providers must not expose models: %s", payload)
	}
	if strings.Count(payload, `"id":"public-alias"`) != 1 {
		t.Fatalf("expected the shared alias listed once, got %s", payload)
	}
	if strings.Index(payload, `"id":"plain-id"`) > strings.Index(payload, `"id":"public-alias"`) {
		t.Fatalf("expected the model list sorted ascending, got %s", payload)
	}
}

func TestProxyReportsAllProvidersFailed(t *testing.T) {
	upstreamServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("content-type", "application/json")
		writer.WriteHeader(http.StatusTooManyRequests)
		_, _ = writer.Write([]byte(`{"error":{"message":"Rate limited"}}`))
	}))
	defer upstreamServer.Close()

	handler, _ := newTestServer(t, model.Config{
		Settings: model.Settings{ProxyToken: "proxy-token"},
		Providers: []model.Provider{{
			ID: "openai", Name: "OpenAI", BaseURL: upstreamServer.URL + "/v1",
			Protocol: model.ProtocolOpenAIChat, APIKey: "secret", Enabled: true,
			Models: []model.ProviderModel{{ID: "m1", UpstreamModel: "gpt-test", Alias: "chat"}},
		}},
	})

	response := call(handler, http.MethodPost, "/v1/chat/completions", "proxy-token", `{"model":"chat","messages":[]}`)
	if response.Code != http.StatusTooManyRequests {
		t.Fatalf("expected the last upstream status 429, got %d (%s)", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "all_providers_failed") || !strings.Contains(response.Body.String(), "Rate limited") {
		t.Fatalf("expected all_providers_failed with the upstream message, got %s", response.Body.String())
	}
}

func TestStreamingReencodesToInboundProtocol(t *testing.T) {
	upstreamServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("content-type", "text/event-stream")
		_, _ = writer.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n"))
	}))
	defer upstreamServer.Close()

	handler, _ := newTestServer(t, model.Config{
		Settings: model.Settings{ProxyToken: "proxy-token"},
		Providers: []model.Provider{{
			ID: "openai", Name: "OpenAI", BaseURL: upstreamServer.URL + "/v1",
			Protocol: model.ProtocolOpenAIChat, APIKey: "secret", Enabled: true,
			Models: []model.ProviderModel{{ID: "m1", UpstreamModel: "gpt-test", Alias: "chat"}},
		}},
	})

	response := call(handler, http.MethodPost, "/v1/messages", "proxy-token", `{"model":"chat","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if response.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", response.Code)
	}
	if got := response.Header().Get("content-type"); got != "text/event-stream; charset=utf-8" {
		t.Fatalf("expected an SSE content type, got %q", got)
	}
	payload := response.Body.String()
	for _, marker := range []string{"event: message_start", "event: content_block_delta", "\"text\":\"hi\"", "event: message_stop"} {
		if !strings.Contains(payload, marker) {
			t.Fatalf("expected %q in the stream, got %s", marker, payload)
		}
	}
}

func TestStreamingResponsesCompletionIncludesUsage(t *testing.T) {
	upstreamServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("content-type", "text/event-stream")
		_, _ = writer.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: {\"usage\":{\"prompt_tokens\":12,\"completion_tokens\":3}}\n\ndata: [DONE]\n\n"))
	}))
	defer upstreamServer.Close()

	handler, _ := newTestServer(t, model.Config{
		Settings: model.Settings{ProxyToken: "proxy-token"},
		Providers: []model.Provider{{
			ID: "openai", Name: "OpenAI", BaseURL: upstreamServer.URL + "/v1",
			Protocol: model.ProtocolOpenAIChat, APIKey: "secret", Enabled: true,
			Models: []model.ProviderModel{{ID: "m1", UpstreamModel: "gpt-test", Alias: "chat"}},
		}},
	})

	response := call(handler, http.MethodPost, "/v1/responses", "proxy-token", `{"model":"chat","stream":true,"input":"hi"}`)
	if response.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", response.Code)
	}
	payload := response.Body.String()
	for _, marker := range []string{
		"event: response.created", "event: response.in_progress", "event: response.output_item.added", "event: response.content_part.added",
		"event: response.output_text.delta", "event: response.output_text.done", "event: response.content_part.done",
		"event: response.output_item.done", "event: response.completed", `"sequence_number":`,
		`"input_tokens":12`, `"output_tokens":3`, `"total_tokens":15`, `"output_text":"hi"`,
	} {
		if !strings.Contains(payload, marker) {
			t.Fatalf("expected %q in the stream, got %s", marker, payload)
		}
	}
}
