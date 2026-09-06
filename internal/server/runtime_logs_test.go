package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/omniapi/omni-api/internal/model"
)

func TestRuntimeLogsRemainAvailableDuringPendingRequest(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	finish := func() { once.Do(func() { close(release) }) }
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		select {
		case <-release:
		case <-r.Context().Done():
		}
		fmt.Fprint(w, `{"choices":[{"message":{"content":"ok"}}]}`)
	}))
	defer up.Close()
	defer finish()
	handler, _ := newTestServer(t, model.Config{
		Settings: model.Settings{AdminToken: "admin-token", ProxyToken: "proxy-token"},
		Providers: []model.Provider{{ID: "p", Name: "Demo", BaseURL: up.URL, Protocol: model.ProtocolOpenAIChat,
			Enabled: true, Models: []model.ProviderModel{{ID: "m", UpstreamModel: "demo"}}}},
	})
	if response := call(handler, "GET", "/api/v1/runtime-logs", "", ""); response.Code != 401 {
		t.Fatalf("runtime logs must require admin authentication, got %d", response.Code)
	}
	completed := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		completed <- call(handler, "POST", "/v1/chat/completions", "proxy-token", `{"model":"demo","messages":[{"role":"user","content":"private prompt"}]}`)
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("upstream was not called")
	}
	response := call(handler, "GET", "/api/v1/runtime-logs", "admin-token", "")
	text := response.Body.String()
	if !strings.Contains(text, "收到外部请求") || !strings.Contains(text, "开始上游转发") || strings.Contains(text, "请求完成") {
		t.Fatalf("expected pending request stages, got %s", text)
	}
	if strings.Contains(text, "private prompt") || strings.Contains(text, "proxy-token") {
		t.Fatal("request secrets leaked")
	}
	finish()
	select {
	case result := <-completed:
		if result.Code != 200 {
			t.Fatalf("unexpected response: %s", result.Body.String())
		}
	case <-time.After(3 * time.Second):
		t.Fatal("request did not finish")
	}
	response = call(handler, "GET", "/api/v1/runtime-logs", "admin-token", "")
	if !strings.Contains(response.Body.String(), "请求完成") {
		t.Fatal("missing completion log")
	}
}

func TestRuntimeLogsRedactSecretsAndBoundHistory(t *testing.T) {
	handler, _ := newTestServer(t, model.Config{
		Settings:   model.Settings{AdminToken: "admin-secret", ProxyToken: "proxy-secret"},
		AccessKeys: []model.AccessKey{{ID: "key", Secret: "access-secret"}},
		Providers:  []model.Provider{{ID: "p", APIKey: "provider-secret"}},
	})
	for index := 0; index < runtimeLogCapacity+5; index++ {
		handler.LogRuntime("error", "request-1", "diagnostic", "admin-secret proxy-secret access-secret provider-secret")
	}
	response := call(handler, "GET", "/api/v1/runtime-logs", "admin-secret", "")
	for _, secret := range []string{"admin-secret", "proxy-secret", "access-secret", "provider-secret"} {
		if strings.Contains(response.Body.String(), secret) {
			t.Fatalf("leaked %s", secret)
		}
	}
	var result struct {
		Data []runtimeLog `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Data) != runtimeLogCapacity {
		t.Fatalf("unexpected history size %d", len(result.Data))
	}
	for index := 1; index < len(result.Data); index++ {
		if result.Data[index].ID != result.Data[index-1].ID+1 {
			t.Fatal("logs are out of order")
		}
	}
	detail := targetDetail(model.Target{Provider: model.Provider{BaseURL: "https://user:password@example.test/v1?api_key=hidden"}}, 1)
	if strings.Contains(detail, "password") || strings.Contains(detail, "hidden") {
		t.Fatal("URL credentials leaked")
	}
}

func TestRuntimeLogsRecordFailoverAndStreamFailure(t *testing.T) {
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(503)
		fmt.Fprint(w, `{"error":{"message":"capacity exhausted"}}`)
	}))
	defer bad.Close()
	stream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\ndata: {\"error\":{\"message\":\"stream quota exhausted\"}}\n\n")
	}))
	defer stream.Close()
	for _, inbound := range []string{"/v1/chat/completions", "/v1/responses", "/v1/messages"} {
		t.Run(inbound, func(t *testing.T) {
			handler, _ := newTestServer(t, model.Config{
				Settings: model.Settings{ProxyToken: "proxy-token"},
				Providers: []model.Provider{
					{ID: "bad", Name: "Bad", Enabled: true, BaseURL: bad.URL, Protocol: model.ProtocolOpenAIChat, Models: []model.ProviderModel{{ID: "m", UpstreamModel: "demo"}}},
					{ID: "stream", Name: "Stream", Enabled: true, BaseURL: stream.URL, Protocol: model.ProtocolOpenAIChat, Models: []model.ProviderModel{{ID: "m", UpstreamModel: "demo"}}},
				},
			})
			response := call(handler, "POST", inbound, "proxy-token", `{"model":"demo","stream":true,"messages":[{"role":"user","content":"hello"}],"input":"hello"}`)
			body := response.Body.String()
			if !strings.Contains(body, "upstream_stream_error") {
				t.Fatalf("missing stream error: %s", body)
			}
			for _, success := range []string{"data: [DONE]", "event: response.completed", "event: message_stop"} {
				if strings.Contains(body, success) {
					t.Fatalf("failed stream reported success: %s", body)
				}
			}
			logs := call(handler, "GET", "/api/v1/runtime-logs", "", "").Body.String()
			for _, stage := range []string{"capacity exhausted", "准备切换下一个上游", "开始向客户端发送流式响应", "stream quota exhausted", "请求失败"} {
				if !strings.Contains(logs, stage) {
					t.Fatalf("missing %s in %s", stage, logs)
				}
			}
		})
	}
}
